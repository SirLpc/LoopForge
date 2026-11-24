package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
)

// prepareInvocationWorkspace writes the upstream inputs the control plane
// resolved for this node and removes a gate result left by an earlier
// invocation, so the engine never reads a stale terminal decision.
func prepareInvocationWorkspace(workspace string, inv *domain.RunnerInvocation) error {
	if inv == nil {
		return nil
	}
	// Parallel branches consuming the same upstream artifact write the same
	// input file; chmod/write/chmod must not interleave.
	unlock := lockInvocationWorkspace(workspace)
	defer unlock()
	if _, err := runtime.RestoreMissingWorkspaceArtifacts(workspace, inv.ArchivedFiles); err != nil {
		return fmt.Errorf("restore archived outputs: %w", err)
	}
	if rel := strings.TrimSpace(inv.GateResultPath); rel != "" {
		if !domain.SafeWorkspaceRelPath(rel) {
			return fmt.Errorf("unsafe gate result path %q", rel)
		}
		if err := os.RemoveAll(filepath.Join(workspace, filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("clear gate result: %w", err)
		}
	}
	for _, input := range inv.Inputs {
		if err := writeInputFile(workspace, input); err != nil {
			return err
		}
	}
	return nil
}

var invocationWorkspaceLocks sync.Map // cleaned absolute workspace -> *sync.Mutex

func lockInvocationWorkspace(workspace string) func() {
	key := filepath.Clean(workspace)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	value, _ := invocationWorkspaceLocks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func writeInputFile(workspace string, input domain.RunnerInputFile) error {
	if !domain.SafeWorkspaceRelPath(input.Path) || !strings.HasPrefix(input.Path, ".agentflow/inputs/") {
		return fmt.Errorf("unsafe input path %q", input.Path)
	}
	target := filepath.Join(workspace, filepath.FromSlash(input.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create input directory: %w", err)
	}
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("input path is not a regular file: %s", input.Path)
		}
		if err := os.Chmod(target, 0o600); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(target, []byte(input.Content), 0o600); err != nil {
		return fmt.Errorf("write input %s: %w", input.Path, err)
	}
	return os.Chmod(target, 0o444)
}

// collectTaskOutputs reports the requested output files, the
// repository-patch inventory of every checkout, and workspace facts. It runs
// after success and failure alike so a failed node keeps its diagnostics.
func collectTaskOutputs(workspace string, inv *domain.RunnerInvocation) *domain.RunnerTaskOutputs {
	out, _ := collectTaskOutputsContext(context.Background(), workspace, inv)
	return out
}

func collectTaskOutputsContext(ctx context.Context, workspace string, inv *domain.RunnerInvocation) (*domain.RunnerTaskOutputs, error) {
	out := collectTaskOutputFiles(workspace, inv)
	if out == nil {
		return nil, nil
	}
	var err error
	out.Patches, err = collectPatchInventoriesContext(ctx, workspace, patchInventoryDirs(inv))
	frontend, evidence := domain.DetectFrontendProject(workspace)
	out.Facts = &domain.RunnerWorkspaceFacts{Frontend: frontend, FrontendEvidence: evidence}
	return out, err
}

func collectTaskOutputFiles(workspace string, inv *domain.RunnerInvocation) *domain.RunnerTaskOutputs {
	if inv == nil || !inv.ReportOutputs {
		return nil
	}
	out := &domain.RunnerTaskOutputs{}
	if inv.RecoverOutputs {
		out.Recovered = recoverOutputFiles(workspace, inv.OutputFiles)
	}
	out.Files, out.TooLarge = readOutputFiles(workspace, inv.OutputFiles)
	return out
}

func taskOutputIndexLock(root string, dirs []string) (string, error) {
	workspace, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer workspace.Close()
	for _, dir := range dirs {
		lock := filepath.Join(dir, ".git", "index.lock")
		if _, err := workspace.Lstat(lock); err == nil {
			return lock, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

var errWarmIndexLocked = errors.New("Git 索引锁未释放")

func outputIndexLocked(lock string) error {
	return fmt.Errorf("%w（%s），请在 Git 操作结束后重试节点", errWarmIndexLocked, filepath.ToSlash(lock))
}

func waitForTaskOutputIndex(ctx context.Context, root string, dirs []string) error {
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		lock, err := taskOutputIndexLock(root, dirs)
		if err != nil || lock == "" {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return outputIndexLocked(lock)
		case <-poll.C:
		}
	}
}

// readOutputFiles reads every safe, regular output file within the upload
// limit. Oversized files are reported by path instead; both results stay nil
// when nothing qualifies.
func readOutputFiles(workspace string, rels []string) (files map[string]string, tooLarge []string) {
	for _, rel := range rels {
		if !domain.SafeWorkspaceRelPath(rel) {
			continue
		}
		full := filepath.Join(workspace, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > maxArtifactUploadBytes {
			tooLarge = append(tooLarge, rel)
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		if files == nil {
			files = map[string]string{}
		}
		files[rel] = string(data)
	}
	return files, tooLarge
}

// patchInventoryDirs lists the workspace-relative checkout directories whose
// patch inventory is reported: the workspace root without repositories,
// otherwise each repository's checkout directory.
func patchInventoryDirs(inv *domain.RunnerInvocation) []string {
	repositories := invocationRepositoryContexts(inv)
	if len(repositories) == 0 {
		return []string{""}
	}
	dirs := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		dirs = append(dirs, filepath.ToSlash(safeRunnerRepoDir(repository.WorkspaceDir)))
	}
	return dirs
}

// collectPatchInventories maps each directory with a non-empty patch
// inventory to it; the map stays nil when no directory has changes.
func collectPatchInventories(workspace string, dirs []string) map[string]string {
	patches, _ := collectPatchInventoriesContext(context.Background(), workspace, dirs)
	return patches
}

func collectPatchInventoriesContext(ctx context.Context, workspace string, dirs []string) (map[string]string, error) {
	var patches map[string]string
	var inventoryErr error
	for _, dir := range dirs {
		patch, err := gitwork.PatchInventoryContext(ctx, filepath.Join(workspace, filepath.FromSlash(dir)))
		if err != nil {
			lock, _ := taskOutputIndexLock(workspace, []string{dir})
			if lock != "" || errors.Is(err, errWarmIndexLocked) || strings.Contains(err.Error(), ".git/index.lock") ||
				strings.Contains(err.Error(), `.git\index.lock`) {
				inventoryErr = outputIndexLocked(filepath.Join(dir, ".git", "index.lock"))
			} else if inventoryErr == nil && errors.Is(err, gitwork.ErrDiffIndexSnapshot) {
				inventoryErr = fmt.Errorf("无法创建 Git 变更清单的临时索引：%w", err)
			}
			continue
		}
		if len(patch) == 0 {
			continue
		}
		if patches == nil {
			patches = map[string]string{}
		}
		patches[dir] = string(patch)
	}
	return patches, inventoryErr
}

// recoverOutputFiles moves a missing root-level output into place when the
// Agent wrote a file with the same name elsewhere in the workspace.
func recoverOutputFiles(workspace string, outputs []string) []string {
	var recovered []string
	for _, rel := range outputs {
		if !domain.SafeWorkspaceRelPath(rel) {
			continue
		}
		dest := filepath.Join(workspace, filepath.FromSlash(rel))
		if _, err := os.Lstat(dest); err == nil {
			continue
		}
		src := findNamedFile(workspace, path.Base(rel), 4)
		if src == "" || src == dest {
			continue
		}
		info, err := os.Lstat(src)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxArtifactUploadBytes {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if os.MkdirAll(filepath.Dir(dest), 0o755) != nil || os.WriteFile(dest, data, 0o644) != nil {
			continue
		}
		from, _ := filepath.Rel(workspace, src)
		recovered = append(recovered, rel+" ← "+filepath.ToSlash(from))
	}
	return recovered
}

func findNamedFile(root, name string, maxDepth int) string {
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			switch d.Name() {
			case ".git", "node_modules", ".agentflow":
				return filepath.SkipDir
			}
			rel, relErr := filepath.Rel(root, p)
			if relErr == nil && strings.Count(rel, string(os.PathSeparator))+1 >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == name && d.Type().IsRegular() {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
