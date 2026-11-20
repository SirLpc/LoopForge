package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScrubConversationOnlyUsesRealPrivateHomeDirectories(t *testing.T) {
	session, workspace, unrelated := t.TempDir(), t.TempDir(), t.TempDir()
	account := filepath.Join(unrelated, "account")
	if err := os.Mkdir(account, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(account, ".netrc")
	if err := os.WriteFile(marker, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, ".agentflow"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unrelated, filepath.Join(workspace, ".agentflow", "private-home")); err != nil {
		t.Fatal(err)
	}
	sb := &localSandbox{workspace: workspace}
	scrubConversationGitSecrets(session, sb)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("credential marker in unrelated directory changed", err)
	}
}

func TestScrubWorkspaceSkipsAliasedConfig(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(t.TempDir(), "canonical")
	contents := []byte("[remote \"o\"]\n\turl = https://fixture@h.example/repo\n")
	if err := os.WriteFile(canonical, contents, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".gitconfig")
	if err := os.Link(canonical, link); err != nil {
		t.Fatal(err)
	}
	if report := ScrubWorkspaceGitSecrets(t.Context(), []string{home}, ""); len(report) != 0 {
		t.Fatal("ambiguous config ownership was accepted")
	}
	a, err := os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(link)
	if err != nil || !os.SameFile(a, b) {
		t.Fatal("aliased config changed", err)
	}
}

func TestScrubGitConfigUsesPinnedDirectory(t *testing.T) {
	parent := t.TempDir()
	active, moved := filepath.Join(parent, "active"), filepath.Join(parent, "moved")
	seed := func() {
		t.Helper()
		if err := os.Mkdir(active, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(active, "config"), []byte("[remote \"origin\"]\nurl = https://fixture@example.test/r\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seed()
	root, err := os.OpenRoot(active)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(active, moved); err != nil {
		t.Fatal(err)
	}
	seed()
	if report := scrubGitConfigFile(t.Context(), root, "config"); len(report) != 1 {
		t.Fatalf("pinned config report = %v", report)
	}
	for dir, wantUserinfo := range map[string]bool{active: true, moved: false} {
		data, err := os.ReadFile(filepath.Join(dir, "config"))
		if err != nil || strings.Contains(string(data), "fixture@") != wantUserinfo {
			t.Fatalf("wrong config after directory rename: %s, err=%v", dir, err)
		}
	}
}

func TestScrubConversationCleansSessionAndWorkspaceHomes(t *testing.T) {
	session, workspace := t.TempDir(), t.TempDir()
	homes := []string{filepath.Join(session, "home"), filepath.Join(workspace, ".agentflow", "private-home", "cli")}
	for _, home := range homes {
		if err := os.MkdirAll(home, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".netrc"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := scrubConversationGitSecrets(session, &localSandbox{workspace: workspace}); len(got) != 2 {
		t.Fatalf("expected both HOME cleanup records, got %v", got)
	}
	for _, home := range homes {
		if _, err := os.Lstat(filepath.Join(home, ".netrc")); !os.IsNotExist(err) {
			t.Fatalf("local credential marker still present: %v", err)
		}
	}
}

func TestScrubGitConfigSkipsOversizedSparseFile(t *testing.T) {
	home, tools := t.TempDir(), t.TempDir()
	config := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(config, (4<<20)+1); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(tools, "invoked")
	t.Setenv("SCRUB_GIT_PROBE", probe)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(tools, "git"), []byte("#!/bin/sh\n: > \"$SCRUB_GIT_PROBE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if cleaned := ScrubWorkspaceGitSecrets(t.Context(), []string{home}, ""); len(cleaned) != 0 {
		t.Fatal("oversized config was reported as cleaned")
	}
	if _, err := os.Stat(probe); !os.IsNotExist(err) {
		t.Fatalf("oversized config reached Git: %v", err)
	}
	// The same bound applies when reading a private result or Git stdout,
	// even if the reader grows beyond its original reported size.
	file, err := os.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if data, err := readScrubConfig(file); err == nil || data != nil {
		t.Fatal("oversized result was accepted")
	}
}
