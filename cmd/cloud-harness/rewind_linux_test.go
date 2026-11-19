//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func rewindTestTTY(t *testing.T, input string) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	index, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", index), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	if _, err := master.WriteString(input); err != nil {
		t.Fatal(err)
	}
	return slave
}

func TestRewindTTYConfirmation(t *testing.T) {
	for _, test := range []struct {
		name     string
		input    string
		accepted bool
	}{
		{"default no", "\n", false},
		{"explicit no", "n\n", false},
		{"invalid", "maybe\n", false},
		{"yes", "y\n", true},
		{"uppercase yes", "YES\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRewindFixture(t)
			stdin := rewindTestTTY(t, test.input)
			if !readerIsTTY(stdin) {
				t.Fatal("PTY not recognized as TTY")
			}
			code, out, stderr := f.invoke(t, stdin, "--all")
			if code != 0 || !strings.Contains(out, "[y/N]") {
				t.Fatalf("confirmation exit=%d: %s; %s", code, out, stderr)
			}
			f.assertFiles(t, test.accepted)
			f.assertChat(t, test.accepted)
			wantRecords := 2
			if test.accepted {
				wantRecords = 3
			} else if !strings.Contains(out, "Rewind cancelled") {
				t.Fatalf("cancellation missing: %s", out)
			}
			if got := len(f.records(t)); got != wantRecords {
				t.Fatalf("snapshots=%d, want %d", got, wantRecords)
			}
		})
	}
}
