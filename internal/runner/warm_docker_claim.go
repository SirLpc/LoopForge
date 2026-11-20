package runner

import (
	"context"
	"fmt"

	"agentflow/internal/domain"
)

func (c *Client) taskClaimPath() string {
	if c.warmOnly {
		return domain.WarmWorkspaceClaimPath
	}
	return "/runners/tasks/claim"
}

func (d *Daemon) checkDedicatedWarmClaim(ctx context.Context) error {
	if !d.client.warmOnly {
		return nil
	}
	_, task, err := d.client.WaitForTask(ctx, ClaimRequest{AvailableRuntimes: []string{"terminal"}, NoWait: true})
	if err != nil {
		return fmt.Errorf("control plane must support the dedicated warm Runner claim endpoint: %w", err)
	}
	if task != nil {
		return fmt.Errorf("dedicated warm Runner preflight unexpectedly leased a task")
	}
	return nil
}
