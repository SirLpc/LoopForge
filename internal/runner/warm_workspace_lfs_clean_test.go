package runner

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/gitwork"
)

func TestWarmLFSReadOnlyClean(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	filter, err := filepath.Abs("warm_workspace_lfs_clean.sh")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	runGitT(t, repo, "init", "-q")
	pointer := []byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 123\n")
	for name, data := range map[string][]byte{
		"empty": nil, "text": []byte("asset"), "changed": []byte("Asset"),
		"binary": bytes.Repeat([]byte{0, 255, 11, 42}, 65536), "pointer": pointer,
		"noncanonical":      bytes.ReplaceAll(pointer, []byte("\n"), []byte("\r\n")),
		"1023-byte pointer": append(bytes.Clone(pointer), bytes.Repeat([]byte("\n"), 1023-len(pointer))...),
		"1024-byte pointer": append(bytes.Clone(pointer), bytes.Repeat([]byte("\n"), 1024-len(pointer))...),
		"malformed pointer": bytes.Replace(pointer, []byte("sha256:"), []byte("sha256:?"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			clean := exec.Command("git", "-c", "lfs.storage="+filepath.Join(repo, "reference-lfs"), "lfs", "clean")
			clean.Dir, clean.Stdin = repo, bytes.NewReader(data)
			want, err := clean.Output()
			if err != nil {
				t.Fatal(err)
			}
			// Limit regular-file writes to 4 KiB. The binary fixture must stream.
			command := exec.Command("sh", "-c", `ulimit -f 8 && exec sh "$1"`, "sh", filter)
			command.Dir, command.Stdin = repo, bytes.NewReader(data)
			got, err := command.Output()
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("clean got %q, %v; git-lfs produced %q", got, err, want)
			}
		})
	}
	assertWarmNoLFSFiles(t, filepath.Join(repo, ".git", "lfs"))
}

func assertWarmNoLFSFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, ent fs.DirEntry, err error) error {
		if os.IsNotExist(err) && path == root {
			return nil
		}
		if err == nil && !ent.IsDir() {
			t.Errorf("baseline verification wrote %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWarmLFSBaselineDoesNotStoreAssets(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	checkout := t.TempDir()
	runGitT(t, checkout, "init", "-q")
	runGitT(t, checkout, "config", "filter.lfs.clean", "git-lfs clean -- %f")
	warmGitWrite(t, checkout, ".gitattributes", "*.bin filter=lfs -text\n")
	asset := strings.Repeat("sample", 65536)
	warmGitWrite(t, checkout, "asset.bin", asset)
	// A future mtime forces Git to hash an otherwise stat-matching, racily clean entry.
	mtime := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(checkout, "asset.bin"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	runGitT(t, checkout, "add", ".")
	runGitT(t, checkout, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "seed")
	head := runGitT(t, checkout, "rev-parse", "HEAD")
	for _, kind := range []string{"seed", "fresh index"} {
		t.Run(kind, func(t *testing.T) {
			private := t.TempDir()
			db := filepath.Join(private, "db")
			runGitT(t, private, "init", "-q", db)
			meta := filepath.Join(db, ".git")
			if err := warmAlternate(meta, filepath.Join(checkout, ".git", "objects")); err != nil {
				t.Fatal(err)
			}
			runGitT(t, db, "config", "filter.lfs.clean", "git-lfs clean -- %f")
			verify := func() error {
				if kind == "seed" {
					return verifyWarmSeedIndex(t.Context(), meta, checkout, head, true)
				}
				work := func(args ...string) (string, error) {
					return gitwork.C(t.Context(), db, append([]string{"--work-tree=" + checkout}, args...)...)
				}
				return verifyWarmWorktree(t.Context(), checkout, db, head, head, "Library", work)
			}
			if err := verify(); err != nil {
				t.Fatal(err)
			}
			assertWarmNoLFSFiles(t, filepath.Join(private, "seed-lfs"))
			assertWarmNoLFSFiles(t, filepath.Join(meta, "lfs"))
			warmGitWrite(t, checkout, "asset.bin", "X"+asset[1:])
			if err := verify(); err == nil {
				t.Fatal("accepted changed asset")
			}
			assertWarmNoLFSFiles(t, filepath.Join(private, "seed-lfs"))
			assertWarmNoLFSFiles(t, filepath.Join(meta, "lfs"))
			warmGitWrite(t, checkout, "asset.bin", asset)
		})
	}
}
