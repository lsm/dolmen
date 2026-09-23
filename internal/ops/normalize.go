package ops

import (
	"context"
	"errors"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/store"
)

func NormalizeNamespace(ns string) string {
	segs := strings.Split(ns, "/")
	for i, seg := range segs {
		segs[i] = strings.ToLower(strings.TrimSpace(seg))
	}
	return strings.Join(segs, "/")
}

func NormalizeTable(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

func Classify(err error) derr.Code {
	if err == nil {
		return derr.Internal
	}
	var de *derr.Error
	if errors.As(err, &de) {
		return de.Code
	}
	var qe *store.QueryError
	if errors.As(err, &qe) {
		if errors.Is(qe, store.ErrNotFound) {
			return derr.NotFound
		}
		return derr.Query
	}
	if errors.Is(err, store.ErrNotFound) {
		return derr.NotFound
	}
	var vce *store.VersionConflictError
	if errors.As(err, &vce) || errors.Is(err, derr.ErrConflict) {
		return derr.Conflict
	}
	if errors.Is(err, store.ErrCatalogTooNew) {
		return derr.InvalidRequest
	}
	if errors.Is(err, store.ErrInvalid) {
		return derr.InvalidRequest
	}
	var le *embed.LoadError
	if errors.As(err, &le) {
		return derr.EmbedderUnavailable
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return derr.EmbedderUnavailable
	}
	if errors.Is(err, context.Canceled) {
		return derr.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return derr.Timeout
	}
	return derr.Internal
}
