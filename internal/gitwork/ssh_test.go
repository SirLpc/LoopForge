package gitwork

import (
	"context"
	"errors"
	"testing"
)

func TestSSHContextRequiresBoundKeyForLFSFetch(t *testing.T) {
	ctx := WithSSHRemote(context.Background(), "git@example.org:team/project")
	for _, opts := range [][]string{{"-C", "/unused"}, {"-C", "/unused", "-c", "lfs.storage=lfs", "-c", "remote.origin.url=git@example.org:team/project"}} {
		_, cleanup, err := sshCommandEnvironment(ctx, append(opts, "lfs", "fetch", "origin", "HEAD"))
		if cleanup != nil {
			cleanup()
		}
		if !errors.Is(err, ErrSSHKeyRequired) {
			t.Fatalf("LFS fetch without a key: %v", err)
		}
	}
}
