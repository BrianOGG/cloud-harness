package hooks

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func timeDurationMS(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }

// The shell never inherits PATH, startup scripts, proxies or credentials from
// the parent. GOROOT/bin also supports installations outside /usr/local/go.
func commandEnv() []string {
	path := filepath.Join(runtime.GOROOT(), "bin") + string(os.PathListSeparator) + "/usr/local/bin:/usr/bin:/bin"
	return []string{"PATH=" + path, "LANG=C", "TMPDIR=" + os.TempDir()}
}

// Continue draining after the cap, so a verbose child cannot deadlock or grow
// the parent heap. Extra headroom allows redaction before the final 4 KiB cap.
type cappedBuffer struct{ data []byte }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := (64 << 10) - len(b.data); remaining > 0 {
		b.data = append(b.data, p[:min(remaining, len(p))]...)
	}
	return n, nil
}

func runProcess(ctx context.Context, dir string, env []string, stdin, name string, args ...string) Event {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(stdin)
	var stdout, stderr cappedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	configureProcess(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	cleanupProcess(cmd)
	event := Event{Stdout: string(stdout.data), Stderr: string(stderr.data)}
	if err != nil {
		event.ExitCode = 1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			event.ExitCode = exit.ExitCode()
		} else {
			event.Stderr += "\n" + err.Error()
		}
	}
	return event
}
