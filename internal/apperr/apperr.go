// Package apperr defines the single error model returned across HTTP boundaries.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is the only error shape sent to clients: an HTTP status, a stable machine code and a
// human message. The wrapped cause is logged but never serialized.
type Error struct {
	Status  int
	Code    string
	Message string
	cause   error
}

// Error formats code, message and cause for logs.
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return e.Code + ": " + e.Message
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// Wrap keeps the cause for logs; it is never serialized to clients.
func (e *Error) Wrap(cause error) *Error {
	c := *e
	c.cause = cause
	return &c
}

// New returns an error with the given HTTP status, machine code and message.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

var (
	ErrBadRequest       = New(http.StatusBadRequest, "bad_request", "invalid request")
	ErrUnauthenticated  = New(http.StatusUnauthorized, "unauthenticated", "authentication required")
	ErrForbidden        = New(http.StatusForbidden, "forbidden", "permission denied")
	ErrNotFound         = New(http.StatusNotFound, "not_found", "resource not found")
	ErrConflict         = New(http.StatusConflict, "conflict", "resource state conflict")
	ErrTooLarge         = New(http.StatusRequestEntityTooLarge, "too_large", "request body too large")
	ErrUnsupportedMedia = New(http.StatusUnsupportedMediaType, "unsupported_media_type", "content type must be application/json")
	ErrRateLimited      = New(http.StatusTooManyRequests, "rate_limited", "too many requests")
	ErrInternal         = New(http.StatusInternalServerError, "internal", "internal error")
	ErrUnavailable      = New(http.StatusServiceUnavailable, "unavailable", "service unavailable")
)

// BadRequest is a 400 with code "bad_request" and the given message.
func BadRequest(message string) *Error {
	return New(http.StatusBadRequest, "bad_request", message)
}

// From maps any error to an *Error; unknown errors become ErrInternal so details never leak.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return ErrInternal.Wrap(err)
}
