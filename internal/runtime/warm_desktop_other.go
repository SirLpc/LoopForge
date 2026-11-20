//go:build !linux

package runtime

import (
	"context"
	"errors"
)

type WarmDockerDesktop struct{}

var errWarmDesktopOS = errors.New("Docker Desktop WSL2 warm workspaces require a Linux runner")

func OpenWarmDockerDesktop(context.Context, string, string) (*WarmDockerDesktop, error) {
	return nil, errWarmDesktopOS
}
func (*WarmDockerDesktop) Backing() string                     { return "" }
func (*WarmDockerDesktop) Ready(context.Context) error         { return errWarmDesktopOS }
func (*WarmDockerDesktop) Fence(context.Context, string) error { return errWarmDesktopOS }
func RunWarmDockerProbeCLI(args []string) bool                 { return false }

func (*WarmDockerDesktop) SetConversationScope(string, func(string) ([]string, bool)) error {
	return errWarmDesktopOS
}
