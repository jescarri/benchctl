package domain

import (
	"errors"
	"fmt"
)

// Kind classifies errors so the CLI can map them to stable exit codes.
type Kind int

const (
	KindInstrument  Kind = 1 // instrument reported an error, or SCPI protocol failure
	KindUsage       Kind = 2 // bad flags/arguments/config
	KindUnreachable Kind = 3 // instrument not reachable / not found
	KindVerify      Kind = 4 // a verification or assertion failed
)

func (k Kind) String() string {
	switch k {
	case KindInstrument:
		return "instrument"
	case KindUsage:
		return "usage"
	case KindUnreachable:
		return "unreachable"
	case KindVerify:
		return "verify"
	}
	return "unknown"
}

// Error is a classified error.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func newErr(k Kind, format string, args ...any) error {
	return &Error{Kind: k, Err: fmt.Errorf(format, args...)}
}

// Usagef returns a usage error.
func Usagef(format string, args ...any) error { return newErr(KindUsage, format, args...) }

// Instrumentf returns an instrument error.
func Instrumentf(format string, args ...any) error { return newErr(KindInstrument, format, args...) }

// Unreachablef returns an unreachable error.
func Unreachablef(format string, args ...any) error { return newErr(KindUnreachable, format, args...) }

// Verifyf returns a verification error.
func Verifyf(format string, args ...any) error { return newErr(KindVerify, format, args...) }

// KindOf returns the error's kind; unclassified errors are instrument errors.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInstrument
}

// ExitCode maps an error to the CLI exit code (0 for nil).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return int(KindOf(err))
}
