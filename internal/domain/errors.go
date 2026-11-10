package domain

import "errors"

// Error is a diagnostic failure with a stable code. Ref is an optional
// id of the run, invocation, or response the caller can look up.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Ref     string `json:"ref,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Ref != "" {
		return e.Code + ": " + e.Message + " (" + e.Ref + ")"
	}
	return e.Code + ": " + e.Message
}

// E builds a domain error. The message is for a human; the code is stable.
func E(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WithRef returns a copy that names the related record.
func (e *Error) WithRef(ref string) *Error {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Ref = ref
	return &cp
}

// IsCode reports whether err is a domain error with the given code.
func IsCode(err error, code string) bool {
	var de *Error
	if errors.As(err, &de) {
		return de.Code == code
	}
	return false
}

const (
	CodeProtocol          = "protocol_error"
	CodeCorrupt           = "corrupt_state"
	CodeIllegalTransition = "illegal_transition"
	CodeBudget            = "budget_exhausted"
	CodeAuthDenied        = "auth_denied"
	CodeSkipped           = "skipped"
	CodeProviderConfig    = "provider_not_configured"
	CodeProviderHTTP      = "provider_http"
	CodeOpaqueReplay      = "opaque_replay_unsupported"
	CodeProviderMismatch  = "provider_mismatch"
	CodeProgramDisabled   = "program_disabled"
	CodeScriptSyntax      = "script_syntax"
	CodeScriptError       = "script_error"
	CodeScriptPolicy      = "script_policy"
	CodeResourceLimit     = "resource_limit"
	CodeWorkerCrashed     = "worker_crashed"
	CodeToolError         = "tool_error"
	CodeInvalidArgument   = "invalid_argument"
	CodeNotFound          = "not_found"
	CodeConflict          = "conflict"
	CodeLease             = "lease_conflict"
	CodeNotWaiting        = "not_waiting"
	CodeNotImplemented    = "not_implemented"
	CodeExternalBind      = "external_bind_refused"
	CodeCancelled         = "cancelled"
)
