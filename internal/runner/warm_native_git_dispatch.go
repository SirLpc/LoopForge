package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

const warmGitMessageLimit = 20 << 20

type warmWindowsHelperMessage struct {
	Git    *warmWindowsHelperRequest       `json:"git,omitempty"`
	Result *domain.RunnerWorkspaceResponse `json:"result,omitempty"`
}

type warmWindowsHelperRequest struct {
	Dir      string
	Args     []string
	Input    []byte
	Deadline time.Time
}

type warmWindowsHelperReply struct {
	Handled     bool
	Output      []byte
	Error       string
	Code        int
	IndexLocked bool `json:",omitempty"`
}

type warmWindowsGitExit struct {
	message string
	code    int
}

func (e warmWindowsGitExit) Error() string { return e.message }
func (e warmWindowsGitExit) ExitCode() int { return e.code }

func warmWindowsHelperContext(ctx context.Context, decoder *json.Decoder, encoder *json.Encoder) context.Context {
	return gitwork.WithLocalGit(ctx, func(ctx context.Context, dir string, input io.Reader, args []string) ([]byte, error, bool) {
		if warmWindowsGitArgs(args) == nil {
			return nil, nil, false
		}
		if err := ctx.Err(); err != nil {
			return nil, err, true
		}
		request := &warmWindowsHelperRequest{Dir: dir, Args: args}
		request.Deadline, _ = ctx.Deadline()
		if input != nil {
			data, err := io.ReadAll(io.LimitReader(input, warmGitMessageLimit+1))
			if err != nil {
				return nil, err, true
			}
			if len(data) > warmGitMessageLimit {
				return nil, errors.New("Windows Git input exceeds limit"), true
			}
			request.Input = data
		}
		if err := encoder.Encode(warmWindowsHelperMessage{Git: request}); err != nil {
			return nil, err, true
		}
		var reply warmWindowsHelperReply
		if err := decoder.Decode(&reply); err != nil {
			return nil, err, true
		}
		if reply.IndexLocked {
			return nil, errWarmIndexLocked, true
		}
		if reply.Error != "" {
			if strings.Contains(reply.Error, "loopforge-windows-git-index-copy-failed") {
				return nil, fmt.Errorf("%w: %s", gitwork.ErrDiffIndexSnapshot, reply.Error), true
			}
			return reply.Output, warmWindowsGitExit{message: reply.Error, code: reply.Code}, true
		}
		if !reply.Handled && input != nil {
			out, err := gitwork.LocalGit(context.Background(), dir, bytes.NewReader(request.Input), args...)
			return out, err, true
		}
		return reply.Output, nil, reply.Handled
	})
}

func (d *Daemon) warmWindowsGitTargets(ctx context.Context, task domain.RunnerTask, workspace string) map[string]warmWindowsTree {
	lease, pool := task.WarmWorkspaceLease, d.warmWorkspacePool
	if lease == nil || pool == nil || !pool.native || workspace != filepath.Join(pool.workspaceRoot, "runs", lease.OwnerRunID, "workspace") {
		return nil
	}
	paths := make(map[string]string)
	pool.mu.Lock()
	a, err := pool.loadLease(ctx, *lease)
	if err == nil && a.Phase == warmWorkspacePlaced && a.GitReady && !a.ReturnProven {
		for id, path := range a.Targets {
			if rel, err := filepath.Rel(workspace, path); err == nil && validWarmWorkspaceRelativePath(filepath.ToSlash(rel)) {
				paths[path] = pool.slots[id].Path
			}
		}
	}
	pool.mu.Unlock()
	targets := make(map[string]warmWindowsTree)
	for path, original := range paths {
		if w, err := selectNativeWarmWindowsGit(ctx, path, os.TempDir(), original); err == nil && w.root != "" {
			targets[path] = w
		}
	}
	return targets
}

