package runner

import (
	"reflect"
	"testing"
)

func TestWarmWindowsGitDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"inventory", []string{"diff", "--name-status", "HEAD", "--", "."}, []string{"--no-pager", "diff", "--no-ext-diff", "--no-textconv", "--name-status", "HEAD", "--", "."}},
		{"stage", []string{"add", "-N", "--pathspec-from-file=-", "--pathspec-file-nul"}, []string{"--no-pager", "add", "-N", "--pathspec-from-file=-", "--pathspec-file-nul"}},
		{"history", []string{"log", "-n", "20", "--format=%H"}, []string{"--no-pager", "log", "-n", "20", "--format=%H"}},
		{"fetch", []string{"fetch", "origin"}, nil},
		{"push", []string{"push", "origin", "HEAD"}, nil},
		{"external config", []string{"-c", "core.fsmonitor=evil", "status"}, nil},
		{"external diff", []string{"diff", "--ext-diff", "HEAD"}, nil},
		{"write outside checkout", []string{"diff", "--output=outside", "HEAD"}, nil},
		{"second pathspec source", []string{"add", "-N", "--pathspec-from-file=outside"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := warmWindowsGitArgs(tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Windows arguments = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWarmWindowsGitDirectoryTransition(t *testing.T) {
	if !warmWindowsDirectoryTransition([]string{"Dir", "dir/old.txt"}) {
		t.Fatal("directory replacement would run with the directory pinned")
	}
	if warmWindowsDirectoryTransition([]string{"Dir/first.txt", "dir/second.txt"}) {
		t.Fatal("ordinary changes in the same directory were treated as a directory replacement")
	}
}
