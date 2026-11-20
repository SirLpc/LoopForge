package runner

import (
	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Heartbeats read the idle checkout structure, index header and origin. Full
// index, refs and worktree verification runs under the exclusive admission fence.
func warmSlotOrigin(slot warmWorkspaceSlot, record warmWorkspaceSlotRecord, journal string) string {
	identity, exists, err := warmWorkspaceEntry(slot.Path, record.Parent)
	if err != nil || !exists || identity != record.Identity {
		return ""
	}
	meta := filepath.Join(slot.Path, ".git")
	if project, err := warmSeedIdentity(filepath.Join(slot.Path, slot.UnityProjectPath)); err != nil || project.Dev != identity.Dev {
		return ""
	}
	for _, dir := range []string{"objects", "refs"} {
		f, err := openWarmWorkspaceDir(filepath.Join(meta, dir), false)
		if err != nil {
			return ""
		}
		f.Close()
	}
	if err := checkWarmPendingGitState(meta); err != nil {
		return ""
	}
	config, index := filepath.Join(meta, "config"), filepath.Join(meta, "index")
	if warmRegular(config) != nil || warmRegular(index) != nil {
		return ""
	}
	if _, err = warmCurrentHead(meta); err != nil {
		return ""
	}
	f, err := os.Open(index)
	if err != nil {
		return ""
	}
	var header [12]byte
	_, err = io.ReadFull(f, header[:])
	f.Close()
	version := binary.BigEndian.Uint32(header[4:8])
	if err != nil || string(header[:4]) != "DIRC" || version < 2 || version > 4 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	origin, err := gitwork.C(ctx, journal, "config", "--no-includes", "--file", config, "--get-all", "remote.origin.url")
	if err != nil || domain.URLHasCredentials(origin) || gitwork.ValidateRemoteURL(origin, true) != nil {
		return ""
	}
	return gitwork.RemoteFingerprint(origin)
}
