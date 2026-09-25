package dolmen

import (
	"context"
	"reflect"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

type EmbeddingProvider interface {
	Identity() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

type config struct {
	engine          string
	ownerKey        string
	openerEngine    string
	opener          EngineOpener
	embedding       EmbeddingProvider
	embeddingSet    bool
	changeRetention time.Duration
	vectorCache     int64
	secretKey       []byte
	secretKeySet    bool
	secrets         *secret.Keyring
}

type EngineOpener func(ctx context.Context, changeRetention time.Duration) (store.Engine, error)

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

func WithEngineOpener(engine, ownerKey string, open EngineOpener) Option {
	return func(c *config) {
		c.engine = engine
		c.openerEngine = engine
		c.ownerKey = ownerKey
		c.opener = open
	}
}

func WithChangeRetention(d time.Duration) Option {
	return func(c *config) {
		c.changeRetention = d
	}
}

func WithSecretKey(key []byte) Option {
	return func(c *config) {
		c.secretKey = append([]byte(nil), key...)
		c.secretKeySet = true
	}
}

func WithVectorCacheBytes(n int64) Option {
	return func(c *config) {
		c.vectorCache = n
	}
}

func (c *config) validate() error {
	if c.vectorCache < 0 {
		return derr.New(derr.InvalidRequest, "WithVectorCacheBytes: size must not be negative (0 disables the cache)")
	}
	if c.embeddingSet && nilProvider(c.embedding) {
		return derr.New(derr.InvalidRequest, "WithEmbedding: provider must not be nil")
	}
	if c.secretKeySet {
		k, err := secret.New(c.secretKey)
		clear(c.secretKey)
		c.secretKey = nil
		if err != nil {
			return derr.New(derr.InvalidRequest, "WithSecretKey: %v", err)
		}
		c.secrets = k
	}
	if c.changeRetention < 0 {
		return derr.New(derr.InvalidRequest, "WithChangeRetention: duration must not be negative (0 disables pruning)")
	}
	if err := store.ValidateEngine(c.engine); err != nil {
		return derr.New(derr.InvalidRequest, "WithEngine: %v", err)
	}
	if c.opener != nil && c.ownerKey == "" {
		return derr.New(derr.InvalidRequest, "WithEngineOpener: an owner key is required so one process does not open the same engine twice")
	}
	if c.opener == nil && c.ownerKey != "" {
		return derr.New(derr.InvalidRequest, "WithEngineOpener: an opener is required")
	}
	if c.opener != nil && c.engine != c.openerEngine {
		return derr.New(derr.InvalidRequest, "WithEngine(%q) contradicts the %q connection already supplied; pass one engine", c.engine, c.openerEngine)
	}
	if c.engine == store.EnginePostgres && c.opener == nil {
		return derr.New(derr.InvalidRequest, "WithEngine: the %q engine needs a connection; import github.com/lsm/dolmen/postgres and pass postgres.With to supply its DSN", store.EnginePostgres)
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
