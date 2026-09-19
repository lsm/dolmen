package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
)

func physicalCandidate(name string, attempt int) string {
	if len(name) <= 63 && attempt == 0 {
		return name
	}
	key := name
	if attempt > 0 {
		key += "/" + strconv.Itoa(attempt)
	}
	sum := sha256.Sum256([]byte(key))
	prefix := name
	if len(prefix) > 46 {
		prefix = prefix[:46]
	}
	return prefix + "_" + hex.EncodeToString(sum[:8])
}

func physicalColumns(fields []schema.Field) (map[string]string, error) {
	out := map[string]string{}
	used := map[string]bool{"id": true, "created_at": true, "_embedding": true}
	for _, f := range fields {
		if len(f.Name) <= 63 {
			out[f.Name] = f.Name
			used[f.Name] = true
		}
	}
	for _, f := range fields {
		if _, ok := out[f.Name]; ok {
			continue
		}
		for i := 0; i < 64; i++ {
			name := physicalCandidate(f.Name, i)
			if !used[name] {
				out[f.Name] = name
				used[name] = true
				break
			}
		}
		if out[f.Name] == "" {
			return nil, fmt.Errorf("postgres: cannot allocate a distinct field identifier")
		}
	}
	return out, nil
}

func physicalTable(ctx context.Context, tx pgx.Tx, n namespace, name string) (string, error) {
	for i := 0; i < 64; i++ {
		candidate := physicalCandidate(name, i)
		var exists bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2)`, n.physical, candidate).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("postgres: cannot allocate a distinct table identifier")
}
