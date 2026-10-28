package model

import (
	"errors"
	"fmt"
)

// Sentinel errors for classification.
var (
	ErrNotFound   = errors.New("not found")
	ErrValidation = errors.New("validation error")
	ErrConflict   = errors.New("conflict")
	ErrDatabase   = errors.New("database error")

	// Outcomes of a command that wrote what it could but did not finish
	// every item: a `crm run` / `crm qualify` (the run and its checkpoints
	// are saved, resumable) or a `crm table import` with skipped rows
	// (ErrItemsFailed). The error only sets the exit status.
	ErrBudgetExhausted = errors.New("budget exhausted")
	ErrItemsFailed     = errors.New("items failed")
	ErrInterrupted     = errors.New("interrupted")
)

// ExitError wraps a sentinel with a user-facing message.
type ExitError struct {
	Message string
	Err     error
}

func (e *ExitError) Error() string { return e.Message }
func (e *ExitError) Unwrap() error { return e.Err }

// NewExitError creates an ExitError wrapping a sentinel.
func NewExitError(sentinel error, format string, args ...any) *ExitError {
	return &ExitError{
		Message: fmt.Sprintf(format, args...),
		Err:     sentinel,
	}
}

// ExitCode maps an error to a CLI exit code. A process stopped by a signal
// exits 128+n instead (cmd/crm/main.go).
func ExitCode(err error) int {
	switch {
	case errors.Is(err, ErrValidation):
		return 2
	case errors.Is(err, ErrNotFound):
		return 3
	case errors.Is(err, ErrConflict):
		return 4
	case errors.Is(err, ErrBudgetExhausted):
		return 5
	case errors.Is(err, ErrItemsFailed):
		return 6
	case errors.Is(err, ErrInterrupted):
		return 7
	case errors.Is(err, ErrDatabase):
		return 10
	default:
		return 1
	}
}
