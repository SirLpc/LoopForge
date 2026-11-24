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
	"fmt"
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

//go:embed warm_native_windows_job.cs
var warmWindowsJobSource string

const warmWindowsPowerShell = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"

var errWindowsWarmGitUnavailable = errors.New("Windows Git helper is unavailable")

// On small worktrees WSL can finish verifying files before a Windows process
// has started. Use Windows once enough paths amortize the interop overhead.
const warmWindowsMinTracked = 512

// Windows helpers are called by the runner, outside the leased task sandbox.
// Only the original registered drive path is visible to Windows. The path in
// the runner's private bind mount is checked before using that original path.
type warmWindowsTree struct {
	root, checkout, original, eol string
	rootInfo, gitInfo             os.FileInfo
	changedPaths                  []string
	refPaths                      []string
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
	gitInfo, err := os.Lstat(filepath.Join(checkout, ".git"))
	if err != nil || !gitInfo.IsDir() {
		return warmWindowsTree{}, false
	}
	return warmWindowsTree{root: root, checkout: checkout, original: original, rootInfo: a, gitInfo: gitInfo}, true
}

func (w warmWindowsTree) verifyPinnedPaths() error {
	for _, path := range []string{w.checkout, w.original} {
		root, err := os.Lstat(path)
		if err != nil || !os.SameFile(root, w.rootInfo) {
			return warmTreeChanged("Windows checkout path changed before Git execution")
		}
		meta, err := os.Lstat(filepath.Join(path, ".git"))
		if err != nil || !os.SameFile(meta, w.gitInfo) {
			return warmTreeChanged("Windows checkout Git directory changed before Git execution")
		}
	}
	return nil
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
	ManagedGit    bool                   `json:"managedGit,omitempty"`
	RefreshIndex  bool                   `json:"refreshIndex,omitempty"`
	JobSource     string                 `json:"jobSource,omitempty"`
	IndexFile     string                 `json:"indexFile,omitempty"`
	FinishWrite   bool                   `json:"finishWrite,omitempty"`
	ChangedPaths  []string               `json:"changedPaths,omitempty"`
	GitInput      []byte                 `json:"gitInput,omitempty"`
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
	input = append(input, '\n')
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

// Keep the lease gate held until PowerShell has stopped its Git child.
// Closing the input pipe signals cancellation across the WSL interop boundary.
func (w warmWindowsTree) runManagedGit(ctx context.Context, request warmWindowsRequest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request.JobSource = warmWindowsJobSource
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(warmWindowsPowerShell, "-NoProfile", "-NonInteractive", "-EncodedCommand", warmWindowsEncodedScript)
	cmd.Dir = "/"
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer stdin.Close()
	var stdout bytes.Buffer
	stderr := warmWindowsGitStderr{ready: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(errWindowsWarmGitUnavailable, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = stdin.Close() })
	defer stop()
	startup := time.AfterFunc(15*time.Second, func() {
		// Git cannot start until this side acknowledges the path pins.
		_ = stdin.Close()
		_ = cmd.Process.Kill()
	})
	defer startup.Stop()
	_, writeErr := stdin.Write(append(input, '\n'))
	if writeErr != nil {
		_ = stdin.Close()
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var waitErr, guardErr error
	select {
	case <-stderr.ready:
		if startup.Stop() {
			guardErr = w.verifyPinnedPaths()
		} else {
			guardErr = errWindowsWarmGitUnavailable
		}
		if guardErr == nil {
			_, guardErr = stdin.Write([]byte("run\n"))
		}
		if guardErr != nil {
			_ = stdin.Close()
		}
		waitErr = <-wait
	case waitErr = <-wait:
		guardErr = errWindowsWarmGitUnavailable
	case <-ctx.Done():
		// No acknowledgement was sent, so only the launcher may be running.
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		waitErr = <-wait
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := errors.Join(writeErr, guardErr, waitErr); err != nil {
		diagnostic := strings.ReplaceAll(stderr.text.String(), warmWindowsGitReady, "")
		return nil, fmt.Errorf("Windows Git: %w: %s", err, gitwork.Redact(strings.TrimSpace(diagnostic)))
	}
	return stdout.Bytes(), nil
}

const warmWindowsGitReady = "loopforge-windows-git-ready\r\n"

type warmWindowsGitStderr struct {
	text  bytes.Buffer
	ready chan struct{}
	sent  bool
}

func (s *warmWindowsGitStderr) Write(p []byte) (int, error) {
	n, err := s.text.Write(p)
	if !s.sent && bytes.Contains(s.text.Bytes(), []byte(warmWindowsGitReady)) {
		s.sent = true
		close(s.ready)
	}
	return n, err
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
	return w.stageGit(ctx, stage, lfs, nil, args...)
}

func (w warmWindowsTree) stageGit(ctx context.Context, stage string, lfs bool, input []byte, args ...string) (string, error) {
	meta, err := warmWindowsPath(ctx, filepath.Join(stage, ".git"))
	if err != nil {
		return "", err
	}
	request := warmWindowsRequest{GitInput: input}
	if lfs {
		request.LFSScratch = rand.Text()
	}
	return w.runGit(ctx, meta, request, args)
}

func (w warmWindowsTree) checkoutGit(ctx context.Context, refreshIndex bool, args ...string) (string, error) {
	return w.checkoutGitIndex(ctx, refreshIndex, "", args...)
}

func (w warmWindowsTree) checkoutGitIndex(ctx context.Context, refreshIndex bool, index string, args ...string) (string, error) {
	request := warmWindowsRequest{ManagedGit: true, RefreshIndex: refreshIndex, LFSScratch: rand.Text()}
	if index != "" {
		request.IndexFile = w.root + `\.git\` + index
	}
	return w.runCheckoutGit(ctx, request, args)
}

func (w warmWindowsTree) changeCheckout(ctx context.Context, args ...string) (string, error) {
	request := warmWindowsRequest{ManagedGit: true, FinishWrite: true, LFSScratch: rand.Text()}
	request.ChangedPaths = w.refPaths
	if args[0] == "checkout" {
		request.ChangedPaths = append(append([]string(nil), w.refPaths...), w.changedPaths...)
	}
	return w.runCheckoutGit(ctx, request, args)
}

func (w warmWindowsTree) configureIdentity(ctx context.Context, name, email string) error {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	if name == "" || email == "" {
		return nil
	}
	for _, field := range []struct{ key, value string }{{"user.name", name}, {"user.email", email}} {
		value, err := nativeWarmGit(ctx, w.checkout, "config", "--local", "--get", field.key)
		if err == nil && value == field.value {
			continue
		}
		if _, err := w.changeCheckout(ctx, "config", "--local", field.key, field.value); err != nil {
			return nativeWarmGitWriteError(ctx, err)
		}
	}
	return nil
}

func (w warmWindowsTree) runCheckoutGit(ctx context.Context, request warmWindowsRequest, args []string) (string, error) {
	if w.eol != "" {
		args = append([]string{"-c", "core.eol=" + w.eol}, args...)
	}
	out, err := w.runGit(ctx, w.root+`\.git`, request, args)
	return strings.TrimRight(out, "\r\n"), err
}

func (w warmWindowsTree) runGit(ctx context.Context, meta string, request warmWindowsRequest, args []string) (string, error) {
	gitArgs := []string{"--no-pager", "--no-replace-objects", "--git-dir=" + meta, "--work-tree=" + w.root,
		"-c", "core.hooksPath=NUL", "-c", "core.attributesFile=NUL", "-c", "core.excludesFile=NUL",
		"-c", "core.fsmonitor=false", "-c", "filter.lfs.required=true", "-c", "core.protectNTFS=true", "-c", "gc.auto=0"}
	request.Worktree = w.root
	request.GitArgs = warmWindowsCommandLine(append(gitArgs, args...))
	if request.ManagedGit {
		out, err := w.runManagedGit(ctx, request)
		return string(out), err
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
