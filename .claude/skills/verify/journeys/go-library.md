# Go library (root `dolmen` package)

For changes to the root package or anything it re-exports. The external-module
example builds against this checkout through `replace => ../..`:

```bash
(cd examples/basic && go build ./... && go vet ./... && go run .)
```

Expect exit 0, printed search results, and `declared relation table and related edge 1
with 2 foreign keys` (one output line). The example uses only public types to opt a
parent into references, declare a relation through `CreateTableWithOptions`, call
`Relate`, and read back its role fields/constraints. This is SQLite-only; the
existing PostgreSQL/lakehouse refusal remains in force. For facade behavior, add the
case to the example or a root-package test, and check
`docs/design/facade-input-matrix.md` and `numeric-fidelity-matrix.md` before
changing any recorded divergence from the wire.
