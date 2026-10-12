package cometgo

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// Stable error codes (PROTOCOL.md §9). Application codes use the "app." prefix.
const (
	CodeBadRequest      = "BAD_REQUEST"
	CodeValidation      = "VALIDATION"
	CodeUnauthenticated = "UNAUTHENTICATED"
	CodeAuthExpired     = "AUTH_EXPIRED"
	CodeForbidden       = "FORBIDDEN"
	CodeNotFound        = "NOT_FOUND"
	CodeConflict        = "CONFLICT"
	CodeRateLimited     = "RATE_LIMITED"
	CodeTimeout         = "TIMEOUT"
	CodeCanceled        = "CANCELED"
	CodeResultTooLarge  = "RESULT_TOO_LARGE"
	CodeOverloaded      = "OVERLOADED"
	CodeUnavailable     = "UNAVAILABLE"
	CodeInternal        = "INTERNAL"
)

// Error is a structured error sent to clients. Return one from a method or
// publication to control what the client sees; any other error is reported
// as INTERNAL with a log correlation ref.
type Error struct {
	Code         string `json:"code"`
	Msg          string `json:"msg"`
	Details      any    `json:"details,omitempty"`
	RetryAfterMs int64  `json:"retryAfterMs,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

// NewError returns an *Error with the given code and message.
func NewError(code, msg string) *Error { return &Error{Code: code, Msg: msg} }

// ValidationError returns a VALIDATION error carrying per-field messages.
func ValidationError(fields map[string]string) *Error {
	return &Error{Code: CodeValidation, Msg: "invalid arguments", Details: map[string]any{"fields": fields}}
}

// ErrForbidden is a convenience FORBIDDEN error.
var ErrForbidden = NewError(CodeForbidden, "forbidden")

// toWireError converts any error into a client-safe *Error. Non-*Error values
// are logged with a ref and never leak their message.
func (a *App) toWireError(err error, what string) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	ref := newID(6)
	a.log.Error("internal error", "ref", ref, "in", what, "err", err)
	return &Error{Code: CodeInternal, Msg: "internal error", Details: map[string]any{"ref": ref}}
}

func newID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("cometgo: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}
