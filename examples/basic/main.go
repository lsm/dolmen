package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/lsm/dolmen"
)

type echoProvider struct{}

func (echoProvider) Identity() string { return "example|echo|8" }

func (echoProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return echoEmbed(texts), nil
}

func (echoProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return echoEmbed([]string{text})[0], nil
}

func echoEmbed(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, 8)
		for j, r := range []byte(texts[i]) {
			v[r%8] += float32(j + 1)
		}
		out[i] = v
	}
	return out
}

func main() {
	dir, err := os.MkdirTemp("", "dolmen-example-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	st, err := dolmen.Open(filepath.Join(dir, "data"), dolmen.WithEmbedding(echoProvider{}))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "demo"); err != nil {
		log.Fatal(err)
	}

	schema, err := st.CreateTable(ctx, "demo", "notes", []dolmen.Field{
		{Name: "body", Type: dolmen.Text, Fulltext: true, Vectorize: true},
		{Name: "priority", Type: dolmen.Number},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("created", schema.Namespace+"/"+schema.Name, "at version", schema.Version)

	ins, err := st.Insert(ctx, "demo", "notes", []map[string]any{
		{"body": "refund processed promptly", "priority": 1},
		{"body": "payment is pending", "priority": 2},
	}, dolmen.InsertOptions{IdempotencyKey: "seed-1"})
	if err != nil {
		log.Fatal(err)
	}

	retry, err := st.Insert(ctx, "demo", "notes", []map[string]any{
		{"body": "refund processed promptly", "priority": 1},
		{"body": "payment is pending", "priority": 2},
	}, dolmen.InsertOptions{IdempotencyKey: "seed-1"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("inserted", ins.Inserted, "rows, replay returned", retry.Replayed, "with ids", retry.Ids)

	_, err = st.Insert(ctx, "demo", "notes", []map[string]any{
		{"body": "different body"},
	}, dolmen.InsertOptions{IdempotencyKey: "seed-1"})
	if !errors.Is(err, dolmen.ErrConflict) {
		log.Fatal(err)
	}
	fmt.Println("typed conflict rejected the reused idempotency key")

	rows, err := st.GetRows(ctx, "demo", "notes", ins.Ids)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("fetched", len(rows.Rows), "rows; first body:", rows.Rows[0]["body"])

	page, err := st.Query(ctx, "demo", "SELECT id, body FROM notes WHERE priority = ? ORDER BY id", dolmen.QueryOptions{Args: []any{1}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("query matched", len(page.Rows), "rows")

	hits, err := st.SearchFulltext(ctx, "demo", "notes", "refunds", dolmen.SearchOptions{Limit: 5})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("fulltext found", len(hits.Rows), "rows")

	near, err := st.SearchVector(ctx, "demo", "notes", dolmen.VectorQuery{Text: "refund"}, dolmen.SearchOptions{Limit: 5})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("vector found", len(near.Rows), "rows, top score", near.Rows[0]["_score"])

	if err := st.Close(); err != nil {
		log.Fatal(err)
	}
	if err := st.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("closed cleanly")
}
