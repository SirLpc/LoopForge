package runtime

import "path/filepath"

type nativeWarmCommandTool struct {
	binary, client string
}

// WithNativeWarmCommandTool attaches a task-only Runner client and its socket.
// Both paths come from local Runner state. The task sees a read-only mount.
func WithNativeWarmCommandTool(command CommandSpec, binary, client, envKey string) CommandSpec {
	command.nativeWarmTool = &nativeWarmCommandTool{binary: binary, client: client}
	env := map[string]string{envKey: client}
	command = WithCommandEnvironment(command, env)
	if command.Provider == "codex" {
		// Codex passes only its core environment to tool shells by default.
		command.Argv = withCodexIsolationEnv(command.Argv, env)
	}
	return command
}

func (tool *nativeWarmCommandTool) bind(args []string, isolation *NativeWarmIsolation) []string {
	if tool == nil || isolation == nil || len(args) == 0 || args[len(args)-1] != "--" {
		return args
	}
	// The task already has a private /tmp. Restore only this invocation's
	// tool directory and pin the binary at its entry point within it.
	dir := filepath.Dir(tool.client)
	return append(args[:len(args)-1], "--ro-bind", dir, dir, "--ro-bind", tool.binary, tool.client, "--")
}
