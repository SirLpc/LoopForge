package runtime

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A sandbox-supplied config, including a sparse file, must not consume the
// Runner's temp volume or an unbounded amount of memory during cleanup.
const maxScrubGitConfigBytes = 4 << 20

func openScrubBase(name string) (*os.Root, error) {
	if name == "" {
		return nil, os.ErrInvalid
	}
	parent, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return openScrubDirectory(parent, filepath.Base(name))
}

// Pin each real directory before opening its child. This also rejects a link
// pointing at another directory inside the same volume.
func openScrubDirectory(base *os.Root, rel string) (*os.Root, error) {
	if !filepath.IsLocal(rel) {
		return nil, os.ErrInvalid
	}
	dir, err := base.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if name == "." {
			continue
		}
		expected, err := dir.Lstat(name)
		if err != nil || !expected.IsDir() {
			dir.Close()
			return nil, os.ErrInvalid
		}
		child, err := dir.OpenRoot(name)
		dir.Close()
		if err != nil {
			return nil, err
		}
		actual, err := child.Stat(".")
		if err != nil || !os.SameFile(expected, actual) {
			child.Close()
			return nil, os.ErrInvalid
		}
		dir = child
	}
	return dir, nil
}

func scrubHomeBelow(ctx context.Context, base *os.Root, rel string) []string {
	home, err := openScrubDirectory(base, rel)
	if err != nil {
		return nil
	}
	defer home.Close()
	return scrubGitHome(ctx, home)
}

func scrubGitHome(ctx context.Context, home *os.Root) []string {
	var cleaned []string
	for _, name := range workspaceGitCredentialFiles {
		parent, err := openScrubDirectory(home, filepath.Dir(name))
		if err != nil {
			continue
		}
		leaf := filepath.Base(name)
		if _, err := parent.Lstat(leaf); err == nil && parent.RemoveAll(leaf) == nil {
			cleaned = append(cleaned, "HOME/"+name)
		}
		parent.Close()
	}
	for _, name := range []string{".gitconfig", ".config/git/config"} {
		for _, key := range scrubGitConfigFile(ctx, home, filepath.FromSlash(name)) {
			cleaned = append(cleaned, "HOME/"+name+" "+key)
		}
	}
	return cleaned
}

func scrubGitRoots(ctx context.Context, workspace string, homes []string, session string) []string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var cleaned []string
	ws, _ := openScrubBase(workspace)
	if ws != nil {
		defer ws.Close()
	}
	for _, home := range homes {
		if home == "" {
			continue
		}
		if rel, err := filepath.Rel(workspace, home); workspace != "" && err == nil && filepath.IsLocal(rel) {
			if ws != nil {
				cleaned = append(cleaned, scrubHomeBelow(ctx, ws, rel)...)
			}
			continue
		}
		// The caller can also supply an independent, trusted HOME.
		if root, err := openScrubBase(home); err == nil {
			cleaned = append(cleaned, scrubGitHome(ctx, root)...)
			root.Close()
		}
	}
	if session != "" {
		if root, err := openScrubBase(session); err == nil {
			cleaned = append(cleaned, scrubHomeBelow(ctx, root, "home")...)
			root.Close()
		}
		if ws != nil {
			cleaned = append(cleaned, scrubPrivateGitHomes(ctx, ws)...)
		}
	}
	if ws != nil {
		cleaned = append(cleaned, scrubCheckoutConfigs(ctx, ws)...)
	}
	return cleaned
}

func scrubPrivateGitHomes(ctx context.Context, ws *os.Root) []string {
	private, err := openScrubDirectory(ws, filepath.Join(".agentflow", "private-home"))
	if err != nil {
		return nil
	}
	defer private.Close()
	var cleaned []string
	entries, _ := fs.ReadDir(private.FS(), ".")
	for _, entry := range entries {
		if entry.IsDir() {
			cleaned = append(cleaned, scrubHomeBelow(ctx, private, entry.Name())...)
		}
	}
	return cleaned
}

func scrubCheckoutConfigs(ctx context.Context, ws *os.Root) []string {
	var cleaned []string
	// Match the depth and checkout limit used by gitwork.Checkouts. Only
	// config is needed here, so do not run Git in the writable repository.
	count := 0
	_ = fs.WalkDir(ws.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil || count >= 50 {
			return fs.SkipAll
		}
		if err != nil || !entry.IsDir() {
			return nil
		}
		if name != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" || entry.Name() == "vendor") {
			return fs.SkipDir
		}
		metadata := path.Join(name, ".git")
		if info, err := ws.Lstat(filepath.FromSlash(metadata)); err == nil && info.IsDir() {
			count++
			config := path.Join(metadata, "config")
			for _, key := range scrubGitConfigFile(ctx, ws, filepath.FromSlash(config)) {
				cleaned = append(cleaned, config+" "+key)
			}
			return fs.SkipDir
		}
		if strings.Count(name, "/") >= 1 {
			return fs.SkipDir
		}
		return nil
	})
	return cleaned
}

// Git only sees the copy in the Runner's private temp directory. Pin the
// original parent, honor Git's config lock, and publish by replacing the
// directory entry, never by truncating an inode supplied by the workspace.
func scrubGitConfigFile(ctx context.Context, base *os.Root, rel string) []string {
	if ctx.Err() != nil {
		return nil
	}
	parent, err := openScrubDirectory(base, filepath.Dir(rel))
	if err != nil {
		return nil
	}
	defer parent.Close()
	name := filepath.Base(rel)
	expected, err := parent.Lstat(name)
	if err != nil || !expected.Mode().IsRegular() {
		return nil
	}
	input, err := parent.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !os.SameFile(expected, info) || !singleLinkConfig(info) || info.Size() > maxScrubGitConfigBytes {
		return nil
	}
	stage, err := os.MkdirTemp("", "loopforge-git-scrub-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(stage)
	file := filepath.Join(stage, "config")
	copyFile, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil
	}
	_, err = io.CopyN(copyFile, input, info.Size())
	if errors.Join(err, copyFile.Close()) != nil {
		return nil
	}
	cleaned := scrubPrivateGitConfig(ctx, file)
	if len(cleaned) == 0 {
		return nil
	}
	result, err := os.Open(file)
	if err != nil {
		return nil
	}
	data, err := readScrubConfig(result)
	if errors.Join(err, result.Close(), ctx.Err()) != nil {
		return nil
	}
	lock := name + ".lock"
	output, err := parent.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return nil
	}
	// Once renamed, a new lock may already belong to another Git process.
	committed := false
	defer func() {
		if !committed {
			_ = parent.Remove(lock)
		}
	}()
	_, err = output.Write(data)
	if errors.Join(err, output.Close()) != nil {
		return nil
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(info, current) || !singleLinkConfig(current) ||
		info.Size() != current.Size() || !info.ModTime().Equal(current.ModTime()) {
		return nil
	}
	if ctx.Err() != nil || parent.Rename(lock, name) != nil {
		return nil
	}
	committed = true
	return cleaned
}

func singleLinkConfig(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && stat.Nlink == 1
}

// Git's expanded config listing can be larger than the original file.
func readScrubConfig(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxScrubGitConfigBytes+1))
	if len(data) > maxScrubGitConfigBytes {
		return nil, errors.New("Git configuration exceeds the cleanup size limit")
	}
	return data, err
}
