package agent

import (
	"os"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/program"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		program.ServeWorker(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
