package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionAndMockRun(t *testing.T) {
	var out, err bytes.Buffer
	if code := Main([]string{"version"}, &out, &err); code != 0 || !strings.Contains(out.String(), "0.5.0-m5") {
		t.Fatalf("version %d %s %s", code, out.String(), err.String())
	}
	out.Reset()
	db := t.TempDir() + "/h.db"
	code := Main([]string{"run", "--db", db, "--provider", "mock", "--message", "status"}, &out, &err)
	if code != 0 {
		t.Fatalf("run %d %s %s", code, out.String(), err.String())
	}
	if !strings.Contains(out.String(), "Mock answer: status") || !strings.Contains(out.String(), "COMPLETED") {
		t.Fatalf("output %s", out.String())
	}
}

func TestResumeFlagOrder(t *testing.T) {
	var out, err bytes.Buffer
	db := t.TempDir() + "/h.db"
	code := Main([]string{"runs", "resume", "run_missing", "--decision", "deny", "--db", db}, &out, &err)
	if code != 2 || !strings.Contains(err.String(), "run id required") {
		t.Fatalf("id-first %d %s %s", code, out.String(), err.String())
	}
	out.Reset()
	err.Reset()
	code = Main([]string{"runs", "resume", "--decision", "deny", "--db", db, "run_missing"}, &out, &err)
	if code == 2 || strings.Contains(err.String(), "run id required") {
		t.Fatalf("flags-first %d %s %s", code, out.String(), err.String())
	}
}

func TestExternalBindRefused(t *testing.T) {
	var out, err bytes.Buffer
	code := Main([]string{"serve", "--addr", "0.0.0.0:19421", "--db", t.TempDir() + "/h.db"}, &out, &err)
	if code == 0 || !strings.Contains(err.String(), "refusing") {
		t.Fatalf("serve %d %s %s", code, out.String(), err.String())
	}
}
