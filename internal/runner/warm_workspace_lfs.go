package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"golang.org/x/sys/unix"
)

type warmLFSFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
	Type string `json:"oid_type"`
}
type warmLFSManifest struct {
	Files    []warmLFSFile `json:"files"`
	storage  string
	verified map[string]int64
	byPath   map[string]warmLFSFile
}

func warmLFSCache(cache, project string, repo domain.RunnerRepository) string {
	key := sha256.Sum256([]byte(strings.Join([]string{project, repo.RepositoryID, repo.CloneURL}, "\x00")))
	return filepath.Join(filepath.Dir(cache), "lfs", hex.EncodeToString(key[:]))
}

func warmLFSObject(storage, oid string) string {
	return filepath.Join(storage, "objects", oid[:2], oid[2:4], oid)
}

// Each exclusive cache is shared only by slots for the same project, registered
// repo and frozen origin. Git authorization happens on each checkout and return.
func prepareWarmLFS(local, auth context.Context, stage, db, scope, head string, repo domain.RunnerRepository, syncDir func(*os.File) error) (warmLFSManifest, error) {
	var manifest warmLFSManifest
	if _, err := exec.LookPath("git-lfs"); err != nil {
		return manifest, checkWarmWithoutLFS(local, stage, head)
	}
	list, err := gitwork.C(local, stage, "lfs", "ls-files", "--json", "--include=*", "--exclude=", head)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal([]byte(list), &manifest); err != nil {
		return manifest, err
	}
	if len(manifest.Files) == 0 {
		return manifest, nil
	}
	dir, err := openWarmWorkspaceDir(scope, true)
	if err != nil {
		return manifest, err
	}
	defer dir.Close()
	lock, err := os.OpenFile(filepath.Join(scope, "transfer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return manifest, err
	}
	defer lock.Close()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return manifest, err
		}
		select {
		case <-local.Done():
			return manifest, local.Err()
		case <-ticker.C:
		}
	}
	manifest.storage = filepath.Join(scope, ".git", "lfs")
	manifest.verified = map[string]int64{}
	manifest.byPath = make(map[string]warmLFSFile, len(manifest.Files))
	missing := map[string]int64{}
	for _, item := range manifest.Files {
		if !validWarmWorkspaceRelativePath(item.Name) || len(item.OID) != 64 || item.Type != "sha256" || item.Size < 0 {
			return manifest, errors.New("invalid committed Git LFS pointer")
		}
		if _, err := hex.DecodeString(item.OID); err != nil {
			return manifest, err
		}
		if previous, ok := manifest.byPath[item.Name]; ok && previous != item {
			return manifest, errors.New("conflicting committed Git LFS paths")
		}
		manifest.byPath[item.Name] = item
		expected, verified := manifest.verified[item.OID]
		if !verified {
			expected, verified = missing[item.OID]
		}
		if verified {
			if expected != item.Size {
				return manifest, errors.New("conflicting committed Git LFS sizes")
			}
			continue
		}
		path := warmLFSObject(manifest.storage, item.OID)
		ok, err := warmLFSMatches(path, item)
		if err != nil && !os.IsNotExist(err) {
			return manifest, err
		}
		if ok {
			manifest.verified[item.OID] = item.Size
			continue
		}
		if err == nil {
			if err := os.Remove(path); err != nil {
				return manifest, err
			}
		}
		missing[item.OID] = item.Size
	}
	if len(missing) > 0 {
		if err := fetchWarmLFS(local, auth, stage, db, scope, head, repo); err != nil {
			return manifest, err
		}
	}
	for oid, size := range missing {
		path := warmLFSObject(manifest.storage, oid)
		ok, err := warmLFSMatches(path, warmLFSFile{OID: oid, Size: size})
		if err != nil {
			return manifest, err
		}
		if !ok {
			return manifest, errors.New("downloaded Git LFS object failed integrity check")
		}
		manifest.verified[oid] = size
	}
	// A failed fetch or an interrupted return may leave valid file bytes
	// with unconfirmed directory entries. Publish hits and downloads alike.
	parents := map[string]bool{}
	for oid := range manifest.verified {
		path := warmLFSObject(manifest.storage, oid)
		for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
			if parents[parent] {
				break
			}
			dir, err := openWarmWorkspaceDir(parent, false)
			if err != nil {
				return manifest, err
			}
			err = syncDir(dir)
			dir.Close()
			if err != nil {
				return manifest, err
			}
			parents[parent] = true
			if parent == scope {
				break
			}
		}
	}
	return manifest, nil
}

