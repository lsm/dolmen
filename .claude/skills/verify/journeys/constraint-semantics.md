# Portable constraint semantics

For changes to managed key equality, predicates, rollback or persistence. Use the
shared synthetic setup and drive a compiled binary:

```bash
S=.claude/skills/verify/scripts
W=$($S/serve.sh)
trap '$S/stop.sh "$W"' EXIT
python3 "$S/constraint-semantics.py" "$W"
```

Expect four `PASS` lines: distinct int64-boundary numbers, exact numeric equality,
stored timestamp offsets/precision, case and NULL components; partial-key state
transitions; a refused batch leaving rows and the change feed untouched; and eight
racing duplicates producing one commit and seven named conflicts. Event comparisons
retain every stable field and omit only the opaque `cursor`, which the server mints
anew on each read; an unchanged-read control pins that comparison.

Run the five `constraint_semantics_test.go` contracts on SQLite, PostgreSQL and
lakehouse using the other-backends journey. They also reopen the store and verify
enforcement persists. With the native bundle, the public Go-library
`lakehouse-constraints.go` journey additionally checks a real library reopen.
