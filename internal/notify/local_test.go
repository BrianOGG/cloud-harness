package notify

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOSC9FormatAndSanitization(t *testing.T) {
	for _, tt := range []struct {
		name, summary, want string
	}{
		{"summary", "Review go test ./...", "Review go test ./..."},
		{"fallback", "", "approval WAITING"},
		{"controls", "a\x00\x01\t\n\r\x1b]9;injected\x07\x7f\u0085\u009b\u009c\u009db", "a]9;injectedb"},
		{"long ASCII", strings.Repeat("a", 201), strings.Repeat("a", 200)},
		{"long Unicode", strings.Repeat("通知", 110), strings.Repeat("通知", 100)},
		{"strip before truncating", strings.Repeat("界", 199) + "\n\x1b\x07尾多", strings.Repeat("界", 199) + "尾"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := captureStderr(t, func() {
				n, err := New(Config{Channels: []ChannelConfig{{Type: "osc9"}}})
				if err != nil {
					t.Fatal(err)
				}
				for err := range n.Notify(context.Background(), Event{EventType: EventWaitingApproval, Status: "WAITING", Summary: tt.summary}) {
					t.Fatalf("delivery: %v", err)
				}
			})
			if want := "\x1b]9;" + tt.want + "\x07"; got != want {
				t.Fatalf("output=%q, want %q", got, want)
			}
		})
	}
}

func TestOSC9CancellationAndWriteFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := captureStderr(t, func() {
		if err := (&osc9{}).Send(ctx, Event{Summary: "discard"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Send error=%v, want cancellation", err)
		}
	})
	if got != "" {
		t.Fatalf("canceled notification wrote %q", got)
	}

	captureStderr(t, func() {
		if err := os.Stderr.Close(); err != nil {
			t.Fatal(err)
		}
		if err := (&osc9{}).Send(context.Background(), Event{Summary: "failure"}); err == nil {
			t.Fatal("write failure was ignored")
		}
	})
}

func TestOSC9LifecycleEventsAndFilter(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		cfg := ChannelConfig{Type: "osc9"}
		if filtered {
			cfg.Events = []string{EventCompleted}
		}
		n, err := New(Config{Channels: []ChannelConfig{cfg}})
		if err != nil {
			t.Fatal(err)
		}
		for _, eventType := range []string{EventWaitingApproval, EventAskUser, EventCompleted, EventFailed} {
			got := captureStderr(t, func() {
				for err := range n.Notify(context.Background(), Event{EventType: eventType, Summary: eventType}) {
					t.Fatal(err)
				}
			})
			want := "\x1b]9;" + eventType + "\x07"
			if filtered && eventType != EventCompleted {
				want = ""
			}
			if got != want {
				t.Fatalf("filtered=%v event=%s output=%q, want %q", filtered, eventType, got, want)
			}
		}
	}
}

func TestLocalChannelConfigValidation(t *testing.T) {
	for _, typ := range []string{"osc9", "desktop"} {
		t.Run(typ, func(t *testing.T) {
			for _, tt := range []struct {
				name, want string
				cfg        ChannelConfig
			}{
				{"url", "does not accept url", ChannelConfig{Type: typ, URL: "https://example.com/secret"}},
				{"credentials", "does not accept credentials", ChannelConfig{Type: typ, Credential: "test-secret"}},
				{"negative timeout", "timeout_ms must not be negative", ChannelConfig{Type: typ, TimeoutMS: -1}},
			} {
				t.Run(tt.name, func(t *testing.T) {
					_, err := New(Config{Channels: []ChannelConfig{tt.cfg}})
					if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "secret") {
						t.Fatalf("error=%v, want %q without secret values", err, tt.want)
					}
				})
			}
			if typ == "desktop" && runtime.GOOS != "linux" {
				_, err := New(Config{Channels: []ChannelConfig{{Type: typ}}})
				if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), runtime.GOOS) {
					t.Fatalf("non-Linux desktop error=%v", err)
				}
				return
			}
			_, err := New(Config{Channels: []ChannelConfig{{Type: typ, Events: []string{"unknown-event"}}}})
			if err == nil || !strings.Contains(err.Error(), `unknown event "unknown-event"`) {
				t.Fatalf("unknown event error=%v", err)
			}
			for _, timeout := range []int{0, 25} {
				n, err := New(Config{Channels: []ChannelConfig{{Type: " " + strings.ToUpper(typ) + " ", TimeoutMS: timeout, Events: []string{" APPROVAL ", "ASK_USER", "completion", "failure"}}}})
				if err != nil {
					t.Fatal(err)
				}
				ch := n.(*notifier).channels[0]
				wantTimeout := time.Duration(timeout) * time.Millisecond
				if timeout == 0 {
					wantTimeout = 5 * time.Second
				}
				if ch.timeout != wantTimeout || ch.target != "" || ch.name != typ || len(ch.events) != 4 {
					t.Fatalf("configured channel=%+v, want timeout=%s", ch, wantTimeout)
				}
			}
		})
	}
}

func captureStderr(t *testing.T, send func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = f
	defer func() {
		os.Stderr = previous
		_ = f.Close()
	}()
	send()
	// Read by path so a test can also exercise a closed stderr file.
	r, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
