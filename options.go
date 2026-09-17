package dolmen

import (
	"context"
	"reflect"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

type EmbeddingProvider interface {
	Identity() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

type config struct {
	engine          string
	embedding       EmbeddingProvider
	embeddingSet    bool
	changeRetention time.Duration
}

type Option func(*config)

func WithEmbedding(provider EmbeddingProvider) Option {
	return func(c *config) {
		c.embedding = provider
		c.embeddingSet = true
	}
}

func WithEngine(name string) Option {
	return func(c *config) {
		c.engine = name
	}
}

func WithChangeRetention(d time.Duration) Option {
	return func(c *config) {
		c.changeRetention = d
	}
}

func (c *config) validate() error {
	if c.embeddingSet && nilProvider(c.embedding) {
		return derr.New(derr.InvalidRequest, "WithEmbedding: provider must not be nil")
	}
	if c.changeRetention < 0 {
		return derr.New(derr.InvalidRequest, "WithChangeRetention: duration must not be negative (0 disables pruning)")
	}
	if err := store.ValidateEngine(c.engine); err != nil {
		return derr.New(derr.InvalidRequest, "WithEngine: %v", err)
	}
	return nil
}

func nilProvider(p EmbeddingProvider) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return v.IsNil()
	}
	return false
}
