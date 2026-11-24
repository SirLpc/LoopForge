package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

type warmGitRunIDKey struct{}

func recordWarmGitDuration(ctx context.Context, phase, backend string, since time.Time, err error) {
	if runID, _ := ctx.Value(warmGitRunIDKey{}).(string); runID != "" {
		log.Printf("warm Git run=%q phase=%s backend=%s elapsed=%s ok=%t", runID, phase, backend, time.Since(since).Round(time.Millisecond), err == nil)
	}
}

func nativeWarmGit(ctx context.Context, checkout string, args ...string) (string, error) {
	return gitwork.CStdout(ctx, checkout, nativeWarmGitArgs(args...)...)
}

func nativeWarmGitArgs(args ...string) []string {
	return append([]string{
		"--no-replace-objects",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "filter.lfs.process=git-lfs filter-process --skip",
		"-c", "filter.lfs.smudge=git-lfs smudge --skip -- %f",
		"-c", "filter.lfs.clean=" + warmLFSVerificationClean,
		"-c", "filter.lfs.required=true",
		"-c", "core.fsmonitor=false",
	}, args...)
}

// An operator's checkout keeps its Git database. Only the trusted cache
// contacts the remote with credentials. Limit the local config before Git
// runs any operation that could interpret executable configuration.
func verifyNativeWarmConfig(ctx context.Context, checkout, scratch string) error {
	meta, err := openWarmWorkspaceDir(filepath.Join(checkout, ".git"), false)
	if err != nil {
		return err
	}
	meta.Close()
	for _, name := range []string{"config", "config.worktree"} {
		path := filepath.Join(meta.Name(), name)
		if err := warmRegular(path); err != nil {
			if name == "config.worktree" {
				if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
					continue
				}
			}
			return err
		}
		entries, err := gitwork.C(ctx, scratch, "config", "--no-includes", "--file", path, "-z", "--list")
		if err != nil {
			return err
		}
		for _, entry := range strings.Split(entries, "\x00") {
			key, value, _ := strings.Cut(entry, "\n")
			if nativeWarmConfigEntry(key, value) {
				continue
			}
			return fmt.Errorf("unsupported Git configuration in fixed warm checkout: %s", key)
		}
	}
	return checkWarmPendingGitState(meta.Name())
}

func nativeWarmConfigEntry(key, value string) bool {
	switch key {
	case "filter.lfs.clean":
		return value == "git-lfs clean -- %f"
	case "filter.lfs.smudge":
		return value == "git-lfs smudge -- %f" || value == "git-lfs smudge --skip -- %f"
	case "filter.lfs.process":
		return value == "git-lfs filter-process" || value == "git-lfs filter-process --skip"
	case "", "core.bare", "core.filemode", "core.ignorecase", "core.logallrefupdates",
		"core.repositoryformatversion", "core.symlinks", "core.autocrlf", "core.eol",
		"extensions.worktreeconfig", "extensions.objectformat", "gc.auto", "remote.origin.url",
		"remote.origin.fetch", "user.name", "user.email", "lfs.concurrenttransfers",
		"lfs.repositoryformatversion", "filter.lfs.required":
		return true
	}
	if strings.HasPrefix(key, "branch.") {
		return strings.HasSuffix(key, ".remote") || strings.HasSuffix(key, ".merge") || strings.HasSuffix(key, ".autodevpip-base")
	}
	return strings.HasPrefix(key, "lfs.") && strings.HasSuffix(key, ".access")
}

func checkNativeWarmClean(ctx context.Context, checkout, scratch, original string) error {
	return checkNativeWarmCleanWithGit(ctx, checkout, scratch, original, warmWindowsTree{})
}

