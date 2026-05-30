package main

import (
	"context"
	"fmt"
	"os"
)

// startupSelfCheck fails-loud at boot if anything load-bearing isn't
// reachable: data dirs, LLM gateway, Affine MCP. Better to exit(1) than
// to silently DLQ every note that arrives.
func startupSelfCheck(ctx context.Context, cfg *config, hwr *hwrClient, aff *affineClient) error {
	for _, d := range []string{cfg.InboxDir(), cfg.ArchiveDir(), cfg.DLQDir(), cfg.StateDir(), cfg.RendersDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	if err := hwr.healthCheck(ctx); err != nil {
		return fmt.Errorf("llm gateway: %w", err)
	}
	if err := aff.healthCheck(ctx); err != nil {
		return fmt.Errorf("affine mcp: %w", err)
	}
	return nil
}
