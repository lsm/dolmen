package dolmen

import "github.com/lsm/dolmen/internal/derr"

type ErrorCode = derr.Code

type Error = derr.Error

var (
	ErrInvalidRequest      = derr.ErrInvalidRequest
	ErrNotFound            = derr.ErrNotFound
	ErrQuery               = derr.ErrQuery
	ErrConflict            = derr.ErrConflict
	ErrForbidden           = derr.ErrForbidden
	ErrEmbedderUnavailable = derr.ErrEmbedderUnavailable
	ErrCanceled            = derr.ErrCanceled
	ErrInternal            = derr.ErrInternal
)
