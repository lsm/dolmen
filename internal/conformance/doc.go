// Package conformance holds the contract-conformance test suite: black-box
// tests that drive the HTTP /v1 API and the MCP tools/call surface of one
// server and pin the interface — transport parity, the golden error contract,
// the documented limits table, typed-read coercion, write semantics, search
// invariants, and migration guards.
//
// The suite is deliberately transport-level: everything except deliberate
// out-of-band fixtures (corrupt vectors, store reopen) goes through the HTTP
// and MCP handlers, never the store directly, so it pins what clients see.
package conformance
