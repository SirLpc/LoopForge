package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

func nativeWarmGit(ctx context.Context, checkout string, args ...string) (string, error) {
	return gitwork.CStdout(ctx, checkout, append([]string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "filter.lfs.process=git-lfs filter-process --skip",
		"-c", "filter.lfs.smudge=git-lfs smudge --skip -- %f",
		"-c", "filter.lfs.clean=" + warmLFSVerificationClean,
		"-c", "filter.lfs.required=true",
		"-c", "core.fsmonitor=false",
	}, args...)...)
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

func prepareNativeWarmCheckout(ctx context.Context, checkout, cache string, slot warmWorkspaceSlot, repo domain.RunnerRepository, restore *domain.WarmWorkspaceRef, pending *nativeWarmCheckoutIntent, beforeWrite func(nativeWarmCheckoutIntent) error) (domain.WarmWorkspaceRef, error) {
	var ref domain.WarmWorkspaceRef
	if slot.Path != "" && nativeWarmEditorBusy(slot) {
		return ref, errors.Join(errNativeWarmCheckoutBusy, errors.New("close Unity before changing the leased checkout"))
	}
	local, cancel := context.WithCancel(context.Background())
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
	if pending == nil {
		status, err := nativeWarmGit(local, checkout, "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=normal", "--ignore-submodules=none")
		if err != nil || status != "" {
			return ref, fmt.Errorf("fixed warm checkout must be clean before switching branches: %w", errors.Join(err, errors.New(status)))
		}
	}
	current, err := warmCheckoutRef(checkout, repo.RepositoryID)
	if err != nil {
		return ref, err
	}
	head := current.Commit
	auth := repositoryGitContext(ctx, repo)
	if err := fetchNativeWarmCache(local, auth, cache, scratch, slot, checkout, head, repo); err != nil {
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
			Target: domain.WarmWorkspaceRef{RepositoryID: repo.RepositoryID, Branch: branch, Commit: target},
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
	library := filepath.Join(slot.UnityProjectPath, "Library")
	if err := warmTreeProtectsLibrary(local, cache, target, library); err != nil {
		return ref, err
	}
	if err := validateWarmLFSConfig(local, cache, target, repo.CloneURL); err != nil {
		return ref, err
	}
	stage := filepath.Join(scratch, "check")
	if _, err := gitwork.C(local, scratch, "init", "--template=", warmObjectFormat(head), stage); err != nil {
		return ref, err
	}
	if err := nativeWarmProofAlternates(filepath.Join(stage, ".git"), cache, checkout); err != nil {
		return ref, err
	}
	if err := warmTargetIgnoresLibrary(local, stage, target, library); err != nil {
		return ref, err
	}
	lfs, err := prepareWarmLFS(local, auth, stage, cache, warmLFSCache(cache, slot.ProjectID, repo), target, repo, (*os.File).Sync)
	if err != nil {
		return ref, err
	}
	// Import only missing Git objects from the authenticated local cache.
	// The checkout stays self-contained and can still be read by Windows Git.
	objects := []string{target}
	if intent.Baseline != target {
		objects = append(objects, intent.Baseline)
	}
	fetch := append([]string{"-c", "protocol.file.allow=always", "fetch", "--no-auto-gc", "--no-tags", "--no-write-fetch-head", cache}, objects...)
	if _, err := nativeWarmGit(local, checkout, fetch...); err != nil {
		return ref, err
	}
	atTarget, err := inspectNativeWarmCheckout(local, checkout, cache, slot, repo, intent)
	if err != nil {
		return ref, fmt.Errorf("verify fixed checkout before writing: %w", err)
	}
	if !atTarget {
		if err := checkWarmIgnoredCollisions(local, checkout, checkout, cache, head, target); err != nil {
			return ref, err
		}
	}
	if pending == nil {
		if err := beforeWrite(intent); err != nil {
			return ref, err
		}
	}
	if err := applyNativeWarmCheckout(local, checkout, slot, intent, atTarget, lfs); err != nil {
		return ref, err
	}
	return intent.Target, nil
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
