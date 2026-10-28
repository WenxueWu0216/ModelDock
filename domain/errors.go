package domain

import "errors"

// Sentinel errors. Callers wrap them with context (fmt.Errorf("...: %w"));
// the API layer maps them to HTTP status codes with errors.Is.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrInvalid      = errors.New("invalid argument")
	ErrForbidden    = errors.New("forbidden")
	ErrUnauthorized = errors.New("unauthorized")
)
