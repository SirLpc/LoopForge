package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"agentflow/internal/gitwork"
)

func warmWindowsLocalRef(ref string) bool {
	return ref != "" && ref != "HEAD" && !strings.HasPrefix(ref, "-") &&
		!strings.ContainsAny(ref, `\:~^ *?[`) && !strings.Contains(ref, "..") && !strings.Contains(ref, "@{") && !strings.ContainsFunc(ref, func(r rune) bool { return r < ' ' || r == 127 })
}

type warmWindowsWorktreePlan struct {
	target, head, tip string
	paths             []string
	lfs               bool
}

// Plan from object metadata so the checks do not scan the Windows worktree
// from WSL. Windows pins the affected paths before the Runner rechecks refs.
func planWarmWindowsWorktree(ctx context.Context, checkout, verb, target string) (warmWindowsWorktreePlan, bool) {
	if verb == "checkout" {
		target = "refs/heads/" + target
	}
	head, headOK := gitwork.RevParse(ctx, checkout, "HEAD")
	tip, tipOK := gitwork.RevParse(ctx, checkout, target)
	if !headOK || !tipOK || !warmWindowsDeliveryTree(ctx, checkout, tip) {
		return warmWindowsWorktreePlan{}, false
	}
	plan := warmWindowsWorktreePlan{target: target, head: head, tip: tip, lfs: warmWindowsHasLFS(ctx, checkout)}
	commands := [][]string{{"diff-tree", "--name-only", "--no-renames", "-r", "-z", head, tip, "--"}}
	if verb != "checkout" {
		base, err := nativeWarmGit(ctx, checkout, "merge-base", head, tip)
		if _, ok := runnerSafeGitObjectID(base); err != nil || !ok {
			return warmWindowsWorktreePlan{}, false
		}
		commands = append(commands, []string{"diff-tree", "--name-only", "--no-renames", "-r", "-z", base, head, "--"})
		if verb == "rebase" {
			if plan.lfs {
				return warmWindowsWorktreePlan{}, false
			}
			commands = append(commands, []string{"log", "--format=", "--name-only", "--no-renames", "-z", base + ".." + head, "--"})
		}
	}
	for _, command := range commands {
		raw, err := gitwork.CStdoutRaw(ctx, checkout, nil, command...)
		if err != nil {
			return warmWindowsWorktreePlan{}, false
		}
		for path := range strings.SplitSeq(strings.TrimSuffix(raw, "\x00"), "\x00") {
			if path == "" {
				continue
			}
			if !warmWindowsProofPath(path) {
				return warmWindowsWorktreePlan{}, false
			}
			plan.paths = append(plan.paths, path)
		}
	}
	slices.Sort(plan.paths)
	plan.paths = slices.Compact(plan.paths)
	return plan, !warmWindowsDirectoryTransition(plan.paths) && (!plan.lfs || plan.hasNoLFSTransitions(ctx, checkout))
}

func warmWindowsDirectoryTransition(paths []string) bool {
	changed := make(map[string]bool, len(paths))
	for _, path := range paths {
		path = strings.ToLower(path)
		if changed[path] {
			return true
		}
		changed[path] = true
	}
	for path := range changed {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if changed[parent] {
				return true
			}
		}
	}
	return false
}

func warmWindowsDeliveryTree(ctx context.Context, checkout, tip string) bool {
	raw, err := gitwork.CStdoutRaw(ctx, checkout, nil, "ls-tree", "-rz", tip)
	if err != nil {
		return false
	}
	seen := make(map[string]string)
	for entry := range strings.SplitSeq(strings.TrimSuffix(raw, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		header, path, ok := strings.Cut(entry, "\t")
		if !ok || !warmWindowsProofPath(path) || !(strings.HasPrefix(header, "100644 blob ") || strings.HasPrefix(header, "100755 blob ")) {
			return false
		}
		for path != "." {
			lower := strings.ToLower(path)
			if previous, exists := seen[lower]; exists {
				if previous != path {
					return false
				}
				break
			}
			seen[lower] = path
			path = filepath.Dir(path)
		}
	}
	return true
}

func (p warmWindowsWorktreePlan) stillCurrent(ctx context.Context, checkout string) bool {
	head, _ := gitwork.RevParse(ctx, checkout, "HEAD")
	tip, _ := gitwork.RevParse(ctx, checkout, p.target)
	return head == p.head && tip == p.tip && p.lfs == warmWindowsHasLFS(ctx, checkout) &&
		(!p.lfs || p.hasNoLFSTransitions(ctx, checkout))
}

// An offline Windows helper must not leave newly checked-out LFS pointers in
// place of assets. Test both trees and the live attributes for affected paths.
func (p warmWindowsWorktreePlan) hasNoLFSTransitions(ctx context.Context, checkout string) bool {
	if len(p.paths) == 0 {
		return true
	}
	for _, path := range p.paths {
		if strings.EqualFold(filepath.Base(path), ".gitattributes") {
			return false
		}
	}
	input := strings.Join(p.paths, "\x00") + "\x00"
	for _, ref := range []string{p.head, p.tip, ""} {
		args := []string{"check-attr", "-z", "--stdin"}
		if ref != "" {
			args = append(args, "--source="+ref)
		}
		raw, err := gitwork.CStdoutRaw(ctx, checkout, strings.NewReader(input), append(args, "filter")...)
		fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
		if err != nil || len(fields) != 3*len(p.paths) {
			return false
		}
		for i := 2; i < len(fields); i += 3 {
			if fields[i] == "lfs" {
				return false
			}
		}
	}
	return true
}

func warmWindowsHasLFS(ctx context.Context, checkout string) bool {
	for _, name := range []string{"config", "config.worktree"} {
		file := filepath.Join(checkout, ".git", name)
		if _, err := os.Lstat(file); name == "config.worktree" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		out, err := gitwork.C(ctx, os.TempDir(), "config", "--no-includes", "--file", file, "--get-regexp", `^filter\.lfs\.`)
		if err == nil || out != "" {
			return true
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return true
		}
	}
	return false
}