func warmWindowsGitExchange(ctx context.Context, command warmGitCommand, targets map[string]warmWindowsTree) func(io.Reader, io.Writer) ([]byte, error) {
	return func(stdout io.Reader, stdin io.Writer) ([]byte, error) {
		decoder, encoder := json.NewDecoder(stdout), json.NewEncoder(stdin)
		if err := encoder.Encode(command); err != nil {
			return nil, err
		}
		for {
			var message warmWindowsHelperMessage
			if err := decoder.Decode(&message); err != nil {
				return nil, err
			}
			if message.Result != nil && message.Git == nil {
				return json.Marshal(message.Result)
			}
			if message.Git == nil || message.Result != nil {
				return nil, errors.New("invalid Windows Git helper message")
			}
			reply := runWarmWindowsHelperRequest(ctx, targets, command, *message.Git)
			if err := encoder.Encode(reply); err != nil {
				return nil, err
			}
		}
	}
}

func runWarmWindowsHelperRequest(ctx context.Context, targets map[string]warmWindowsTree, command warmGitCommand, request warmWindowsHelperRequest) warmWindowsHelperReply {
	w, ok := targets[request.Dir]
	args := warmWindowsGitArgs(request.Args)
	verb := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		verb = args[i]
		break
	}
	movesWorktree := verb == "checkout" || verb == "merge" || verb == "rebase"
	mutates := movesWorktree || verb == "add" || verb == "commit" || verb == "update-ref" || verb == "rm"
	if mutates && command.Action == "" && (verb != "add" || command.Outputs == nil || !command.Outputs.RecoverOutputs || !slices.Contains(request.Args, "-N")) {
		return warmWindowsHelperReply{}
	}
	if !ok || args == nil {
		return warmWindowsHelperReply{}
	}
	if !request.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, request.Deadline)
		defer cancel()
	}
	if movesWorktree {
		plan, ok := planWarmWindowsWorktree(ctx, w.checkout, verb, request.Args[len(request.Args)-1])
		if !ok {
			return warmWindowsHelperReply{}
		}
		if len(request.Input) != 0 {
			return warmWindowsHelperReply{Handled: true, Error: "unexpected Windows Git input", Code: -1}
		}
		spec := warmWindowsRequest{ManagedGit: true, PinConfig: true, FinishWrite: true, ChangedPaths: plan.paths, PinChangedParents: true}
		spec.Guard = func() error {
			if !plan.stillCurrent(ctx, w.checkout) {
				return errWindowsWarmGitUnavailable
			}
			return nil
		}
		if verb != "checkout" {
			args = slices.Clone(args)
			args[len(args)-1] = plan.tip
		}
		if verb == "rebase" {
			// Rebase's autostash can touch files outside the commits' path
			// set. Require a clean tracked tree and let Git reject later edits.
			status := executeWarmWindowsDispatch(ctx, w, []string{"status", "--porcelain=v1", "--untracked-files=no"},
				warmWindowsRequest{ManagedGit: true, PinConfig: true}, "rebase-status")
			if !status.Handled || status.Error != "" || len(bytes.TrimSpace(status.Output)) != 0 {
				return warmWindowsHelperReply{}
			}
			args[slices.Index(args, "--autostash")] = "--no-autostash"
		}
		if plan.lfs {
			spec.LFSScratch = rand.Text()
		}
		return executeWarmWindowsDispatch(ctx, w, args, spec, verb)
	}
	spec := warmWindowsRequest{ManagedGit: true, GitInput: request.Input, PinConfig: true}
	if len(request.Input) != 0 || verb == "add" && slices.Contains(request.Args, "-N") {
		if verb != "add" || !slices.Contains(request.Args, "-N") || len(request.Input) > warmGitMessageLimit {
			return warmWindowsHelperReply{Handled: true, Error: "invalid Windows Git input", Code: -1}
		}
		for path := range strings.SplitSeq(strings.TrimSuffix(string(request.Input), "\x00"), "\x00") {
			if !warmWindowsProofPath(path) || gitwork.Excluded(path) {
				return warmWindowsHelperReply{Handled: true, Error: "invalid intent-add path", Code: -1}
			}
			spec.ChangedPaths = append(spec.ChangedPaths, path)
		}
		args = append([]string{"--literal-pathspecs"}, args...)
	}
	// Intent-to-add and worktree reads need LFS filters but must not publish
	// objects. Real staging can route to Windows only with a persistent LFS store.
	if verb == "diff" || verb == "status" || verb == "add" {
		spec.LFSScratch = rand.Text()
	}
	spec.CopyIndex = verb == "diff"
	if verb == "add" && !slices.Contains(request.Args, "-N") {
		return stageWarmWindowsGit(ctx, w, args, spec, verb)
	}
	if mutates {
		spec.FinishWrite = true
	}
	if verb == "rm" {
		spec.ChangedPaths = []string{request.Args[len(request.Args)-1]}
	}
	return executeWarmWindowsDispatch(ctx, w, args, spec, verb)
}

