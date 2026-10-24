package badgerindex

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/dgraph-io/badger/v4"
	gb "github.com/lsm/dolmen/spike/graph-index"
)

type index struct{ db *badger.DB }
type view struct{ tx *badger.Txn }

func Open(dir string) (gb.Index, error) {
	opts := badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(true).WithBlockCacheSize(48 << 20).WithIndexCacheSize(16 << 20).WithMemTableSize(16 << 20).WithNumMemtables(2).WithValueLogFileSize(64 << 20)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}
	return &index{db}, nil
}
func key(prefix byte, node, neighbor, id uint64) []byte {
	k := make([]byte, 25)
	k[0] = prefix
	binary.BigEndian.PutUint64(k[1:], node)
	binary.BigEndian.PutUint64(k[9:], neighbor)
	binary.BigEndian.PutUint64(k[17:], id)
	return k
}
func (i *index) Append(ctx context.Context, edges []gb.Edge) error {
	if err := gb.Validate(edges); err != nil {
		return err
	}
	return i.db.Update(func(tx *badger.Txn) error {
		for _, e := range edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			id := key('e', e.ID, 0, 0)[:9]
			if _, err := tx.Get(id); err == nil {
				return fmt.Errorf("duplicate edge ID")
			} else if err != badger.ErrKeyNotFound {
				return err
			}
			value := make([]byte, 17)
			binary.BigEndian.PutUint64(value, e.From)
			binary.BigEndian.PutUint64(value[8:], e.To)
			value[16] = e.Kind
			for _, kv := range []struct{ k, v []byte }{{id, value}, {key('o', e.From, e.To, e.ID), []byte{e.Kind}}, {key('i', e.To, e.From, e.ID), []byte{e.Kind}}} {
				if err := tx.Set(kv.k, kv.v); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func (i *index) Snapshot(ctx context.Context) (gb.View, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &view{i.db.NewTransaction(false)}, nil
}
func (v *view) Adjacent(ctx context.Context, node uint64, reverse bool, limit int) ([]gb.Edge, bool, error) {
	if limit < 0 || limit > 1000000 {
		return nil, false, fmt.Errorf("adjacency limit must be between zero and one million")
	}
	direction := byte('o')
	if reverse {
		direction = 'i'
	}
	prefix := key(direction, node, 0, 0)[:9]
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	it := v.tx.NewIterator(opts)
	defer it.Close()
	edges := []gb.Edge{}
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if len(edges) == limit {
			return edges, true, nil
		}
		item := it.Item()
		k := item.Key()
		e := gb.Edge{ID: binary.BigEndian.Uint64(k[17:]), From: node, To: binary.BigEndian.Uint64(k[9:])}
		if reverse {
			e.From, e.To = e.To, e.From
		}
		if err := item.Value(func(value []byte) error { e.Kind = value[0]; return nil }); err != nil {
			return nil, false, err
		}
		edges = append(edges, e)
	}
	return edges, false, nil
}
func (v *view) Close() error  { v.tx.Discard(); return nil }
func (i *index) Close() error { return i.db.Close() }
