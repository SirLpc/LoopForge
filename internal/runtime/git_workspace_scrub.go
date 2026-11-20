package runtime

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// workspaceGitCredentialFiles hold credentials by nature. Git never writes
// them in a workspace turn (its helper list is reset), so any of them was
// written by the agent and may hold a token the platform handed out.
var workspaceGitCredentialFiles = []string{".git-credentials", ".netrc", "_netrc", ".config/gh", ".config/glab-cli", ".config/tea"}

var gitConfigUserinfoURL = regexp.MustCompile(`(?i)\bhttps?://[^/\s@]+@`)

// ScrubWorkspaceGitSecrets removes Git credentials an agent wrote by hand
// during a workspace turn: credential files in each HOME, and HTTPS userinfo
// or authorization headers in each HOME's Git config and in the config of
// every checkout below workspace (those configs travel with the workspace
// snapshot). It reports what it removed, without values.
func ScrubWorkspaceGitSecrets(ctx context.Context, homes []string, workspace string) []string {
	return scrubGitRoots(ctx, workspace, homes, "")
}

func scrubPrivateGitConfig(ctx context.Context, file string) []string {
	out, err := scrubGit(ctx, file, "--no-includes", "--null", "--list")
	if err != nil {
		return nil
	}
	var cleaned []string
	removed := map[string]bool{}
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		if ctx.Err() != nil {
			return nil
		}
		key, value, _ := strings.Cut(string(entry), "\n")
		if key == "" {
			continue
		}
		lower := strings.ToLower(key)
		section, _, _ := cutLast(key, ".")
		switch {
		case strings.HasPrefix(lower, "url.") && gitConfigUserinfoURL.MatchString(section):
			if removed[section] {
				continue
			}
			removed[section] = true
			if _, err := scrubGit(ctx, file, "--remove-section", section); err == nil {
				cleaned = append(cleaned, "url.<redacted>")
			}
		case strings.HasSuffix(lower, ".extraheader") && strings.Contains(strings.ToLower(value), "authorization"):
			if removed[key] {
				continue
			}
			removed[key] = true
			if _, err := scrubGit(ctx, file, "--unset-all", key); err == nil {
				cleaned = append(cleaned, key)
			}
		case gitConfigUserinfoURL.MatchString(value):
			if _, err := scrubGit(ctx, file, "--fixed-value", "--replace-all", key, stripURLUserinfo(value), value); err == nil {
				cleaned = append(cleaned, key)
			}
		}
	}
	return cleaned
}

func stripURLUserinfo(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.User == nil {
		return gitConfigUserinfoURL.ReplaceAllStringFunc(value, func(match string) string {
			scheme, _, _ := strings.Cut(match, "://")
			return scheme + "://"
		})
	}
	u.User = nil
	return u.String()
}

func cutLast(value, sep string) (string, string, bool) {
	i := strings.LastIndex(value, sep)
	if i < 0 {
		return value, "", false
	}
	return value[:i], value[i+len(sep):], true
}

// scrubGit edits one config file without reading any other configuration,
// so the file being cleaned cannot make git run a program.
func scrubGit(ctx context.Context, file string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"config", "--file", file}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_COUNT=0")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := readScrubConfig(out)
	// Close before waiting, so a producer stopped at the size limit can exit.
	_ = out.Close()
	return data, errors.Join(readErr, cmd.Wait())
}
