package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Run once, before starting the opt-in daemon or any of its Docker consumers.
// Let the installed CLI resolve DOCKER_HOST, DOCKER_CONTEXT and its config.
func pinWarmDockerEndpoint(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", `{{ (index .Endpoints "docker").Host }}`).Output()
	if err != nil {
		return fmt.Errorf("resolve Docker endpoint: %w", err)
	}
	endpoint := strings.TrimSpace(string(out))
	path, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("warm workspaces require a local Docker Unix socket")
	}
	if err := os.Setenv("DOCKER_HOST", endpoint); err != nil {
		return err
	}
	return os.Unsetenv("DOCKER_CONTEXT")
}
