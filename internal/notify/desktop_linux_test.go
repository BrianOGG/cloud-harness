package notify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDesktopNotifySendArguments(t *testing.T) {
	argsPath := fakeNotifySend(t, `printf '%s\000' "$@" >> "$M11_6_NOTIFY_ARGS"`)
	for _, tt := range []struct {
		name, summary, want string
	}{
		{"summary", "Review \"go test\"; $(touch unwanted)\nnext line", "Review \"go test\"; $(touch unwanted)\nnext line"},
		{"fallback", "", "approval WAITING"},
		{"option-like body", "--help", "--help"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(argsPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			n, err := New(Config{Channels: []ChannelConfig{{Type: "desktop"}}})
			if err != nil {
				t.Fatal(err)
			}
			for err := range n.Notify(context.Background(), Event{EventType: EventWaitingApproval, Status: "WAITING", Summary: tt.summary}) {
				t.Fatal(err)
			}
			if got, want := readNotifyArgs(t, argsPath), []string{"--", EventWaitingApproval, tt.want}; !reflect.DeepEqual(got, want) {
				t.Fatalf("notify-send args=%q, want %q", got, want)
			}
		})
	}
}

func TestDesktopLifecycleEventsAndFilter(t *testing.T) {
	argsPath := fakeNotifySend(t, `printf '%s\000' "$@" >> "$M11_6_NOTIFY_ARGS"`)
	for _, filtered := range []bool{false, true} {
		cfg := ChannelConfig{Type: "desktop"}
		if filtered {
			cfg.Events = []string{EventCompleted}
		}
		n, err := New(Config{Channels: []ChannelConfig{cfg}})
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, eventType := range []string{EventWaitingApproval, EventAskUser, EventCompleted, EventFailed} {
			for err := range n.Notify(context.Background(), Event{EventType: eventType, Summary: eventType}) {
				t.Fatal(err)
			}
			if !filtered || eventType == EventCompleted {
				want = append(want, "--", eventType, eventType)
			}
		}
		if got := readNotifyArgs(t, argsPath); !reflect.DeepEqual(got, want) {
			t.Fatalf("filtered=%v args=%q, want %q", filtered, got, want)
		}
		if err := os.WriteFile(argsPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDesktopMissingCommandAndExitFailure(t *testing.T) {
	t.Run("missing command fails on send", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		n, err := New(Config{Channels: []ChannelConfig{{Type: "desktop"}}})
		if err != nil {
			t.Fatalf("construction should not require notify-send: %v", err)
		}
		var failures []error
		for err := range n.Notify(context.Background(), Event{EventType: EventCompleted}) {
			failures = append(failures, err)
		}
		var de DeliveryError
		if len(failures) != 1 || !errors.As(failures[0], &de) || !errors.Is(de.Err, exec.ErrNotFound) {
			t.Fatalf("delivery errors=%v, want one missing command error", failures)
		}
	})
	t.Run("failed command is not retried", func(t *testing.T) {
		argsPath := fakeNotifySend(t, "printf 'call\\n' >> \"$M11_6_NOTIFY_ARGS\"\nexit 7")
		n, err := New(Config{Channels: []ChannelConfig{{Type: "desktop"}}})
		if err != nil {
			t.Fatal(err)
		}
		failures := 0
		for err := range n.Notify(context.Background(), Event{EventType: EventCompleted}) {
			var de DeliveryError
			if !errors.As(err, &de) || de.Channel != "desktop" || de.Target != "" || !strings.Contains(err.Error(), "exit status 7") {
				t.Fatalf("delivery error=%v", err)
			}
			failures++
		}
		body, err := os.ReadFile(argsPath)
		if err != nil || string(body) != "call\n" || failures != 1 {
			t.Fatalf("calls=%q errors=%d read error=%v", body, failures, err)
		}
	})
}

func TestDesktopTimeoutAndCancellation(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("M11_6_NOTIFY_SLEEP", sleep)
	for _, canceled := range []bool{false, true} {
		name := "timeout"
		if canceled {
			name = "run cancellation discards pending delivery"
		}
		t.Run(name, func(t *testing.T) {
			argsPath := fakeNotifySend(t, "printf 'started' > \"$M11_6_NOTIFY_ARGS\"\nexec \"$M11_6_NOTIFY_SLEEP\" 30")
			cfg := ChannelConfig{Type: "desktop", TimeoutMS: 100}
			if canceled {
				cfg.TimeoutMS = 5000
			}
			n, err := New(Config{Channels: []ChannelConfig{cfg}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			results := n.Notify(ctx, Event{EventType: EventCompleted})
			if canceled {
				deadline := time.Now().Add(2 * time.Second)
				for {
					if body, _ := os.ReadFile(argsPath); string(body) == "started" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("notify-send did not start")
					}
					time.Sleep(5 * time.Millisecond)
				}
				cancel()
			}
			select {
			case err, ok := <-results:
				if canceled {
					if ok {
						t.Fatalf("canceled notification reported error: %v", err)
					}
				} else if !ok || err == nil {
					t.Fatalf("timed out notification did not report an error: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("notification did not stop after timeout/cancellation")
			}
			for err := range results {
				t.Fatalf("unexpected extra delivery error: %v", err)
			}
		})
	}
}

func fakeNotifySend(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "notify-send"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("M11_6_NOTIFY_ARGS", argsPath)
	return argsPath
}

func readNotifyArgs(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\x00"), "\x00")
}
