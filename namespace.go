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
		return facadeErr(err)
	}
	ns := ops.NormalizeNamespace(namespace)
	return facadeErr(s.eng.CreateNamespace(ctx, ns, [16]byte{}))
}

func (s *Store) ListNamespaces(ctx context.Context, opts ListNamespacesOptions) (r0 []string, err error) {
	ctx, span := s.startOp(ctx, "list_namespaces", "", "")
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return nil, facadeErr(err)
	}
	prefix := ops.NormalizeNamespace(opts.Prefix)
	if opts.Prefix != "" && prefix == "" {
		return nil, derr.New(derr.InvalidRequest, "prefix must not be empty — omit it to list every namespace")
	}
	nss, err := s.eng.ListNamespaces(ctx, prefix, nil)
	if err != nil {
		return nil, facadeErr(err)
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
		return facadeErr(err)
	}
	ns := ops.NormalizeNamespace(namespace)
	return facadeErr(s.eng.DropNamespace(ctx, ns, [16]byte{}))
}

func facadeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrClosed) || errors.Is(err, store.ErrClosed) {
		return ErrClosed
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, store.ErrInvalid.Error()+": ")
	msg = strings.TrimPrefix(msg, store.ErrNotFound.Error()+": ")
	return &derr.Error{Code: ops.Classify(err), Message: msg, Cause: err}
}
