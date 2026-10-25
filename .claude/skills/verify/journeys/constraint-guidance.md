# Served constraint guidance

For changes to the constraint sections of `skill/dolmen*.md`. Run the rendered
contracts for SQLite, PostgreSQL and DuckDB, then read both skills from a compiled
throwaway server:

```bash
go test -count=1 ./skill -run '^TestConstraintGuidanceExplainsPortableKeysAndRepair$'
S=.claude/skills/verify/scripts
W=$($S/serve.sh)
trap '$S/stop.sh "$W"' EXIT
curl -fsS "$(cat "$W/url")/skills/dolmen-admin"
curl -fsS "$(cat "$W/url")/skills/dolmen"
```

The admin guide must teach exact numeric key equality, timestamps compared as
stored, NULL keys, per-owner keys and references, no cascades, relation endpoint
protection and the older-writer fence. It must retain each constraint migration
as the sole change in its call and the authenticated read gates. The data guide
must teach repairing a conflict rather than retrying unchanged data and whole-
batch rollback. Both responses must contain rendered Markdown, with no template
placeholders. Stop the server even on failure.
