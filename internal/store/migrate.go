package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

func (s *Store) Migrate(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder) (*schema.TableSchema, error) {
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	old, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, err
	}

	fields := make([]schema.Field, len(old.Fields))
	copy(fields, old.Fields)
	cur := &schema.TableSchema{Namespace: nsName, Name: table, Version: old.Version, Fields: fields, EmbedSpace: old.EmbedSpace, EmbedDim: old.EmbedDim}

	type step func(ctx context.Context, tx *sql.Tx) error
	var steps []step
	var rebuildFTSNeeded, vectorizeChanged bool
	var droppedFTSChange bool

	findField := func(name string) (*schema.Field, error) {
		for i := range cur.Fields {
			if cur.Fields[i].Name == name {
				return &cur.Fields[i], nil
			}
		}
		return nil, invalidf("field %q not found", name)
	}

	for _, ch := range changes {
		switch ch.Op {
		case schema.OpAddField:
			if ch.Field == nil {
				return nil, invalidf("add_field needs a field object")
			}
			f := schema.Normalize([]schema.Field{*ch.Field})[0]
			if err := schema.ValidateIdent(f.Name, "field name"); err != nil {
				return nil, invalidf("%s", err)
			}
			for _, ef := range cur.Fields {
				if ef.Name == f.Name {
					return nil, invalidf("field %q already exists", f.Name)
				}
			}
			if len(cur.Fields) >= MaxFieldsPerTable {
				return nil, invalidf("migration would leave %d fields (max %d; ALTERs run in request order, so adds cannot exceed the cap even when later drops reduce the final count)", len(cur.Fields)+1, MaxFieldsPerTable)
			}
			cur.Fields = append(cur.Fields, f)
			if f.Fulltext {
				rebuildFTSNeeded = true
			}
			if f.Vectorize {
				vectorizeChanged = true
			}
			steps = append(steps, func(ctx context.Context, tx *sql.Tx) error {
				if f.Required {
					var n int64
					if err := tx.QueryRowContext(ctx,
						fmt.Sprintf(`SELECT count(*) FROM %s`, q(table))).Scan(&n); err != nil {
						return err
					}
					if n > 0 {
						return invalidf("cannot add required field %q to a table with %d existing rows (no backfill value can be supplied); add it nullable instead", f.Name, n)
					}
				}
				ddl := schema.SQLType(f)
				if f.Required {
					ddl += ` NOT NULL`
				}
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, q(table), q(f.Name), ddl))
				return err
			})
		case schema.OpRenameField:
			if err := schema.ValidateIdent(ch.To, "new field name"); err != nil {
				return nil, invalidf("%s", err)
			}
			f, err := findField(ch.From)
			if err != nil {
				return nil, err
			}
			oldName := f.Name
			if ch.To != ch.From {
				for _, ef := range cur.Fields {
					if ef.Name == ch.To {
						return nil, invalidf("field %q already exists", ch.To)
					}
				}
			}
			f.Name = ch.To
			if f.Fulltext {
				rebuildFTSNeeded = true
			}
			steps = append(steps, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, q(table), q(oldName), q(ch.To)))
				return err
			})
		case schema.OpDropField:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if f.Vectorize {
				vectorizeChanged = true
			}
			if f.Fulltext {
				rebuildFTSNeeded = true
				droppedFTSChange = true
			}
			idx := -1
			for i := range cur.Fields {
				if cur.Fields[i].Name == ch.Name {
					idx = i
					break
				}
			}
			cur.Fields = append(cur.Fields[:idx], cur.Fields[idx+1:]...)
			steps = append(steps, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, q(table), q(ch.Name)))
				return err
			})
		case schema.OpSetFulltext:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if ch.Value && f.Type != schema.String && f.Type != schema.Text {
				return nil, invalidf("field %q: fulltext is only allowed on string or text fields", f.Name)
			}
			if f.Fulltext != ch.Value {
				f.Fulltext = ch.Value
				rebuildFTSNeeded = true
			}
		case schema.OpSetVectorize:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if ch.Value {
				if f.Type != schema.String && f.Type != schema.Text {
					return nil, invalidf("field %q: vectorize is only allowed on string or text fields", f.Name)
				}
				for i := range cur.Fields {
					if cur.Fields[i].Vectorize && &cur.Fields[i] != f {
						return nil, invalidf("at most one vectorized field per table (already %q)", cur.Fields[i].Name)
					}
				}
			}
			if f.Vectorize != ch.Value {
				f.Vectorize = ch.Value
				vectorizeChanged = true
			}
		default:
			return nil, invalidf("unknown migration op %q (valid: add_field, rename_field, drop_field, set_fulltext, set_vectorize)", ch.Op)
		}
	}
	if err := schema.Validate(cur.Fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(ftsFields(cur.Fields)) == 0 && len(ftsFields(old.Fields)) > 0 {
		rebuildFTSNeeded = true
		droppedFTSChange = true
	}
	_ = droppedFTSChange

	for _, st := range steps {
		if err := st(ctx, tx); err != nil {
			return nil, fmt.Errorf("migration step failed: %w", err)
		}
	}

	if rebuildFTSNeeded {
		if err := dropFTS(ctx, tx, table); err != nil {
			return nil, err
		}
		if fts := ftsFields(cur.Fields); len(fts) > 0 {
			if err := createFTS(ctx, tx, table, fts); err != nil {
				return nil, err
			}
		}
	}

	if vectorizeChanged {
		newVec := vectorizeField(cur.Fields)
		if newVec != nil {
			modelChanged := cur.EmbedSpace != "" && emb.Identity != "" && cur.EmbedSpace != emb.Identity
			if old.VectorizeField() != nil || modelChanged {
				if _, err := tx.ExecContext(ctx,
					fmt.Sprintf(`UPDATE %s SET "_embedding" = NULL`, q(table))); err != nil {
					return nil, err
				}
			}
			cur.EmbedDim = 0
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE %s ADD COLUMN "_embedding" BLOB`, q(table))); err != nil {
				if !strings.Contains(err.Error(), "duplicate column") {
					return nil, fmt.Errorf("add _embedding column: %w", err)
				}
			}
			if emb.Embed == nil {
				return nil, invalidf("vectorize requires an embedding provider (set DOLMEN_EMBED_PROVIDER)")
			}
			if emb.Identity == "" {
				return nil, invalidf("vectorize requires an embedding provider with a reported identity so backfilled rows are attributable to an embedding space")
			}
			for {
				rows, err := tx.QueryContext(ctx,
					fmt.Sprintf(`SELECT id, %s FROM %s WHERE "_embedding" IS NULL AND %s IS NOT NULL AND %s != '' ORDER BY id LIMIT 128`,
						q(newVec.Name), q(table), q(newVec.Name), q(newVec.Name)))
				if err != nil {
					return nil, err
				}
				type pending struct {
					id   int64
					text string
				}
				var batch []pending
				for rows.Next() {
					var p pending
					if err := rows.Scan(&p.id, &p.text); err != nil {
						rows.Close()
						return nil, err
					}
					batch = append(batch, p)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return nil, err
				}
				if len(batch) == 0 {
					break
				}
				texts := make([]string, len(batch))
				for i, p := range batch {
					texts[i] = p.text
				}
				vecs, err := emb.Embed(ctx, texts)
				if err != nil {
					return nil, fmt.Errorf("backfill embedding failed: %w", err)
				}
				if len(vecs) != len(texts) {
					return nil, fmt.Errorf("backfill: embedding provider returned %d vectors for %d texts", len(vecs), len(texts))
				}
				for _, v := range vecs {
					if len(v) == 0 {
						return nil, invalidf("backfill: embedding provider returned a zero-dimensional vector for table %s", table)
					}
					for _, x := range v {
						if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
							return nil, invalidf("backfill: embedding provider returned a non-finite vector component for table %s", table)
						}
					}
					if cur.EmbedDim == 0 {
						cur.EmbedDim = len(v)
					} else if len(v) != cur.EmbedDim {
						return nil, invalidf("embedding provider returned %d-dimensional vectors mid-backfill (expected %d)", len(v), cur.EmbedDim)
					}
				}
				for i, p := range batch {
					if _, err := tx.ExecContext(ctx,
						fmt.Sprintf(`UPDATE %s SET "_embedding" = ? WHERE id = ?`, q(table)),
						schema.EncodeVector(vecs[i]), p.id); err != nil {
						return nil, err
					}
				}
			}
			cur.EmbedSpace = emb.Identity
			if cur.EmbedDim == 0 {
				var dim int
				if err := tx.QueryRowContext(ctx,
					fmt.Sprintf(`SELECT length("_embedding") / 4 FROM %s WHERE "_embedding" IS NOT NULL LIMIT 1`, q(table))).Scan(&dim); err == nil && dim > 0 {
					cur.EmbedDim = dim
				}
			}
		} else if old.VectorizeField() != nil {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE %s SET "_embedding" = NULL`, q(table))); err != nil {
				return nil, err
			}
		}
	}

	cur.Version = old.Version + 1
	if err := saveSchemaTx(ctx, tx, nsName, cur, old.Version, changes); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cur, nil
}
