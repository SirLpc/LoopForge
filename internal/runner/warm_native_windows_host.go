package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"agentflow/internal/gitwork"
)

//go:embed warm_native_windows.ps1
var warmWindowsScript string

const warmWindowsPowerShell = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"

// On small worktrees WSL can finish verifying files before a Windows process
// has started. Use Windows once enough paths amortize the interop overhead.
const warmWindowsMinTracked = 512

// Windows helpers are called by the runner, outside the leased task sandbox.
// Only the original registered drive path is visible to Windows. The path in
// the runner's private bind mount is checked before using that original path.
type warmWindowsTree struct {
	root string
}

func openWarmWindowsTree(ctx context.Context, checkout, original string) (warmWindowsTree, bool) {
	if original == "" {
		return warmWindowsTree{}, false
	}
	if _, err := os.Stat(warmWindowsPowerShell); err != nil {
		return warmWindowsTree{}, false
	}
	a, err := os.Lstat(original)
	if err != nil || !a.IsDir() {
		return warmWindowsTree{}, false
	}
	b, err := os.Lstat(checkout)
	if err != nil || !b.IsDir() || !os.SameFile(a, b) || !warmWindowsDriveWithoutMetadata(original) {
		return warmWindowsTree{}, false
	}
	root, err := warmWindowsPath(ctx, original)
	if err != nil || len(root) < 3 || root[1:3] != `:\` {
		return warmWindowsTree{}, false
	}
	return warmWindowsTree{root: root}, true
}

func warmWindowsPath(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, "wslpath", "-w", path).Output()
	return strings.TrimRight(string(out), "\r\n"), err
}

type warmWindowsRequest struct {
	Worktree      string                 `json:"worktree,omitempty"`
	GitArgs       string                 `json:"gitArgs,omitempty"`
	LFSScratch    string                 `json:"lfsScratch,omitempty"`
	RemoveScratch string                 `json:"removeScratch,omitempty"`
	Files         []warmWindowsProofFile `json:"files,omitempty"`
}

var warmWindowsEncodedScript = func() string {
	var b []byte
	for _, unit := range utf16.Encode([]rune(warmWindowsScript)) {
		b = binary.LittleEndian.AppendUint16(b, unit)
	}
	return base64.StdEncoding.EncodeToString(b)
}()

func runWarmWindows(ctx context.Context, request warmWindowsRequest) ([]byte, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, warmWindowsPowerShell, "-NoProfile", "-NonInteractive", "-EncodedCommand", warmWindowsEncodedScript)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}

// Quote for Windows's argv parser. The PowerShell process never evaluates the
// arguments as source code or invokes a shell to run Git.
func warmWindowsCommandLine(args []string) string {
	var result strings.Builder
	for i, arg := range args {
		if i > 0 {
			result.WriteByte(' ')
		}
		result.WriteByte('"')
		backslashes := 0
		for _, c := range arg {
			if c == '\\' {
				backslashes++
				continue
			}
			if c == '"' {
				backslashes = backslashes*2 + 1
			}
			result.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0
			result.WriteRune(c)
		}
		result.WriteString(strings.Repeat(`\`, backslashes*2))
		result.WriteByte('"')
	}
	return result.String()
}

func (w warmWindowsTree) git(ctx context.Context, stage string, lfs bool, args ...string) (string, error) {
	meta, err := warmWindowsPath(ctx, filepath.Join(stage, ".git"))
	if err != nil {
		return "", err
	}
	gitArgs := []string{"--no-pager", "--no-replace-objects", "--git-dir=" + meta, "--work-tree=" + w.root,
		"-c", "core.hooksPath=NUL", "-c", "core.attributesFile=NUL", "-c", "core.excludesFile=NUL",
		"-c", "core.fsmonitor=false", "-c", "filter.lfs.required=true"}
	request := warmWindowsRequest{
		Worktree: w.root,
		GitArgs:  warmWindowsCommandLine(append(gitArgs, args...)),
	}
	if lfs {
		request.LFSScratch = rand.Text()
	}
	out, err := runWarmWindows(ctx, request)
	if request.LFSScratch != "" && ctx.Err() != nil {
		// A canceled interop process may never enter PowerShell's finally.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = runWarmWindows(cleanup, warmWindowsRequest{RemoveScratch: request.LFSScratch})
	}
	return string(out), err
}

func (w warmWindowsTree) clean(ctx context.Context, checkout, scratch string) error {
	meta := filepath.Join(checkout, ".git")
	head, err := warmCurrentHead(meta)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(scratch, ".windows-status-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if _, err := gitwork.C(ctx, stage, "init", "--template=", warmObjectFormat(head)); err != nil {
		return err
	}
	rules, err := nativeWarmProofConfig(ctx, checkout, stage)
	if err != nil {
		return err
	}
	if rules.fileMode {
		return errors.New("Windows Git cannot check Unix executable bits")
	}
	if err := copyWarmGitExclude(checkout, stage); err != nil {
		return err
	}
	index, err := os.ReadFile(filepath.Join(meta, "index"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, ".git", "index"), index, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, ".git", "HEAD"), []byte(head+"\n"), 0600); err != nil {
		return err
	}
	if err := warmSeedObjects(filepath.Join(meta, "objects")); err != nil {
		return err
	}
	windowsObjects := strings.ReplaceAll(w.root+`\.git\objects`, `\`, "/")
	if strings.ContainsAny(windowsObjects, "\r\n") {
		return errors.New("invalid Windows Git objects path")
	}
	if err := warmAlternate(filepath.Join(stage, ".git"), windowsObjects); err != nil {
		return err
	}
	return nativeWarmStatus(func(args ...string) (string, error) {
		return w.git(ctx, stage, true, args...)
	})
}

type warmWindowsProofFile struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
	LFS  string `json:"lfs,omitempty"`
	Size int64  `json:"size,omitempty"`
}

func (w warmWindowsTree) verifiedFiles(ctx context.Context, tracked map[string][]string, manifest warmLFSManifest) map[string]bool {
	files := make([]warmWindowsProofFile, 0, len(tracked))
	for path, entry := range tracked {
		if (entry[0] != "100644" && entry[0] != "100755") || !warmWindowsProofPath(path) {
			continue
		}
		f := warmWindowsProofFile{Path: path, Blob: entry[2]}
		if lfs, ok := manifest.byPath[path]; ok {
			size, verified := manifest.verified[lfs.OID]
			if !manifest.contentOnly && (!verified || size != lfs.Size) {
				continue
			}
			f.LFS, f.Size = lfs.OID, lfs.Size
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil
	}
	out, err := runWarmWindows(ctx, warmWindowsRequest{Worktree: w.root, Files: files})
	if err != nil {
		return nil
	}
	var paths []string
	if err := json.Unmarshal(out, &paths); err != nil {
		return nil
	}
	verified := make(map[string]bool, len(paths))
	for _, path := range paths {
		verified[path] = true
	}
	return verified
}

func warmWindowsProofPath(path string) bool {
	if !validWarmWorkspaceRelativePath(path) || !utf8.ValidString(path) || strings.ContainsAny(path, `\:"<>|?*`) ||
		strings.ContainsFunc(path, func(r rune) bool { return r < ' ' }) {
		return false
	}
	for part := range strings.SplitSeq(path, "/") {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}
