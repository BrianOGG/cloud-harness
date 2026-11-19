# M11-6 local notifications handoff

The `internal/notify` registry now supports `osc9` and Linux `desktop` channels.
Both use the existing event filters, concurrent delivery, five-second default
timeout, cancellation, and delivery error reporting. OSC 9 strips control
characters and limits its rendered body to 200 Unicode characters before writing
the terminal sequence to stderr. Desktop uses `notify-send -- <event_type> <body>`;
the body follows Bark's existing summary/fallback rendering. Non-Linux builds
reject desktop channels during construction with an `unsupported` error.

The existing webhook, Bark, and ntfy tests are unchanged. Additional tests check
their exact JSON request bytes with both summary and fallback payloads. Local
channel tests cover terminal output, sanitization, Unicode truncation, all four
event types, event filtering, config validation, fake notify-send arguments,
command failures, timeout, cancellation, and Manager failure isolation with a
healthy webhook channel. Linux integration tests also exercise the existing JSON
configuration loader, including rejection of inline credential fields.

## Required integration outside this task's allowed files

`internal/config/config.go`, in the channel credential loading loop in `Load`
(currently lines 138–148), reads environment variables before calling
`notify.New`. The existing switch only handles `webhook`, `bark`, and `ntfy` and
ignores credential environment variables for other channel types. The notify
package rejects nonempty `ChannelConfig.Credential` for `osc9`/`desktop`, but
cannot detect values the config loader never passes to it.

At merge time, add a local-channel branch to that loop which rejects any
nonempty credential environment variable for that channel index:

- `CLOUD_HARNESS_NOTIFY_CHANNEL_N_WEBHOOK_BEARER`
- `CLOUD_HARNESS_NOTIFY_CHANNEL_N_BARK_KEY`
- `CLOUD_HARNESS_NOTIFY_CHANNEL_N_NTFY_TOKEN`

Use the loader's supplied `env` function and its existing invalid-argument
error convention; never include a credential value in the error. Add the
corresponding environment-mismatch tests in `internal/config/config_test.go`.
Those two files were left untouched to respect this task's parallel ownership
boundary. No package outside `internal/notify` needs a channel type registration
change: startup validation already delegates to `notify.New`.

## Acceptance output

The complete required command output and each exit code are recorded in
`.cloud-harness/m11-6/acceptance-final.log`.

All five required checks passed on Linux/arm64: `go build ./...`,
`go vet ./...`, `gofmt -l .` (no output), `go test -p 1 ./...`, and the specified
seven-package race test with `CGO_ENABLED=1`. Each command exited with code 0.
The notify test package also cross-compiled successfully for Darwin/arm64 and
Windows/arm64; those platform test binaries were not executed on the Linux host.
