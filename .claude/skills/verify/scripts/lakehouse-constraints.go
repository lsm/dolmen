package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/lsm/dolmen"
	"github.com/lsm/dolmen/lakehouse"
)

func require(err error) {
	if err != nil {
		panic(err)
	}
}

func conflict(label string, err error) {
	if !errors.Is(err, dolmen.ErrConflict) {
		panic(fmt.Sprintf("%s: want conflict, got %v", label, err))
	}
	fmt.Printf("PASS %s: %v\n", label, err)
}

func main() {
	if len(os.Args) != 3 {
		panic("usage: go run lakehouse-constraints.go SIDECAR EXTENSIONS")
	}
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "dolmen-lakehouse-constraints-")
	require(err)
	defer os.RemoveAll(dir)
	option := lakehouse.With(lakehouse.Config{Sidecar: os.Args[1], Extensions: os.Args[2]})
	st, err := dolmen.Open(dir, option)
	require(err)
	defer func() { require(st.Close()) }()
	_, err = st.CreateTableWithOptions(ctx, "app", "projects", []dolmen.Field{{Name: "slug", Type: dolmen.String}}, dolmen.CreateTableOptions{AcceptReferences: true})
	require(err)
	schema, err := st.CreateTableWithOptions(ctx, "app", "tasks", []dolmen.Field{{Name: "project_id", Type: dolmen.Number}, {Name: "slug", Type: dolmen.String}, {Name: "done", Type: dolmen.Boolean}}, dolmen.CreateTableOptions{Constraints: []dolmen.Constraint{dolmen.ForeignKeyConstraint{Name: "project_ref", Field: "project_id", ReferencesTable: "projects"}, dolmen.UniqueConstraint{Name: "open_slug", Fields: []string{"slug"}, Where: "done = FALSE"}}})
	require(err)
	if len(schema.Constraints) != 2 {
		panic("constraints missing from created schema")
	}
	fmt.Printf("PASS declared schema: %d constraints\n", len(schema.Constraints))
	_, err = st.Insert(ctx, "app", "projects", []map[string]any{{"slug": "project"}}, dolmen.InsertOptions{})
	require(err)
	insert := func(parent int, done bool) error {
		_, err := st.Insert(ctx, "app", "tasks", []map[string]any{{"project_id": parent, "slug": "same", "done": done}}, dolmen.InsertOptions{})
		return err
	}
	require(insert(1, false))
	conflict("partial unique duplicate", insert(1, false))
	conflict("foreign key orphan", insert(99, true))
	require(insert(1, true))
	_, err = st.Delete(ctx, "app", "projects", dolmen.DeleteOptions{Filter: "id = 1"})
	conflict("parent delete restricted", err)
	require(st.Close())
	st, err = dolmen.Open(dir, option)
	require(err)
	schema, count, err := st.DescribeTable(ctx, "app", "tasks")
	require(err)
	if len(schema.Constraints) != 2 || count != 2 {
		panic(fmt.Sprintf("restart readback: constraints=%d rows=%d", len(schema.Constraints), count))
	}
	fmt.Printf("PASS restart readback: %d constraints, %d rows\n", len(schema.Constraints), count)
	conflict("restart enforcement", insert(1, false))
}
