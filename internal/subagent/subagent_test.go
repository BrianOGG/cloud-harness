package subagent

import (
	"context"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func TestDisabledDoesNotDelegate(t *testing.T) {
	err := Disabled{}.Delegate(context.Background(), Request{ParentRunID: "run", Goal: "investigate"})
	if !domain.IsCode(err, domain.CodeNotImplemented) {
		t.Fatal(err)
	}
}
