# Numeric fidelity matrix

Status: pinned decisions transcribed from the [#279](https://github.com/lsm/dolmen/issues/279)
parity arc (PRs #301–#309). Documentation-only: every ruling below is already landed in code and
pinned by `TestNumericFidelityMatrix` (`internal/conformance/embedded_parity_test.go`) unless
another test is named. Number handling is one engine policy expressed per surface.

## Storage classes

Number fields coerce at `coerceValue` (`internal/store/insert.go:448`), int64-first:

- **Class (a), int64 exactness.** Every integer kind narrows to int64 before storage; unsigned
  values above `math.MaxInt64` are rejected (`number overflows int64`). The signed-int64
  boundaries — minimum `-9223372036854775808` (−2^63) and maximum `9223372036854775807`
  (2^63−1) — round-trip exactly on both surfaces. The overflow rejection is a typed-integer
  rule: a `json.Number` token above the int64 range (e.g. `9223372036854775809`) is not
  rejected — `Int64()` fails and the `Float64()` fallback accepts it with rounding, keeping the
  REAL storage class — so the wire's token path and the façade's typed-integer rejection
  diverge for the same value (code-verified; no repo test pins this band). Query bind arguments
  follow the same policy (`normalizeArg`, `internal/store/query.go:14`: integral `json.Number`
  → int64, else float64).
- **Class (b), float64 shortest round-trip.** A decimal beyond float64 precision stores to the
  nearest double and reads back in its shortest round-trip form — at most 17 significant digits:
  `0.1234567890123456789012345` → `0.12345678901234568`, while `0.10000000000000000001` →
  `0.1`. Adjacent decimals beyond float64 precision conflate to the same double — documented
  behavior, asserted equal on both surfaces, not a defect.
- **Class (c), JSON blobs verbatim.** `json` fields keep their number tokens byte-for-byte:
  `3.141592653589793238462643383279` and the below-range exponent `1e-400` survive exactly,
  decoding back as numeric `json.Number` tokens (a quoted-string regression on either surface
  fails the pin). The wire's `UseNumber` decode preserves the original token; the façade's
  `json.Number` values pass through.

`json.Number` on **number** fields is accepted and parsed to its numeric value (the #309
design-note resolution): `Int64()` first, else `Float64()` — `json.Number("2.5")` stores 2.5.

## The finite guard

`finiteNumber` (`internal/store/insert.go:441`) rejects NaN and ±Inf
(`number must be finite (NaN and infinities are not storable)`) as `invalid_request`. One point
guards every write path — `Insert`, `Update`, `UpsertByKey` share the `coerceValue` call
sites — covering `float64`, `float32`, and the reflect fallback. Pinned by
`TestInsertRejectsNonFiniteNumbers` (root `familyaudit_test.go`): NaN/±Inf/float32-NaN and the
`json.Number` spellings on `Insert`, plus an `Update` `Set`; the `UpsertByKey` leg is guarded by
the same call sites but carries no dedicated test pin. The wire is structurally immune for float
tokens (JSON cannot spell a non-finite number), but `json.Number` is a string-backed token a Go
caller can construct, which was the hole #309 closed (squash commit `ba06ba2`; the arc's
in-branch commits are squash-merged and unresolvable): json.Number parses are routed
through the guard, so `json.Number("NaN")` and `json.Number("Inf")` are rejected instead of
stored. Before the fix NaN read back as silent NULL and ±Inf poisoned the row — every later
read errored `column produced a non-finite value`.

Range rejection fires **before** the guard: for `1e400`, `json.Number.Float64()`
(`strconv.ParseFloat` underneath) returns a range error, so `coerceValue` fails with
`expected a number` → `invalid_request` without the finiteness check ever being consulted.
Pinned on both surfaces: HTTP 400 `invalid_request`, façade `ErrInvalidRequest`.

## Read-back canonicalization

Cross-surface comparison of stored numbers is the parity harness's rule, not a shared library
helper: `numberKey` (`internal/conformance/embedded_parity_test.go:44`) parses int64-first —
`Int64()`, else `Float64()` — emitting `strconv.FormatInt` or `strconv.FormatFloat(f, 'g', -1,
64)` for the respective parse; the raw token survives **only** when parsing fails outright.
That rule reconciles the wire's JSON decimal spellings with the embedded float64 values without
privileging either surface's formatting. The JSON-RPC drain path keeps a separate request-ID key
(`internal/mcp/drain.go:124`) with two more steps — an unsigned-exact `ParseUint` branch, and a
collapse of exactly-integral float64s in range back to `FormatInt`; the two helpers serve
different purposes (stored-value parity vs request-ID bookkeeping) and are not interchangeable.

The decimal-vs-`'g'` band is pinned as spec rows, not formatting accidents:
`0.00001` (the wire's plain-decimal spelling of the [1e-6, 1e-4) band) and `1e-7` (the wire's
exponent-stripped spelling below 1e-6) both read back as the `'g'` canonical `1e-05` and
`1e-07` on both surfaces.

## SQLite NUMERIC affinity read-back

Number columns use NUMERIC affinity, and SQLite rewrites a fractionless REAL to INTEGER storage
at write time when the value is exactly representable as a signed integer.

- Pinned on the embedded surface: `−0.0` reads back as concrete **int64 0** — the Go-type pin
  (the assert requires `.(int64)`, not just value equality), embodying the twice-adjudicated
  hyperneo r2 P1 concern (adjudication 5659925896): the INTEGER storage class is the engine's
  documented behavior, not a parity bug. The wire leg of the same matrix pins value equality
  only: it sends `json.Number("-0")`, which `Int64()` parses to zero before SQLite ever sees a
  REAL, so the HTTP response carries the value without pinning a concrete type.
- The rewrite's bound follows from SQLite's affinity rule, not from a repo test pin: a
  fractionless float inside int64 range (e.g. `2.0`) returns through the integer read path —
  embedded `int64` where the wire carries the equivalent JSON number — while a fractionless
  float outside the range (e.g. `1e20`) keeps the REAL storage class and reads back as
  `float64` (verified against SQLite directly; `TestNumericFidelityMatrix` stores no
  out-of-range fractionless float, so this direction carries no test pin).
