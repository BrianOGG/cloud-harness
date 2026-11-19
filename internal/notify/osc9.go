package notify

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"
)

type osc9 struct{}

func (*osc9) Send(ctx context.Context, e Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(os.Stderr, "\x1b]9;%s\x07", sanitizeOSC9(notificationBody(e)))
	return err
}

func sanitizeOSC9(text string) string {
	var clean strings.Builder
	count := 0
	for _, r := range text {
		// This also removes ESC, BEL and the C1 terminal control characters.
		if unicode.IsControl(r) {
			continue
		}
		if count == 200 {
			break
		}
		clean.WriteRune(r)
		count++
	}
	return clean.String()
}
