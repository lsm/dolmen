# Go library (root `dolmen` package)

For changes to the root package or anything it re-exports. The external-module
example builds against this checkout through `replace => ../..`:

```bash
(cd examples/basic && go build ./... && go vet ./... && go run .)
```

Expect exit 0 and its printed search results. For facade behavior, add the
case to the example or a root-package test, and check
`docs/design/facade-input-matrix.md` and `numeric-fidelity-matrix.md` before
changing any recorded divergence from the wire.
