# The sidecar protocol (spike version)

Not the final wire — a minimal shape both engines can implement and the driver
can attack. It exists so the spike measures the same things for DuckDB and
DataFusion rather than measuring two different protocols.

## Framing

One request per line on the sidecar's stdin, one response per line on stdout.
Both are tab-separated so a value can never contain the separator, and both carry
a request id so a response can be matched when a query is cancelled.

```
request:  <id>\t<op>\t<arg>\t<arg>...
response: <id>\t<ok|error>\t<payload...>
```

Tab-separated rather than JSON on purpose: neither engine needs a JSON library
for it, so the protocol cannot be the reason one engine looks harder than it is.

## Ops

| op | args | meaning |
|---|---|---|
| `init` | `dataDir`, `snapshot`, `table`, `location` | register one namespace's table at a pinned Iceberg snapshot |
| `query` | `sql` | run caller SQL; the result is the response payload |
| `cancel` | — | interrupt the in-flight query |
| `memory_limit` | `bytes` | refused with `not_supported`: the ceiling is fixed at startup by `SIDECAR_MEMORY_MAX`, before the configuration is locked |
| `shutdown` | — | exit cleanly |

`init` is repeated once per table in a namespace. There is deliberately no op
that registers a second namespace: confinement is a property of how the sidecar
is started, and the tests check that the started sidecar cannot reach anything
else rather than that the protocol declines politely.

**`init` must acknowledge with a non-empty payload**, and the driver rejects an
empty one. An empty acknowledgement cannot be told apart from a sidecar that
accepted the op and registered nothing, and that ambiguity is not theoretical:
it ran through an entire CI run of the DataFusion side, where every query
answered "no table registered" while every `init` reported success.

## Query response

```
<id>\tok\t<columns semicolon-separated>\t<rows>\t<elapsed_ms>\t<truncated 0|1>
```

`columns` is `name:sqlType` per column. `rows` is `|`-separated values with
`\n` and `|` backslash-escaped. Errors are

```
<id>\terror\t<class>\t<message>
```

where `class` is one of `not_found`, `query_error`, `not_supported`,
`engine_unavailable`, `internal_error` — the subset of `internal/derr` the two
engines can distinguish, so a driver-side error can be compared across them.

## What this protocol does not decide

It is not the final wire and should not be mistaken for it: no streaming, no
pagination, no cursor, no parameter binding, no auth. A query is one statement
and the whole result comes back on one line. Anything the conformance suite
needs beyond that is a later decision.