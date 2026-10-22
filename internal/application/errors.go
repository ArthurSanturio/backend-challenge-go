package application

import "errors"

var (
	ErrInboxConflict       = errors.New("message identity was reused with different content")
	ErrInvalidInput        = errors.New("invalid use case input")
	ErrInvalidDependencies = errors.New("use case dependencies are invalid")
	ErrNotFound            = errors.New("record not found")
	ErrConflict            = errors.New("record or idempotency conflict")
	ErrUnauthenticated     = errors.New("authenticated actor is required")
	ErrForbidden           = errors.New("actor is not allowed to perform this action")
)
