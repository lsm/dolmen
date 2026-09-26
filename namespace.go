package dolmen

import (
	"context"
	"errors"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

type ListNamespacesOptions struct {
	Prefix string
}

func (s *Store) CreateNamespace(ctx context.Context, namespace string) (err error) {
	ctx, span := s.startOp(ctx, "create_namespace", namespace, "")
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return facadeErr(ctx, err)
	}
	ns := ops.NormalizeNamespace(namespace)
	return facadeErr(ctx, s.eng.CreateNamespace(ctx, ns, [16]byte{}))
}

func (s *Store) ListNamespaces(ctx context.Context, opts ListNamespacesOptions) (r0 []string, err error) {
	ctx, span := s.startOp(ctx, "list_namespaces", "", "")
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return nil, facadeErr(ctx, err)
	}
	prefix := ops.NormalizeNamespace(opts.Prefix)
	if opts.Prefix != "" && prefix == "" {
		return nil, derr.New(derr.InvalidRequest, "prefix must not be empty — omit it to list every namespace")
	}
	nss, err := s.eng.ListNamespaces(ctx, prefix, nil)
	if err != nil {
		return nil, facadeErr(ctx, err)
	}
	if nss == nil {
		nss = []string{}
	}
	return nss, nil
}

func (s *Store) DropNamespace(ctx context.Context, namespace string) (err error) {
	ctx, span := s.startOp(ctx, "drop_namespace", namespace, "")
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return facadeErr(ctx, err)
	}
	ns := ops.NormalizeNamespace(namespace)
	return facadeErr(ctx, s.eng.DropNamespace(ctx, ns, [16]byte{}))
}

func facadeErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrClosed) || errors.Is(err, store.ErrClosed) {
		return ErrClosed
	}
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &derr.Error{Code: derr.Timeout, Message: err.Error(), Cause: errors.Join(err, ctx.Err())}
		}
		return &derr.Error{
			Code:    derr.Canceled,
			Message: "the call was cancelled before it completed; the operation may or may not have finished server-side — check with a query before retrying a write",
			Cause:   errors.Join(err, ctx.Err()),
		}
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, store.ErrInvalid.Error()+": ")
	msg = strings.TrimPrefix(msg, store.ErrNotFound.Error()+": ")
	return &derr.Error{Code: ops.Classify(err), Message: msg, Cause: err}
}
