package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/BrianOGG/cloud-harness/internal/repomap"
)

func cmdRepoMap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repomap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "workspace root to map")
	maxBytes := fs.Int("max-bytes", repomap.DefaultMaxBytes, "maximum rendered map size in bytes")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "repomap does not accept positional arguments")
		return 2
	}
	if *maxBytes <= 0 {
		_, _ = fmt.Fprintln(stderr, "repomap: --max-bytes must be greater than zero")
		return 2
	}
	info, err := os.Stat(*dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "repomap: --dir %q: %v\n", *dir, err)
		return 1
	}
	if !info.IsDir() {
		_, _ = fmt.Fprintf(stderr, "repomap: --dir %q is not a directory\n", *dir)
		return 1
	}

	result, err := repomap.Scan(*dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "repomap: %v\n", err)
		return 1
	}
	rendered := result.Render(*maxBytes)
	if rendered.LimitExceeded {
		_, _ = fmt.Fprintf(stderr, "repomap: --max-bytes %d is too small to include the map and truncation marker\n", *maxBytes)
		return 2
	}
	if _, err := io.WriteString(stdout, rendered.Text); err != nil {
		_, _ = fmt.Fprintf(stderr, "repomap: write output: %v\n", err)
		return 1
	}
	for _, diagnostic := range result.Diagnostics {
		_, _ = fmt.Fprintf(stderr, "repomap: skipped %s: %s\n", diagnostic.Path, diagnostic.Error)
	}
	return 0
}
