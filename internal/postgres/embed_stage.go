package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
)

const stageLookupChunk = 500

const (
	dolmenStageDrop = "dolmen-embed-stage-drop"
	dolmenStagePut  = "dolmen-embed-stage-put"
)

type pgStaged struct {
	id     int64
	digest []byte
	vec    []byte
}

func (s *Store) writeStagedVectors(ctx context.Context, ns, table string, gen int64, provider string, batch []pgStaged) error {
	if len(batch) == 0 {
		return nil
	}
	return s.write(ctx, ns, [16]byte{}, func(tx pgx.Tx, n namespace) error {
		if _, err := tx.Prepare(ctx, dolmenStageDrop, "DELETE FROM "+s.relation("embed_stage")+
			" WHERE namespace = $1 AND table_name = $2 AND drop_generation = $3 AND provider = $4 AND row_id = $5"); err != nil {
			return err
		}
		if _, err := tx.Prepare(ctx, dolmenStagePut, "INSERT INTO "+s.relation("embed_stage")+
			" (namespace, table_name, drop_generation, provider, row_id, digest, vector) VALUES ($1,$2,$3,$4,$5,$6,$7)"); err != nil {
			return err
		}
		for _, v := range batch {
			if _, err := tx.Exec(ctx, dolmenStageDrop, ns, table, gen, provider, v.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, dolmenStagePut, ns, table, gen, provider, v.id, v.digest, v.vec); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) loadStagedVectors(ctx context.Context, tx pgx.Tx, ns, table string, gen int64, provider string, ids []int64) (map[int64]pgStaged, error) {
	out := map[int64]pgStaged{}
	if len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += stageLookupChunk {
		end := min(start+stageLookupChunk, len(ids))
		chunk := ids[start:end]
		marks := make([]string, len(chunk))
		args := []any{ns, table, gen, provider}
		for i, id := range chunk {
			marks[i] = fmt.Sprintf("$%d", len(args)+1)
			args = append(args, id)
		}
		rows, err := tx.Query(ctx, "SELECT row_id, digest, vector FROM "+s.relation("embed_stage")+
			" WHERE namespace = $1 AND table_name = $2 AND drop_generation = $3 AND provider = $4 AND row_id IN ("+
			strings.Join(marks, ",")+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var v pgStaged
			if err := rows.Scan(&v.id, &v.digest, &v.vec); err != nil {
				rows.Close()
				return nil, err
			}
			if _, held := out[v.id]; held {
				continue
			}
			out[v.id] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) deleteStagedVectors(ctx context.Context, tx pgx.Tx, ns, table string) error {
	_, err := tx.Exec(ctx, "DELETE FROM "+s.relation("embed_stage")+" WHERE namespace = $1 AND table_name = $2", ns, table)
	return err
}

func (s *Store) dropAbandonedStages(ctx context.Context, ns string, running func(string) bool) error {
	return s.write(ctx, ns, [16]byte{}, func(tx pgx.Tx, n namespace) error {
		rows, err := tx.Query(ctx, "SELECT DISTINCT table_name FROM "+s.relation("embed_stage")+" WHERE namespace = $1", ns)
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				rows.Close()
				return err
			}
			tables = append(tables, table)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, table := range tables {
			if running(table) {
				continue
			}
			if err := s.deleteStagedVectors(ctx, tx, ns, table); err != nil {
				return err
			}
		}
		return nil
	})
}

func encodePGVector(v []float32) []byte {
	return schema.EncodeVector(v)
}

func (s *Store) beginMigrate(ns, table string) func() {
	s.migrateMu.Lock()
	defer s.migrateMu.Unlock()
	if s.migrating == nil {
		s.migrating = map[string]int{}
	}
	key := ns + "." + table
	s.migrating[key]++
	return func() {
		s.migrateMu.Lock()
		defer s.migrateMu.Unlock()
		if s.migrating[key] <= 1 {
			delete(s.migrating, key)
			return
		}
		s.migrating[key]--
	}
}

func (s *Store) migrateRunning(ns string) func(string) bool {
	return func(table string) bool {
		s.migrateMu.Lock()
		defer s.migrateMu.Unlock()
		return s.migrating[ns+"."+table] > 0
	}
}
