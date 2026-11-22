package program

import "github.com/BrianOGG/cloud-harness/internal/domain"

// ToolFact is the slice of a tool spec that decides whether a crashed script
// may run again. A side effect, or a tool that is not readonly and idempotent,
// is not safe to replay.
type ToolFact struct {
	Name       string
	Readonly   bool
	Idempotent bool
	SideEffect string
}

// SafeReplay is true only when every declared tool can be repeated without a
// new side effect. An empty allowlist is pure computation and is safe.
func SafeReplay(tools []ToolFact) bool {
	for _, tool := range tools {
		if !tool.Readonly || !tool.Idempotent || tool.SideEffect != "none" {
			return false
		}
	}
	return true
}

// Attempt runs a script once.
type Attempt func() (Result, error)

// Replay runs a crashed script one more time only when safe is true.
// Any other error, including a crash of an unsafe script, is returned as-is
// and the attempt function is not called again.
func Replay(safe bool, run Attempt) (Result, error) {
	res, err := run()
	if err == nil || !safe || !domain.IsCode(err, domain.CodeWorkerCrashed) {
		return res, err
	}
	return run()
}