func executeWarmWindowsDispatch(ctx context.Context, w warmWindowsTree, args []string, spec warmWindowsRequest, verb string) warmWindowsHelperReply {
	if w.eol != "" {
		args = append([]string{"-c", "core.eol=" + w.eol}, args...)
	}
	start := time.Now()
	output, err := w.runGit(ctx, w.root+`\.git`, spec, args)
	recordWarmGitDuration(ctx, verb, "windows", start, err)
	if errors.Is(err, errWarmIndexLocked) {
		return warmWindowsHelperReply{Handled: true, IndexLocked: true}
	}
	if errors.Is(err, errWindowsWarmGitUnavailable) {
		return warmWindowsHelperReply{}
	}
	reply := warmWindowsHelperReply{Handled: true, Code: -1}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			reply.Code = exit.ExitCode()
		}
		reply.Error = err.Error()
		return reply
	}
	reply.Output = []byte(output)
	return reply
}

// Only Runner-generated repository-local operations may cross to Windows.
// Network, external diff drivers, option injection and alternate worktrees
// remain in the existing isolated Git process.
func warmWindowsGitArgs(input []string) []string {
	args := input
	if len(args) > 0 && args[0] == "--no-optional-locks" {
		args = args[1:]
	}
	if len(args) >= 3 && args[0] == "-c" && args[1] == "diff.autoRefreshIndex=true" && args[2] == "diff" {
		args = args[2:]
	}
	identity := []string{"-c", "user.name=LoopForge", "-c", "user.email=loopforge@local"}
	var prefix []string
	if len(args) >= len(identity) && slices.Equal(args[:len(identity)], identity) {
		prefix, args = identity, args[len(identity):]
	}
	if len(args) == 0 {
		return nil
	}
	cmd, operands := args[0], args[1:]
	var allowed bool
	switch cmd {
	case "diff":
		allowed = warmWindowsGitOptions(operands, "--name-status", "--numstat", "--name-only", "--diff-filter=U", "--cached", "--quiet", "--no-ext-diff", "--no-color")
		if allowed {
			args = append([]string{"-c", "diff.autoRefreshIndex=true", "diff", "--no-ext-diff", "--no-textconv"}, operands...)
		}
	case "status":
		allowed = warmWindowsGitOptions(operands, "--porcelain", "--porcelain=v1", "-z", "--untracked-files=all")
	case "add":
		allowed = slices.Equal(operands, []string{"-N", "--pathspec-from-file=-", "--pathspec-file-nul"}) ||
			slices.Equal(operands, []string{"-A"}) ||
			len(operands) >= 3 && operands[0] == "-u" && operands[1] == "--" && warmWindowsGitOptions(operands[1:])
	case "ls-files":
		allowed = warmWindowsGitOptions(operands, "-z", "--others", "--exclude-standard", "--cached")
	case "show":
		allowed = len(operands) == 1 && strings.Contains(operands[0], ":") && !strings.HasPrefix(operands[0], "-")
	case "cat-file":
		allowed = len(operands) == 2 && operands[0] == "-e" && !strings.HasPrefix(operands[1], "-")
	case "log":
		allowed = len(operands) == 3 && operands[0] == "-n" && !strings.HasPrefix(operands[1], "-") && strings.HasPrefix(operands[2], "--format=")
	case "diff-tree":
		allowed = warmWindowsGitOptions(operands, "--no-commit-id", "--name-status", "-r", "--root", "--no-renames", "-z")
	case "for-each-ref":
		allowed = len(operands) == 2 && strings.HasPrefix(operands[0], "--format=") && strings.HasPrefix(operands[1], "refs/")
	case "rev-parse":
		allowed = warmWindowsGitOptions(operands, "--verify", "--abbrev-ref", "--end-of-options")
	case "rev-list":
		allowed = len(operands) == 2 && operands[0] == "--count" && !strings.HasPrefix(operands[1], "-")
	case "merge-base":
		allowed = len(operands) == 3 && operands[0] == "--is-ancestor" && !strings.HasPrefix(operands[1], "-") && !strings.HasPrefix(operands[2], "-")
	case "checkout":
		allowed = len(operands) == 1 && warmWindowsLocalRef(operands[0])
	case "merge":
		allowed = len(operands) == 3 && slices.Equal(operands[:2], []string{"--ff-only", "-q"}) && warmWindowsLocalRef(operands[2]) ||
			len(operands) == 4 && slices.Equal(operands[:2], []string{"--no-edit", "-m"}) && warmWindowsLocalRef(operands[3])
	case "rebase":
		allowed = len(operands) == 3 && slices.Equal(operands[:2], []string{"-q", "--autostash"}) && warmWindowsLocalRef(operands[2])
	case "branch":
		allowed = slices.Equal(operands, []string{"--show-current"})
	case "remote":
		allowed = slices.Equal(operands, []string{"get-url", "origin"})
	case "config":
		allowed = len(operands) == 1 && (operands[0] == "user.name" || operands[0] == "user.email") ||
			slices.Equal(operands, []string{"--local", "--no-includes", "--get-regexp", `^remote\..*\.url$`})
	case "commit":
		allowed = len(operands) == 4 && operands[0] == "--no-verify" && operands[1] == "-q" && operands[2] == "-m" ||
			len(operands) == 3 && operands[0] == "-m" && operands[2] == "--quiet"
		if allowed {
			args = append([]string{"commit", "--no-gpg-sign"}, operands...)
		}
	case "rm":
		allowed = len(operands) == 3 && operands[0] == "-f" && operands[1] == "--" && warmWindowsProofPath(operands[2]) ||
			len(operands) == 5 && slices.Equal(operands[:4], []string{"-f", "--cached", "--ignore-unmatch", "--"}) && warmWindowsProofPath(operands[4])
	case "update-ref":
		if len(operands) == 2 && strings.HasPrefix(operands[0], "refs/remotes/origin/") {
			_, allowed = runnerSafeGitObjectID(operands[1])
		}
	}
	if !allowed || len(prefix) != 0 && cmd != "commit" && cmd != "rebase" {
		return nil
	}
	return append(append([]string{"--no-pager"}, prefix...), args...)
}

