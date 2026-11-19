//go:build linux

package notify

import (
	"context"
	"fmt"
	"os/exec"
)

type desktop struct{}

func newDesktop() (Channel, error) { return &desktop{}, nil }

func (*desktop) Send(ctx context.Context, e Event) error {
	// End option parsing so even a title or body beginning with '-' is literal.
	if err := exec.CommandContext(ctx, "notify-send", "--", e.EventType, notificationBody(e)).Run(); err != nil {
		return fmt.Errorf("notify-send failed: %w", err)
	}
	return nil
}
