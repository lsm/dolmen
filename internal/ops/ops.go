package ops

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

type EmbeddingProvider interface {
	Identity() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

func Embedder(emb EmbeddingProvider) store.Embedder {
	return store.Embedder{Embed: emb.Embed, Identity: emb.Identity()}
}

func EnsureNamespace(ctx context.Context, eng store.Engine, ns string) error {
	if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil && !errors.Is(err, store.ErrExists) {
		return err
	}
	return nil
}

type VectorQuery struct {
	Column   string
	Text     string
	Vec      []float64
	Filter   string
	Args     []any
	MinScore *float64
}

func PrepareVectorQuery(ctx context.Context, eng store.Engine, ns, table string, in VectorQuery, emb EmbeddingProvider, providerHelp string) (store.VectorQuery, error) {
	if in.Text != "" && len(in.Vec) > 0 {
		return store.VectorQuery{}, derr.New(derr.InvalidRequest, "pass either text or vector, not both")
	}
	column := strings.ToLower(strings.TrimSpace(in.Column))
	var vec []float32
	switch {
	case in.Text != "":
		if err := EnsureNamespace(ctx, eng, ns); err != nil {
			return store.VectorQuery{}, err
		}
		sc, _, err := eng.TableState(ctx, ns, table, nil)
		if err != nil {
			return store.VectorQuery{}, err
		}
		if err := store.ValidateVectorQuery(sc, table, column, emb.Identity()); err != nil {
			return store.VectorQuery{}, err
		}
		if emb.Identity() == "" {
			msg := "text queries are embedded server-side, but this server has no usable embedding provider (none is configured, or the configured one does not report its identity)"
			if providerHelp != "" {
				msg += "; " + providerHelp
			}
			return store.VectorQuery{}, derr.New(derr.InvalidRequest, "%s", msg)
		}
		qv, err := emb.EmbedQuery(ctx, in.Text)
		if err != nil {
			return store.VectorQuery{}, err
		}
		if len(qv) == 0 {
			return store.VectorQuery{}, derr.New(derr.InvalidRequest, "embedding provider returned a zero-dimensional vector for the query text")
		}
		vec = qv
	case len(in.Vec) > 0:
		vec = make([]float32, len(in.Vec))
		for i, x := range in.Vec {
			if math.IsNaN(x) || math.Abs(x) > math.MaxFloat32 {
				return store.VectorQuery{}, derr.New(derr.InvalidRequest, "vector entry %d is outside the float32 range", i)
			}
			vec[i] = float32(x)
		}
		if err := EnsureNamespace(ctx, eng, ns); err != nil {
			return store.VectorQuery{}, err
		}
	default:
		return store.VectorQuery{}, derr.New(derr.InvalidRequest, "pass either text or vector")
	}
	queryIdentity := ""
	if in.Text != "" {
		queryIdentity = emb.Identity()
	}
	return store.VectorQuery{
		Column:     column,
		Vec:        vec,
		EmbedModel: queryIdentity,
		Filter:     in.Filter,
		Args:       in.Args,
		MinScore:   in.MinScore,
	}, nil
}