func warmWindowsGitOptions(args []string, flags ...string) bool {
	for i, arg := range args {
		if arg == "--" {
			for _, path := range args[i+1:] {
				if path != "." && !slices.Contains(gitwork.ExcludePathspecs, path) && !warmWindowsProofPath(path) {
					return false
				}
			}
			return true
		}
		if strings.HasPrefix(arg, "-") && !slices.Contains(flags, arg) {
			return false
		}
	}
	return true
}

const maxWarmGitSessionBytes = 128 << 20

func verifyWarmWindowsDispatchConfig(ctx context.Context, checkout string) error {
	if err := verifyNativeWarmConfig(ctx, checkout, os.TempDir()); err != nil {
		return err
	}
	meta := filepath.Join(checkout, ".git")
	// Deferred includes and alternate stores would make Git consult paths
	// outside the registered repository after the Windows path check.
	for _, name := range []string{"objects/info/alternates", "objects/info/http-alternates", "commondir"} {
		if _, err := os.Lstat(filepath.Join(meta, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			return errors.New("Windows Git requires a self-contained object store")
		}
	}
	if _, err := os.Lstat(filepath.Join(meta, "config.worktree")); errors.Is(err, os.ErrNotExist) {
		value, _ := gitwork.C(ctx, os.TempDir(), "config", "--no-includes", "--file", filepath.Join(meta, "config"), "--type=bool", "--get", "extensions.worktreeConfig")
		if value == "true" {
			return errors.New("Windows Git requires an existing worktree configuration file")
		}
	}
	return nil
}
