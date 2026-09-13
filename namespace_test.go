package dolmen

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNamespaceLifecycle(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.CreateNamespace(ctx, "App/Sub"); err != nil {
		t.Fatalf("create: %v", err)
	}
	nss, err := st.ListNamespaces(ctx, ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 1 || nss[0] != "app/sub" {
		t.Fatalf("expected normalized app/sub, got %v", nss)
	}
	sub, err := st.ListNamespaces(ctx, ListNamespacesOptions{Prefix: "app"})
	if err != nil {
		t.Fatalf("list prefix: %v", err)
	}
	if len(sub) != 1 || sub[0] != "app/sub" {
		t.Fatalf("prefix must include the subtree, got %v", sub)
	}
	if err := st.DropNamespace(ctx, "app/sub"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	nss, err = st.ListNamespaces(ctx, ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list after drop: %v", err)
	}
	if len(nss) != 0 {
		t.Fatalf("expected empty listing after drop, got %v", nss)
	}
}

func TestCreateNamespaceDuplicateKeepsWireClassification(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "app"); err != nil {
		t.Fatalf("create: %v", err)
	}
	err = st.CreateNamespace(ctx, "app")
	if err == nil {
		t.Fatal("duplicate create must fail")
	}
	var de *Error
	if !errors.As(err, &de) || de.Code != ErrInvalidRequest.Code {
		t.Fatalf("duplicate create keeps the wire classification, got %+v", err)
	}
	if strings.HasPrefix(de.Message, "invalid request: ") {
		t.Fatalf("message should not carry the wire prefix, got %q", de.Message)
	}
}

func TestDropMissingNamespaceIsNotFound(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	err = st.DropNamespace(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("dropping a missing namespace must be not_found, got %v", err)
	}
}

func TestListNamespacesRejectsBlankPrefix(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := st.ListNamespaces(context.Background(), ListNamespacesOptions{Prefix: " "}); err == nil {
		t.Fatal("a prefix that normalizes to empty must be rejected")
	}
}

func TestNamespaceOperationsAfterCloseReturnErrClosed(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "app"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := st.CreateNamespace(ctx, "other"); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after close must return ErrClosed, got %v", err)
	}
	if _, err := st.ListNamespaces(ctx, ListNamespacesOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("list after close must return ErrClosed, got %v", err)
	}
	if err := st.DropNamespace(ctx, "app"); !errors.Is(err, ErrClosed) {
		t.Fatalf("drop after close must return ErrClosed, got %v", err)
	}
}
