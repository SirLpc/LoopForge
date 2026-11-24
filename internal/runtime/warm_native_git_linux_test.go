package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeWarmGitCancellationClosesHelperStreams(t *testing.T) {
	workspace := t.TempDir()
	launcher := filepath.Join(workspace, "bwrap")
	// A descendant retains stdout after the launcher is killed.
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\n/bin/sleep 3 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := RunNativeWarmGitSession(ctx, workspace, launcher, false, false, nil,
		func(stdout io.Reader, _ io.Writer) ([]byte, error) { return io.ReadAll(stdout) })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("helper cancellation took %s and returned %v", time.Since(start), err)
	}
}
