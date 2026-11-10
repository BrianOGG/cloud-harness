package program

import (
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func TestSafeReplayRequiresReadonlyIdempotentTools(t *testing.T) {
	if !SafeReplay(nil) {
		t.Fatal("pure script was refused")
	}
	if !SafeReplay([]ToolFact{{Name: "read_file", Readonly: true, Idempotent: true, SideEffect: "none"}}) {
		t.Fatal("readonly tool was refused")
	}
	if SafeReplay([]ToolFact{{Name: "write_file", Readonly: false, Idempotent: true, SideEffect: "reversible"}}) {
		t.Fatal("side-effect tool was replayable")
	}
}

func TestReplayRunsASafeCrashOnceMore(t *testing.T) {
	n := 0
	res, err := Replay(true, func() (Result, error) {
		n++
		if n == 1 {
			return Result{}, domain.E(domain.CodeWorkerCrashed, "worker killed")
		}
		return Result{OK: true, Data: []byte(`{"ok":true}`)}, nil
	})
	if err != nil || !res.OK || n != 2 {
		t.Fatalf("n=%d res=%+v err=%v", n, res, err)
	}
}

func TestReplayDoesNotRepeatAnUnsafeCrash(t *testing.T) {
	n := 0
	_, err := Replay(false, func() (Result, error) {
		n++
		return Result{}, domain.E(domain.CodeWorkerCrashed, "worker killed")
	})
	if n != 1 || !domain.IsCode(err, domain.CodeWorkerCrashed) {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
