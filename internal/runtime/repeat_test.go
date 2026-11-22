package runtime

import (
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestRepeatReminderAndNoProgress(t *testing.T) {
	fp := Fingerprint("read_file", []byte(`{"path":"a"}`))
	var obs []Observation
	for i := 0; i < 2; i++ {
		obs = append(obs, Observation{Fingerprint: fp, ResultHash: "h"})
	}
	if NeedsRepeatReminder(obs) {
		t.Fatal("two calls should not warn")
	}
	obs = append(obs, Observation{Fingerprint: fp, ResultHash: "h"})
	if !NeedsRepeatReminder(obs) {
		t.Fatal("three identical calls should warn")
	}
	if Blocked(obs, fp) {
		t.Fatal("three calls should not yet block")
	}
	obs = append(obs, Observation{Fingerprint: fp, ResultHash: "h"})
	if !Blocked(obs, fp) {
		t.Fatal("four identical results should block")
	}
	if domain.DefaultRepeatWarning != 3 || domain.DefaultNoProgressLimit != 4 {
		t.Fatal("thresholds drifted")
	}
	alt := []Observation{
		{Fingerprint: "A", ResultHash: "1"},
		{Fingerprint: "B", ResultHash: "2"},
		{Fingerprint: "A", ResultHash: "1"},
		{Fingerprint: "B", ResultHash: "2"},
	}
	if !AlternatingNoProgress(alt) {
		t.Fatal("A-B-A-B with stable hashes is no progress")
	}
	alt[3].ResultHash = "changed"
	if AlternatingNoProgress(alt) {
		t.Fatal("a changed result is progress")
	}
}
