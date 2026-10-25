# Constraint-aware authorization

For changes to constraint read gates in `internal/api`. Use only a disposable
server and a throwaway admin key; keep the shared setup in SKILL.md.

```bash
set -e
S=.claude/skills/verify/scripts
export DOLMEN_ADMIN_KEY="$(openssl rand -hex 24)"
W=$($S/serve.sh -auth on -trusted-proxies 127.0.0.0/8)
trap '$S/stop.sh "$W"' EXIT
python3 "$S/constraint-auth.py" "$W"
```

Expect five `PASS` lines. Through HTTP and MCP, writes require read on both
constraint endpoints; a table-wide child read alone cannot replace parent read.
Deleting from an opted-in parent and changing its `row_access` each have an
identical namespace-read gate with zero or hidden incoming references. After that grant, RESTRICT still blocks deletion
of a referenced parent. Constraint migration planning requires all affected table
reads. The conformance tests additionally cover standing own-row reads, wider
mutations, imports, relation writes, batches and authenticated tool schemas.
