//go:build !linux

package notify

import (
	"fmt"
	"runtime"
)

func newDesktop() (Channel, error) {
	return nil, fmt.Errorf("desktop notifications unsupported on %s", runtime.GOOS)
}