func fetchWarmLFS(local, auth context.Context, stage, db, scope, head string, repo domain.RunnerRepository) error {
	// A trusted transfer repository supplies both a private .git/lfs store and
	// commit-specific endpoint rules. Relative lfs.storage is resolved inside the
	// local and remote repos separately, including standalone-file transports.
	if _, err := gitwork.C(local, scope, "init", "--template=", warmObjectFormat(head)); err != nil {
		return err
	}
	objects := filepath.Join(db, "objects")
	alternates, err := os.ReadFile(filepath.Join(stage, ".git", "objects", "info", "alternates"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(alternates) > 0 {
		objects = strings.TrimSuffix(string(alternates), "\n")
	}
	if err := warmAlternate(filepath.Join(scope, ".git"), objects); err != nil {
		return err
	}
	config, err := gitwork.C(local, scope, "ls-tree", head, "--", ".lfsconfig")
	if err != nil {
		return err
	}
	if strings.HasPrefix(config, "100") {
		_, err = gitwork.C(local, scope, "restore", "--source="+head, "--worktree", "--", ".lfsconfig")
	} else {
		err = os.WriteFile(filepath.Join(scope, ".lfsconfig"), nil, 0600)
	}
	if err != nil {
		return err
	}
	if _, err := gitwork.C(auth, scope, "-c", "lfs.storage=lfs", "-c", "remote.origin.url="+repo.CloneURL, "-c", "lfs.fetchrecentalways=false", "lfs", "fetch", "--include=*", "--exclude=", "origin", head); err != nil {
		return fmt.Errorf("%w: cannot recover Git LFS objects: %w", errWarmCheckoutRetryable, err)
	}
	return nil
}

// Without git-lfs, ordinary repositories remain usable. Inspect candidate
// committed blobs before allowing a worktree to fall back to raw pointer text.
func checkWarmWithoutLFS(ctx context.Context, stage, head string) error {
	const version = "version https://git-lfs.github.com/spec/v1"
	pattern := "^" + regexp.QuoteMeta(version) + "$"
	paths, err := gitwork.C(ctx, stage, "grep", "-l", "-z", "-I", "-E", "-e", pattern, head, "--")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil
		}
		return err
	}
	for _, object := range strings.Split(paths, "\x00") {
		if object == "" {
			continue
		}
		if !strings.HasPrefix(object, head+":") {
			return errors.New("invalid Git LFS pointer candidate")
		}
		raw, err := gitwork.C(ctx, stage, "cat-file", "-s", object)
		if err != nil {
			return err
		}
		size, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		if size > 1024 {
			continue
		}
		blob, err := gitwork.C(ctx, stage, "cat-file", "blob", object)
		if err != nil {
			return err
		}
		if strings.HasPrefix(blob, version+"\n") && strings.Contains(blob, "\noid sha256:") {
			return errors.New("Git LFS is required to verify and restore committed assets")
		}
	}
	return nil
}
func warmLFSMatches(path string, item warmLFSFile) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("invalid protected Git LFS object")
	}
	if info.Size() != item.Size {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil || n != item.Size || hex.EncodeToString(sum.Sum(nil)) != item.OID {
		return false, err
	}
	return true, f.Sync()
}

// Materialize only pointer files. Never hardlink a user-writable asset into the
// protected cache or expose its storage path through the checkout's Git config.
func (m warmLFSManifest) hydrate(checkout, private string) error {
	for _, item := range m.Files {
		path := filepath.Join(checkout, item.Name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("expected regular Git LFS file %s", item.Name)
		}
		if info.Size() > 1024 {
			if info.Size() != item.Size {
				return fmt.Errorf("changed hydrated Git LFS asset %s", item.Name)
			}
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d", item.OID, item.Size)
		if strings.TrimSuffix(string(b), "\n") != pointer {
			if int64(len(b)) != item.Size || fmt.Sprintf("%x", sha256.Sum256(b)) != item.OID {
				return fmt.Errorf("invalid or changed Git LFS pointer %s", item.Name)
			}
			continue
		}
		if err := m.hydrateFile(path, private, info.Mode().Perm(), item); err != nil {
			return err
		}
	}
	return nil
}
func (m warmLFSManifest) hydrateFile(path, private string, mode os.FileMode, item warmLFSFile) error {
	source, err := os.Open(warmLFSObject(m.storage, item.OID))
	if err != nil {
		return err
	}
	defer source.Close()
	if private == "" {
		private = filepath.Dir(path)
	}
	tmp, err := os.CreateTemp(private, ".warm-lfs-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), source)
	if err != nil {
		return err
	}
	if n != item.Size || hex.EncodeToString(sum.Sum(nil)) != item.OID {
		return errors.New("Git LFS cache changed during materialization")
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncWarmWorkspaceParent(path)
}
