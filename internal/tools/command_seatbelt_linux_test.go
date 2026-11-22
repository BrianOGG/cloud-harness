//go:build linux

package tools

import "os"

// On macOS the trampoline is registered by the harness itself. Linux only
// needs it so the portable Seatbelt code can be tested here.
func init() {
	if len(os.Args) > 1 && os.Args[1] == seatbeltExecArg {
		execSeatbelt(os.Args[2:])
	}
}
