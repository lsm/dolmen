# Release fixtures

Each `vX.Y.Z/fixture.db` is a namespace file written by that released binary and checked in
unchanged. `upgrade_fixture_test.go` opens a copy with the current code and requires it to read,
search and write it. Never regenerate an existing fixture; add one per release that changes the
on-disk format.

To add one: build the tag (`git worktree add --detach /tmp/old vX.Y.Z && go build -o /tmp/dolmen-old ./cmd/dolmen`),
run it with `DOLMEN_EMBED_PROVIDER=none -data <dir>`, create namespace `fixture` and table `notes`
with the same writes as `v0.3.0` (a full-text field, an enum, a default, a 3-dim vector, an update, a
delete, and an `add_field` migration), stop the server so the WAL is checkpointed, and copy
`<dir>/fixture.db` here.