func checkNativeWarmCleanWithGit(ctx context.Context, checkout, scratch, original string, windows warmWindowsTree) error {
	windowsCompatible, count, err := inspectNativeWarmIndex(ctx, checkout, scratch)
	if err != nil {
		return err
	}
	if windows.root != "" {
		if !windowsCompatible {
			return warmTreeChanged("native checkout index changed before Windows Git verification")
		}
		return nativeWarmStatus(func(args ...string) (string, error) {
			out, err := windows.checkoutGit(ctx, false, args...)
			return out, nativeWarmGitWriteError(ctx, err)
		})
	}
	if windowsCompatible && count >= warmWindowsMinTracked {
		if w, ok := openWarmWindowsTree(ctx, checkout, original); ok {
			if err := w.clean(ctx, checkout, scratch); err == nil || errors.Is(err, errNativeWarmDirty) || ctx.Err() != nil {
				return err
			}
		}
	}
	return nativeWarmStatus(func(args ...string) (string, error) {
		return gitwork.CStdoutRaw(ctx, checkout, nil, nativeWarmGitArgs(args...)...)
	})
}

func inspectNativeWarmIndex(ctx context.Context, checkout, scratch string) (bool, int, error) {
	if err := verifyNativeWarmConfig(ctx, checkout, scratch); err != nil {
		return false, 0, err
	}
	// Git status must compare the published tree using the repository's indexed
	// attributes and ancestry. A local override could hide a worktree edit.
	for _, name := range []string{"attributes", "grafts"} {
		info, err := os.Lstat(filepath.Join(checkout, ".git", "info", name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, 0, err
		}
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return false, 0, fmt.Errorf("fixed warm checkout has unsupported local Git info/%s", name)
		}
	}
	if err := warmRegular(filepath.Join(checkout, ".git", "index")); err != nil {
		return false, 0, err
	}
	entries, err := gitwork.CStdoutRaw(ctx, checkout, nil, "ls-files", "-v", "--stage", "-z")
	if err != nil {
		return false, 0, err
	}
	windowsCompatible := true
	for entry := range strings.SplitSeq(strings.TrimSuffix(entries, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		flags, path, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasPrefix(flags, "H ") {
			return false, 0, fmt.Errorf("fixed warm checkout has hidden or unmerged index entries: %q", path)
		}
		if !strings.HasPrefix(flags, "H 100644 ") && !strings.HasPrefix(flags, "H 100755 ") {
			windowsCompatible = false
		}
		if strings.HasPrefix(flags, "H 160000 ") {
			if !validWarmWorkspaceRelativePath(path) {
				return false, 0, errors.New("invalid fixed warm submodule path")
			}
			children, err := os.ReadDir(filepath.Join(checkout, path))
			if err != nil && !os.IsNotExist(err) {
				return false, 0, err
			}
			if len(children) > 0 {
				return false, 0, warmTreeChanged("populated submodule %s needs inspection", path)
			}
		}
	}
	return windowsCompatible, strings.Count(entries, "\x00"), nil
}

func selectNativeWarmWindowsGit(ctx context.Context, checkout, scratch, original string) (warmWindowsTree, error) {
	start := time.Now()
	w, ok := openWarmWindowsTree(ctx, checkout, original)
	if !ok {
		recordWarmGitDuration(ctx, "windows-path-ineligible", "wsl", start, ctx.Err())
		return warmWindowsTree{}, ctx.Err()
	}
	compatible, count, err := inspectNativeWarmIndex(ctx, checkout, scratch)
	if err != nil || !compatible || count < warmWindowsMinTracked {
		recordWarmGitDuration(ctx, "windows-index-ineligible", "wsl", start, err)
		return warmWindowsTree{}, err
	}
	info, err := openWarmWorkspaceDir(filepath.Join(checkout, ".git", "info"), false)
	if err == nil {
		info.Close()
		if err := warmRegular(filepath.Join(info.Name(), "exclude")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return warmWindowsTree{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return warmWindowsTree{}, err
	}
	mode, err := nativeWarmGit(ctx, checkout, "config", "--type=bool", "--get", "core.filemode")
	if err != nil || mode != "false" {
		recordWarmGitDuration(ctx, "windows-filemode-ineligible", "wsl", start, err)
		return warmWindowsTree{}, ctx.Err()
	}
	eol, err := nativeWarmGit(ctx, checkout, "config", "--get", "core.eol")
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return warmWindowsTree{}, err
	}
	if eol == "" || eol == "native" {
		eol = "lf"
	}
	if eol != "lf" && eol != "crlf" {
		return warmWindowsTree{}, errors.New("unsupported Git line endings in fixed warm checkout")
	}
	w.eol = eol
	return w, nil
}

var errNativeWarmDirty = errors.New("fixed warm checkout is not clean")

func nativeWarmStatus(git func(...string) (string, error)) error {
	// Let Git refresh index stat information after LFS files are materialized.
	status, err := git("status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil || status == "" {
		return err
	}
	// Git can report replaced LFS placeholders as modified when only their
	// index stat information is stale. Compare filtered contents in that case.
	changes := strings.TrimSuffix(status, "\x00")
	for change := range strings.SplitSeq(changes, "\x00") {
		if !strings.HasPrefix(change, " M ") {
			return fmt.Errorf("%w: %s", errNativeWarmDirty, strings.ReplaceAll(changes, "\x00", "\n"))
		}
	}
	_, err = git("diff", "--quiet", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", "--")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return fmt.Errorf("%w: %s: %w", errNativeWarmDirty, strings.ReplaceAll(changes, "\x00", "\n"), err)
		}
		return err
	}
	return nil
}

type nativeWarmCheckoutResult struct {
	domain.WarmWorkspaceRef
	windows warmWindowsTree
}

func prepareNativeWarmCheckout(ctx context.Context, checkout, cache string, slot warmWorkspaceSlot, repo domain.RunnerRepository, restore *domain.WarmWorkspaceRef, pending *nativeWarmCheckoutIntent, beforeWrite func(nativeWarmCheckoutIntent) error) (result nativeWarmCheckoutResult, resultErr error) {
	var ref nativeWarmCheckoutResult
	if slot.Path != "" && nativeWarmEditorBusy(slot) {
		return ref, errors.Join(errNativeWarmCheckoutBusy, errors.New("close Unity before changing the leased checkout"))
	}
	runID, _ := ctx.Value(warmGitRunIDKey{}).(string)
	local, cancel := context.WithCancel(context.WithValue(context.Background(), warmGitRunIDKey{}, runID))
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	parent, err := openWarmWorkspaceDir(filepath.Dir(cache), true)
	if err != nil {
		return ref, err
	}
	parent.Close()
	scratch, err := os.MkdirTemp(parent.Name(), ".native-warm-git-")
	if err != nil {
		return ref, err
	}
	defer os.RemoveAll(scratch)
	if err := verifyNativeWarmConfig(local, checkout, scratch); err != nil {
		return ref, err
	}
	origin, err := nativeWarmGit(local, checkout, "config", "--local", "--get-all", "remote.origin.url")
	if err != nil || domain.URLHasCredentials(origin) || gitwork.CanonicalRemote(origin) != gitwork.CanonicalRemote(repo.CloneURL) {
		return ref, errors.New("fixed warm checkout origin does not match the frozen remote")
	}
	if err := gitwork.ValidateRemoteURL(repo.CloneURL, true); err != nil {
		return ref, err
	}
	current, err := warmCheckoutRef(checkout, repo.RepositoryID)
	if err != nil {
		return ref, err
	}
	head := current.Commit
	auth := repositoryGitContext(ctx, repo)
	start := time.Now()
	err = fetchNativeWarmCache(local, auth, cache, scratch, slot, checkout, head, repo)
	recordWarmGitDuration(ctx, "remote-fetch", "wsl", start, err)
	if err != nil {
		return ref, err
	}
	var intent nativeWarmCheckoutIntent
	if pending != nil {
		intent = *pending
		if err := authorizeWarmRestore(local, auth, scratch, cache, repo, intent.Target.Commit); err != nil {
			return ref, errors.Join(errWarmCheckoutRetryable, err)
		}
	} else {
		if restore != nil {
			if restore.RepositoryID != repo.RepositoryID || !warmCommitID.MatchString(restore.Commit) {
				return ref, errors.New("invalid fixed warm restore reference")
			}
			if err := authorizeWarmRestore(local, auth, scratch, cache, repo, restore.Commit); err != nil {
				return ref, errors.Join(errWarmCheckoutRetryable, err)
			}
		}
		target, branch, err := warmCheckoutTarget(local, cache, repo, restore)
		if err != nil {
			return ref, err
		}
		tip, err := nativeWarmBranchTip(local, checkout, branch)
		if err != nil {
			return ref, err
		}
		intent = nativeWarmCheckoutIntent{
			SlotID: slot.ID, Before: current, BranchBefore: tip, Baseline: nativeWarmBaseline(local, cache, repo, restore, target),
			LFSStageID: rand.Text(),
			Target:     domain.WarmWorkspaceRef{RepositoryID: repo.RepositoryID, Branch: branch, Commit: target},
		}
		if tip != "" && tip != target {
			if _, err := gitwork.C(local, cache, "merge-base", "--is-ancestor", tip, target); err != nil {
				if restore == nil {
					return ref, fmt.Errorf("fixed warm checkout has local commits on %s", branch)
				}
				intent.Rewind = true
			}
		}
	}
	if intent.Rewind {
		if err := checkNativeWarmRewind(local, cache, intent); err != nil {
			return ref, err
		}
	}
	target := intent.Target.Commit
	start = time.Now()
	verifyRecovery := pending != nil || restore != nil
	var windows warmWindowsTree
	if verifyRecovery || head != target || current.Branch != intent.Target.Branch {
		windows, err = selectNativeWarmWindowsGit(local, checkout, scratch, slot.Path)
		if err != nil {
			return ref, err
		}
	}
	if windows.root != "" && intent.Before.Commit != target {
		entries, err := gitwork.CStdoutRaw(local, cache, nil, "ls-tree", "-rz", target)
		if err != nil {
			return ref, err
		}
		for entry := range strings.SplitSeq(entries, "\x00") {
			if strings.HasPrefix(entry, "120000 ") || strings.HasPrefix(entry, "160000 ") {
				windows = warmWindowsTree{}
				break
			}
		}
		if windows.root != "" {
			paths, err := gitwork.CStdoutRaw(local, cache, nil, "diff-tree", "-r", "--no-renames", "--name-only", "-z", intent.Before.Commit, target)
			if err != nil {
				return ref, err
			}
			for path := range strings.SplitSeq(strings.TrimSuffix(paths, "\x00"), "\x00") {
				if path == "" {
					continue
				}
				if !warmWindowsProofPath(path) {
					windows = warmWindowsTree{}
					break
				}
				windows.changedPaths = append(windows.changedPaths, path)
			}
		}
	}
	if windows.root != "" && intent.Target.Branch != "" {
		ref := "refs/heads/" + intent.Target.Branch
		if !warmWindowsProofPath(ref) {
			windows = warmWindowsTree{}
		} else {
			windows.refPaths = []string{".git/" + ref, ".git/logs/" + ref}
		}
	}
	backend := "wsl"
	if windows.root != "" {
		backend = "windows"
	}
	if slot.Path != "" {
		recordWarmGitDuration(ctx, "select-checkout-git", backend, start, nil)
	}
	defer func(since time.Time) { recordWarmGitDuration(ctx, "local-preparation", backend, since, resultErr) }(time.Now())
	if pending == nil && current.Branch == "" && head != target {
		if err := authorizeWarmRestore(local, auth, scratch, cache, repo, head); err != nil {
			return ref, fmt.Errorf("fixed warm checkout is detached at a commit unavailable from the remote: %w", err)
		}
	}
	if err := validateWarmLFSConfig(local, cache, target, repo.CloneURL); err != nil {
		return ref, err
	}
	var lfs warmLFSManifest
	if verifyRecovery || head != target {
		start := time.Now()
		stage := filepath.Join(scratch, "check")
		if _, err := gitwork.C(local, scratch, "init", "--template=", warmObjectFormat(head), stage); err != nil {
			return ref, err
		}
		if err := nativeWarmProofAlternates(filepath.Join(stage, ".git"), cache, checkout); err != nil {
			return ref, err
		}
		scope := warmLFSCache(cache, slot.ProjectID, repo)
		if verifyRecovery {
			lfs, err = prepareWarmLFS(local, auth, stage, cache, scope, target, repo, (*os.File).Sync)
		} else {
			lfs, err = prepareNativeWarmChangedLFS(local, auth, stage, scope, head, target, repo)
		}
		recordWarmGitDuration(ctx, "lfs-prepare", "wsl", start, err)
		if err != nil {
			return ref, err
		}
	}
	// Import only missing Git objects from the authenticated local cache.
	// The checkout stays self-contained and can still be read by Windows Git.
	if head != target || intent.Baseline != head {
		if err := verifyNativeWarmConfig(local, checkout, scratch); err != nil {
			return ref, err
		}
		objects := []string{target}
		if intent.Baseline != target {
			objects = append(objects, intent.Baseline)
		}
		fetch := append([]string{"-c", "protocol.file.allow=always", "fetch", "--no-auto-gc", "--no-tags", "--no-write-fetch-head", cache}, objects...)
		start := time.Now()
		_, err := nativeWarmGit(local, checkout, fetch...)
		recordWarmGitDuration(ctx, "local-object-import", "wsl", start, err)
		if err != nil {
			return ref, err
		}
	}
	var atTarget bool
	var publishIndex func() error
	start = time.Now()
	if verifyRecovery {
		if windows.root != "" && (pending != nil || current.Commit != target || current.Branch != intent.Target.Branch) {
			winHead, err := windows.checkoutGit(local, false, "rev-parse", "--verify", "HEAD^{commit}")
			if errors.Is(err, errWindowsWarmGitUnavailable) {
				windows, backend = warmWindowsTree{}, "wsl"
				recordWarmGitDuration(ctx, "windows-git-unavailable", backend, start, err)
			} else if err != nil {
				return ref, err
			} else if winHead != head {
				return ref, warmTreeChanged("Windows Git resolved a different checkout")
			}
		}
		atTarget, err = inspectNativeWarmCheckout(local, checkout, cache, slot, repo, intent)
		if err != nil {
			recordWarmGitDuration(ctx, "pre-checkout-verification", backend, start, err)
			return ref, fmt.Errorf("verify fixed checkout before writing: %w", err)
		}
	} else {
		current, err := warmCheckoutRef(checkout, repo.RepositoryID)
		if err != nil {
			return ref, err
		}
		if current.Commit != intent.Before.Commit || current.Branch != intent.Before.Branch {
			return ref, warmTreeChanged("fixed warm checkout changed while fetching")
		}
		if windows.root != "" {
			compatible, _, inspectErr := inspectNativeWarmIndex(local, checkout, scratch)
			if inspectErr != nil {
				return ref, inspectErr
			}
			if !compatible {
				return ref, warmTreeChanged("native checkout index changed before Windows Git verification")
			}
			publishIndex, err = windows.planIndexRefresh(local, nil)
		} else {
			err = checkNativeWarmClean(local, checkout, scratch, slot.Path)
		}
		if errors.Is(err, errWindowsWarmGitUnavailable) {
			windows, backend = warmWindowsTree{}, "wsl"
			recordWarmGitDuration(ctx, "windows-git-unavailable", backend, start, err)
			err = checkNativeWarmClean(local, checkout, scratch, "")
		}
		if err != nil {
			recordWarmGitDuration(ctx, "pre-checkout-verification", backend, start, err)
			return ref, err
		}
		atTarget = current.Commit == intent.Target.Commit && current.Branch == intent.Target.Branch
	}
	recordWarmGitDuration(ctx, "pre-checkout-verification", backend, start, nil)
	if !atTarget {
		if err := checkWarmIgnoredCollisions(local, checkout, checkout, cache, head, target, windows); err != nil {
			return ref, err
		}
	}
	if pending == nil {
		if err := beforeWrite(intent); err != nil {
			return ref, err
		}
	}
	if publishIndex != nil {
		if err := publishIndex(); err != nil {
			return ref, err
		}
	}
	if err := applyNativeWarmCheckout(local, checkout, slot, intent, atTarget, lfs, windows); err != nil {
		return ref, err
	}
	return nativeWarmCheckoutResult{WarmWorkspaceRef: intent.Target, windows: windows}, nil
}

func fetchNativeWarmCache(local, auth context.Context, cache, scratch string, slot warmWorkspaceSlot, checkout, head string, repo domain.RunnerRepository) error {
	seed := slot.Path
	if seed == "" {
		seed = checkout
	}
	objects := filepath.Join(seed, ".git", "objects")
	if err := warmSeedObjects(objects); err != nil {
		return err
	}
	db := cache
	_, err := os.Lstat(cache)
	fresh := errors.Is(err, os.ErrNotExist)
	if fresh {
		db = filepath.Join(scratch, "cache")
		for _, args := range [][]string{
			{"init", "--bare", "--template=", warmObjectFormat(head), db},
			{"-C", db, "config", "warm.repositoryID", repo.RepositoryID},
			{"-C", db, "remote", "add", "origin", repo.CloneURL},
		} {
			if _, err := gitwork.C(local, scratch, args...); err != nil {
				return err
			}
		}
		if err := warmAlternate(db, objects); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	id, err := gitwork.C(local, db, "config", "--get", "warm.repositoryID")
	if err != nil || id != repo.RepositoryID {
		return errors.New("fixed warm cache repository changed")
	}
	origin, err := gitwork.C(local, db, "remote", "get-url", "origin")
	if err != nil || origin != repo.CloneURL {
		return errors.New("fixed warm cache remote changed")
	}
	// The seed remains in the personal checkout. Advertise only its HEAD
	// for negotiation and never promote locally stored refs to remote refs.
	if _, err := gitwork.C(local, db, "update-ref", "refs/warm/seed", head); err != nil {
		return err
	}
	_, err = gitwork.C(auth, db, "fetch", "--no-auto-gc", "--prune", "--no-tags", "origin",
		"+HEAD:refs/warm/default", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*")
	_, cleanErr := gitwork.C(local, db, "update-ref", "-d", "refs/warm/seed")
	if err != nil || cleanErr != nil {
		return errors.Join(errWarmCheckoutRetryable, err, cleanErr)
	}
	if fresh {
		return os.Rename(db, cache)
	}
	return nil
}

// Git LFS's object scanner reads direct alternates only.
func nativeWarmProofAlternates(meta, cache, checkout string) error {
	objects := filepath.Join(cache, "objects") + "\n" + filepath.Join(checkout, ".git", "objects")
	if strings.ContainsAny(cache+checkout, "\r\n") {
		return errors.New("invalid native warm Git objects path")
	}
	return warmAlternate(meta, objects)
}

func nativeWarmProofConfig(ctx context.Context, checkout, stage string) (warmCheckoutFileRules, error) {
	rules := warmCheckoutFileRules{fileMode: true, symlinks: true}
	for _, setting := range []struct {
		key    string
		option string
	}{
		{"core.filemode", "--type=bool"},
		{"core.symlinks", "--type=bool"},
		{"core.ignorecase", "--type=bool"},
		{"core.autocrlf", "--type=bool-or-str"},
		{"core.eol", ""},
	} {
		args := []string{"config"}
		if setting.option != "" {
			args = append(args, setting.option)
		}
		value, err := nativeWarmGit(ctx, checkout, append(args, "--get", setting.key)...)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && value == "" {
			continue
		}
		if err != nil {
			return rules, err
		}
		switch setting.key {
		case "core.filemode":
			rules.fileMode = value != "false"
		case "core.symlinks":
			rules.symlinks = value != "false"
		case "core.autocrlf":
			if value != "true" && value != "false" && value != "input" {
				return rules, errors.New("unsupported core.autocrlf value in native warm checkout")
			}
		case "core.eol":
			if value != "lf" && value != "crlf" && value != "native" {
				return rules, errors.New("unsupported core.eol value in native warm checkout")
			}
		}
		if _, err := gitwork.C(ctx, stage, "config", setting.key, value); err != nil {
			return rules, err
		}
	}
	return rules, nil
}
