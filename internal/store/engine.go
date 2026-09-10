package store

import (
	"context"

	"github.com/lsm/dolmen/internal/schema"
)

// Engine is the storage seam of docs/design/identity-and-engines.md §6: the
// tenancy portability guarantee. Everything above it — envelope, error
// mapping, validation, authn/authz, visible-set computation, skills/MCP/
// OpenAPI rendering — is engine-neutral and shared; the engine owns
// everything namespace/table/row below (§6.1: namespace lifecycle, table DDL,
// row CRUD, filtered reads, migrate ops, search execution, and change-log
// access per §9). SQLite (*Store) is adapter #1 with zero contract change —
// the conformance suite is the proof (§6, §8).
//
// Signatures here are pinned once (slice 2a): later slices add
// implementations, never parameters — the guard parameters below carry the
// authorization semantics so no slice re-signatures this interface.
//
// GLOBAL RULES, binding on every implementation (§6.2):
//
//   - The engine NEVER creates a namespace implicitly. Every operation that
//     opens a namespace — listing, table state, DDL, rows, search — requires
//     it to exist, checked atomically with the operation (ErrNotFound
//     otherwise).
//
//   - EVERY call whose authorization was resolved against an object carries
//     and atomically verifies that object's incarnation: the namespace
//     creation id (nsGen [16]byte) for namespace-level calls — Query, the
//     change-log reads, and the namespace lifecycle methods — and the full
//     Incarnation for table-level calls. A drop-and-recreate between
//     authorization and execution can never act on a successor the old grant
//     does not cover.
//
//   - Zero values mean "no guard" (auth off): a zero nsGen, a zero
//     Incarnation, a nil RowScope, an empty AuthBinding set. An empty binding
//     set carries exactly that one meaning at the seam — under auth: on the
//     API layer never calls these methods with zero bindings (a principal
//     holding no grant is short-circuited above the seam, §2), so the engine
//     cannot mistake "filter everything" for "no guard" and expose tenant
//     names.
//
// The engine knows nothing of principals, grants, or verbs (§6.3): it
// receives opaque owner strings and the already-resolved guards below.
type Engine interface {
	// NamespaceState returns the namespace's creation id (§6.2): the read the
	// API layer uses to build CreateTable's nsGen guard, ErrNotFound when the
	// namespace is absent — the read never creates implicitly, and an empty
	// namespace has no TableState to consult. auth is verified atomically with
	// the read (see AuthBinding).
	NamespaceState(ctx context.Context, ns string, auth []AuthBinding) ([16]byte, error)

	// ListNamespaces lists the namespaces visible through bindings, verified
	// atomically with the listing (§6.2, §5.3): visibility granted by a
	// binding bound to a predecessor namespace lifetime must not surface a
	// recreated successor. prefix restricts the listing to that path's subtree
	// ("" lists all, in database-filename order — path + ".db", §5.3).
	ListNamespaces(ctx context.Context, prefix string, bindings []AuthBinding) ([]string, error)

	// CreateNamespace creates an empty namespace (§6.2). parentNsGen binds
	// child creation to the authorized parent: the engine verifies the
	// parent's incarnation atomically with creation, so a drop/recreate of the
	// parent between authorization and execution cannot place the child under
	// a successor. Zero = no parent guard (depth-1 child of `*`, or auth off).
	CreateNamespace(ctx context.Context, ns string, parentNsGen [16]byte) error

	// DropNamespace removes a namespace and everything in it (§6.2, §5.4).
	// nsGen is the namespace-lifetime guard, verified atomically with the
	// drop; zero = no guard (auth off).
	DropNamespace(ctx context.Context, ns string, nsGen [16]byte) error

	// TableState is the one-snapshot read the API layer resolves scopes with
	// (§6.2): schema (row_access, vectorize field, embed-space identity)
	// together with the Incarnation a later scoped call must pass back. It is
	// also where a text vector query validates its preconditions — vectorize
	// field present, embed-space identity pinned — BEFORE the embedding
	// provider is called, so an invalid query fails without contacting (or
	// billing) the embedder (§7). auth is verified atomically with the read:
	// otherwise a stale grant could fetch the successor's incarnation here and
	// pass it to a later scoped operation, laundering expired authorization
	// through the very call that mints the guard.
	TableState(ctx context.Context, ns, table string, auth []AuthBinding) (*schema.TableSchema, Incarnation, error)

	// ListTables lists the namespace's tables visible through bindings (§6.2):
	// each binding is verified atomically with the listing, and a direct-table
	// binding's (TableNsGen, Table, TableDropGen) is checked per listed table
	// — a same-named successor recreated within the namespace must not appear
	// under a grant naming the predecessor's DropGen.
	ListTables(ctx context.Context, ns string, bindings []AuthBinding) ([]string, error)

	// CreateTable creates a table (§6.2). nsGen is the namespace's creation
	// id: inside the operation's critical section the engine verifies the
	// namespace exists with exactly that id — table creation never creates a
	// namespace implicitly and cannot race a concurrent drop_namespace into
	// recreating one. Zero = no guard (auth off). opts carries the table-level
	// annotations (TableOpts).
	CreateTable(ctx context.Context, ns, table string, fields []schema.Field, opts TableOpts, nsGen [16]byte) (*schema.TableSchema, error)

	// DescribeTable returns the table's schema and its row count, scoped to
	// the caller's visible set (§4.3): scope bounds the count. The zero
	// Incarnation (no guard) is still re-checked inside the operation — §4.3's
	// scope guard binds even to a scope resolved against a predecessor table.
	DescribeTable(ctx context.Context, ns, table string, scope *RowScope, scopeIncarnation Incarnation) (*schema.TableSchema, int64, error)

	// DropTable removes a table and everything dolmen tracks alongside it
	// (§6.2). inc is the full lifetime guard, verified atomically with the
	// drop; zero = no guard (auth off).
	DropTable(ctx context.Context, ns, table string, inc Incarnation) error

	// PlanMigration returns the FULL MigrationPlan a change list would apply
	// (§6.2): operations, destructive changes, and the row-dependent counts
	// (backfill/reindex/embed rows) — values computed against engine-owned
	// data that cannot be rebuilt above the seam. expected is the full
	// Incarnation the plan is made against; a bare version cannot distinguish
	// a same-named successor recreated at version 1 (§4.3), and the zero value
	// is the auth-off compatibility path, intentionally unguarded. scope is
	// the DISCLOSURE scope: the plan's row counts are computed over the
	// caller's visible set while validation remains table-wide; nil =
	// unscoped (auth off, or a table-wide reader). scopeIncarnation is the
	// guard that scope was resolved against, verified inside the operation
	// exactly as for every other scoped operation — the expected migration
	// precondition cannot serve as this guard (a dry run may carry none).
	PlanMigration(ctx context.Context, ns, table string, changes []schema.Change, emb Embedder, expected Incarnation, scope *RowScope, scopeIncarnation Incarnation) (*MigrationPlan, error)

	// Migrate applies a change list, verifying expected (the full Incarnation
	// the plan was made against) inside the apply exactly as PlanMigration
	// does (§6.2). emb re-embeds set_vectorize backfills. The zero value is
	// the auth-off compatibility path.
	Migrate(ctx context.Context, ns, table string, changes []schema.Change, emb Embedder, expected Incarnation) (*schema.TableSchema, error)

	// ListMigrations returns the table's migration history, newest first
	// (§6.2). inc is the table-lifetime guard, verified atomically with the
	// read; zero = no guard (auth off).
	ListMigrations(ctx context.Context, ns, table string, inc Incarnation) ([]Migration, error)

	// Insert inserts records as-is (§6.2, §6.3). scope filters which existing
	// rows may be matched or counted; on Insert it scopes the idempotency
	// replay to the caller's own principal domain — own-domain hit = replay,
	// miss = insert; foreign records neither conflict nor reveal (§4.3's
	// legacy exception: a table-wide caller's miss also replays a matching
	// pre-auth record, so a lost response retried after enabling auth does
	// not duplicate). opts carries the owner stamped on every row-insert path
	// and the idempotency key (WriteOpts). emb embeds vectorize fields on
	// write, passed per call.
	Insert(ctx context.Context, ns, table string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)

	// GetRows is the id-addressed scoped fetch behind read_rows (§2, §6.2):
	// the realtime recovery path (§9.3) and agents generally need by-id reads
	// without raw SQL's namespace-wide gate (§4.4).
	GetRows(ctx context.Context, ns, table string, ids []int64, scope *RowScope, scopeIncarnation Incarnation) (QueryResult, error)

	// UpsertByKey writes records keyed by a natural key (§6.2): a record
	// whose key matches an existing row updates it (partial update);
	// otherwise it inserts and must satisfy required fields. Same scope and
	// WriteOpts rules as Insert — opts.Owner stamps the insert branches too.
	UpsertByKey(ctx context.Context, ns, table string, on []string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)

	// Upsert updates every row matching the filter; when no row matches, the
	// record inserts instead (§6.2). Same scope and WriteOpts rules as
	// Insert.
	Upsert(ctx context.Context, ns, table string, filter string, args []any, record map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)

	// Update returns UpdateResult, not a bare count (§6.2): the count AND the
	// ChangeRange minted by the same transaction — the op layer cannot
	// reconstruct what a mutation changed without leaking engine storage
	// above the seam, and a count alone cannot notify (§9.3). scope filters
	// which existing rows may be matched; emb re-embeds changed vectorize
	// fields.
	Update(ctx context.Context, ns, table string, filter string, args []any, set map[string]any, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (UpdateResult, error)

	// Delete removes rows matching the filter (§6.2). opts carries the safety
	// guard (dry_run, limit, confirm) and the engine enforces the threshold
	// inside the delete transaction — an API-layer preflight would race.
	// DeleteResult keeps the contract's matched/deleted pair and carries the
	// minted ChangeRange like every write result.
	Delete(ctx context.Context, ns, table string, filter string, args []any, opts DeleteOpts, scope *RowScope, scopeIncarnation Incarnation) (DeleteResult, error)

	// ChangesSince is the scoped replay read over the engine-owned durable
	// change log (§6.2, §9.3). table != "" applies the table-authorized feed
	// rule: per-record scope via the Owner label, and only records of the
	// table's CURRENT lifetime (the per-record Lifetime label — a caller
	// granted on a recreated same-named successor can never replay a
	// predecessor's records from an old cursor). table == "" is the namespace
	// feed, guarded by nsGen exactly like Query. Returns records in cursor
	// order plus the next cursor; pages follow Page's conventions.
	ChangesSince(ctx context.Context, ns, table string, from Cursor, nsGen [16]byte, scope *RowScope, scopeIncarnation Incarnation, page Page) ([]ChangeRecord, Cursor, error)

	// Listen is the engine-declared notification capability (B4-style,
	// §6.2, §9.3), cursor-aware so the seam itself provides §9.3's atomic
	// register-and-replay: the engine registers the listener and fixes the
	// replay boundary at the registration point as ONE coordinated operation.
	// See ChangeReplay for the replay contract and the notify gating. The
	// second return value cancels the registration: it releases the listener
	// and stops notify delivery, so a client disconnect cancels cleanly.
	//
	// liveAuthz runs BEFORE a record reaches the caller, and it is LIVE: the
	// engine consults it before exposing each record — enqueuing it for live
	// delivery or returning it on a replay page — passing the record's
	// target table. The API layer's re-resolver returns the caller's CURRENT
	// RowScope and the Incarnation the authorization for THAT table was
	// resolved against (a grant revoked or narrowed mid-stream takes effect
	// at the next event), or ok=false, which teaching-closes the stream —
	// during replay exactly as during live delivery: closed fires with the
	// revocation cause, Next reports done, and no further records are
	// exposed. Replay pages are therefore filtered through the same
	// per-record scope/Owner-label/lifetime rules as live records (§9.3:
	// every event is filtered through the caller's visible set — replay
	// included, never a trust-the-cursor firehose), so foreign records never
	// appear in a replay page either. The per-record table parameter is what
	// makes namespace-wide listeners correct: authorization
	// is resolved per target, never once for the whole stream. The engine
	// stays grant-blind: it filters by the returned scope and the record's
	// Owner label and — for table-filtered feeds — atomically compares the
	// returned incarnation with the record's Lifetime, so a drop/recreate
	// between callback and exposure cannot carry a stale unscoped decision
	// onto the successor's records. Namespace-wide feeds make no per-record
	// table-lifetime comparison (their replay spans table lifetimes by
	// design); they revalidate the namespace authorization and nsGen instead.
	// Foreign records therefore never enter the handoff queue at all — they
	// cannot fill, displace, or starve a scoped subscriber, and overflow
	// reconnects can only ever be triggered by the subscriber's own visible
	// traffic. Engines without the capability degrade — wait_for still meets
	// its bounded-time contract via internal scanning — surfaced through
	// Capabilities like every other engine capability; notification is never
	// the durability mechanism.
	//
	// closed is the engine's terminal signal — the reply path the teaching
	// close needs: the engine invokes it at most once when IT ends the
	// session (authorization revoked via liveAuthz's ok=false, interim
	// buffer overflow with the reconnect recipe, or engine shutdown), with
	// cause carrying the teaching error to surface to the client. After
	// closed fires no further records are delivered and Next reports done;
	// it never fires for a caller-initiated cancel (the caller already
	// knows). nil = no terminal signal requested. notify alone cannot carry
	// a close — it takes only valid records — and a revoked or overflowed
	// subscriber must not hang waiting for a record that never comes.
	Listen(ctx context.Context, ns, table string, from Cursor, nsGen [16]byte, liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool), notify func(ChangeRecord), closed func(cause error)) (*ChangeReplay, func(), error)

	// Query executes a read-only SELECT/WITH statement (§6.2, §4.4). It takes
	// NO scope: the API layer gates raw SQL by table-wide read, which is
	// precisely why no scope parameter exists here. nsGen is the
	// namespace-lifetime guard, verified atomically with execution — a
	// drop-and-recreate between authorization and execution cannot let a
	// predecessor's grant read a successor tenant's data. Zero = no guard
	// (auth off).
	Query(ctx context.Context, ns, sql string, args []any, nsGen [16]byte, page Page) (QueryResult, error)

	// SearchFulltext executes a full-text search (§6.2, §7). includeHidden
	// must cross the seam: truncated is computed against the projected
	// response-byte budget inside the engine, so fetching hidden columns and
	// stripping them above is not equivalent.
	SearchFulltext(ctx context.Context, ns, table, match string, filter string, args []any, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error)

	// SearchVector executes a vector search (§6.2, §7). A text vector query
	// validates against the TableState snapshot (vectorize field present,
	// identity pinned) and embeds BEFORE calling SearchVector — preserving
	// today's error precedence, where an invalid query never reaches the
	// embedding provider. q carries the query vector (raw or freshly
	// embedded), its space identity, and the filter. The result's Execution
	// reports the path that served this query (§7) — exact even on an
	// ANN-capable engine that fell back for this one query.
	SearchVector(ctx context.Context, ns, table string, q VectorQuery, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error)

	// Capabilities is the engine's static self-description (§6.2): the
	// seam-level source for the capabilities op and describe_server's auth:on
	// extension (§2). Its serialization is pinned (EngineCapabilities) so two
	// conforming adapters report the same facts under the same names — the
	// conformance suite compares them — and the op layer publishes it
	// verbatim, never invents it.
	Capabilities() EngineCapabilities

	// Close releases everything the engine holds (§6.2).
	Close() error
}

// AuthBinding is one grant the authorization layer PROVED for a call, as
// handed to the binding-aware reads — NamespaceState, TableState,
// ListNamespaces, ListTables — never caller-supplied (§3.4, §6.2). One
// binding per grant that CONTRIBUTED to the operation's required verbs, plus
// every binding that DERIVES a security-sensitive option — the call site's
// RowScope and WriteOpts.TableWideRead — even when its verb was not required
// to dispatch: verifying only the required-verb bindings would launder a
// stale scope onto a recreated successor. For conjunctive requirements
// (upsert = create AND update; drop_table = schema AND admin) every
// contributing binding is verified: one stale contributor denies the whole
// state acquisition — no arbitrarily selected grant can launder the others.
// Targeted grants mismatch successors; inherited grants verify their
// ANCESTOR's (path, nsGen) while the call returns the target's current
// generation; a Root (*) grant verifies nothing. TableState and the listing
// reads verify the FULL applicable binding: a direct table grant carries the
// complete lifetime key (TableNsGen, Table, TableDropGen) — TableDropGen
// alone is not enough, because a whole-namespace drop and recreate resets
// table drop generations and a same-named successor can repeat the
// predecessor's value; the namespace generation is what separates the two.
type AuthBinding struct {
	Root         bool     // matched a * grant
	Ancestor     string   // ancestor namespace path, when inherited
	AncestorGen  [16]byte // that ancestor's nsGen at grant time
	TargetGen    [16]byte // target's nsGen, for namespace-targeted grants
	Table        string   // table name, for direct table grants
	TableNsGen   [16]byte // the table's NAMESPACE generation at grant time
	TableDropGen int64    // that table's DropGen at grant time
}

// Incarnation identifies one lifetime of a table (§6.3). NsGen is the
// namespace's creation id — a random 128-bit value assigned when the
// namespace is created and persisted in its registry; DropNamespace deletes
// it with the file, so a recreated namespace gets a fresh one and the key can
// never repeat across namespace lifetimes (the drop generation alone cannot
// guarantee this: it lives inside the namespace database and is reset by the
// drop). Version+DropGen distinguish a dropped table's same-named successor,
// which is recreated at version 1 — the store tracks drop generations
// (_dolmen_drop_gen) for exactly this. Table binds the token to its table:
// two same-namespace tables both at (version 1, DropGen 0) share NsGen and
// would otherwise compare equal, so a plan's token could be replayed against
// a different table — the engine verifies Table matches the request's table.
// The zero value means "no guard" (auth off).
type Incarnation struct {
	NsGen   [16]byte
	Table   string
	Version int64
	DropGen int64
}

// RowScope restricts row visibility for one call (§6.3, §4.3). Nil = unscoped
// (auth off, or a table-wide reader). Non-nil = only rows with owner == Owner
// are visible, or no rows at all when Empty is set (schema/admin-only
// describe_table, §4.3). The incarnation a scope was resolved against
// travels as the separate scopeIncarnation argument on every scoped
// operation — separate because the guard must bind even to a nil scope: a
// request resolved before row_access was enabled, or against a predecessor
// table of the same name, must not execute as unscoped afterwards.
type RowScope struct {
	Owner string
	Empty bool
}

// WriteOpts carries the write-path guards and stamps (§6.2, §6.3): the owner
// stamped on EVERY row-insert path — including the upsert insert branches,
// including table-wide callers whose scope is nil (§4.2) — insert's
// idempotency key (empty = plain insert), and TableWideRead, set iff the
// caller holds read through a covering grant: nil scope alone cannot
// distinguish a create-only caller on a default table from a table-wide
// reader (§4.3), and the idempotency replay's legacy-domain consultation
// depends on the difference. The idempotency lookup consults only the
// caller's own principal domain — hit = verbatim replay, miss = insert;
// foreign records neither block nor reveal, TableWideRead included. The
// stamp owner is independent of the scope: a caller may be unscoped yet
// still be the writer.
type WriteOpts struct {
	Owner          string
	IdempotencyKey string
	TableWideRead  bool
}

// TableOpts carries create_table's table-level annotations (§4.1). RowAccess
// is the row_access option: "" (the zero value) means no row filtering — the
// default; "own" is the only value this stream defines, accepted only under
// auth: on. Declared now so enabling it later adds a value, never a new
// parameter — no slice re-signatures CreateTable.
type TableOpts struct {
	RowAccess string
}

// DeleteOpts is the seam's name for the delete safety guard (§6.2,
// §6.3: dry_run, limit, confirm — the engine enforces the threshold inside
// the delete transaction). It aliases the existing DeleteOptions so adapter
// #1 and its callers share one type.
type DeleteOpts = DeleteOptions

// Cursor is a client-facing change-log resume token (§9.3): opaque and
// non-order-revealing, bound to the feed it was minted on and to the
// namespace lifetime that minted it. The zero value starts at the CURRENT
// HEAD — no backlog is replayed; the CursorBegin sentinel starts at the
// retained-history boundary. Engines map tokens to internal positions on
// resume and mint fresh ones per issuance, so consecutive visible records
// yield tokens indistinguishable from adjacent ones and no foreign commit is
// observable through cursor arithmetic. An EMPTY page is not an issuance —
// the position did not move — so the caller's own presented token may be
// returned unchanged (its deadline refreshed by the resolve), keeping a
// polling wait from minting a durable row per poll.
type Cursor string

// CursorBegin is the sentinel requesting retained history from the oldest
// readable boundary — §9.3's pinned request form (no separate begin field).
const CursorBegin Cursor = "begin"

// Page is the paging window shared by Query, the searches, and the change-log
// reads (§6.2's paging conventions). Limit <= 0 selects the operation's
// default page size; engines clamp to the contract maxima (query 1000, search
// 200, changes 1000 — §7, §9.3). Negative Offset is invalid.
type Page struct {
	Offset int
	Limit  int
}

// ChangeKind is the closed vocabulary of change-record kinds (§6.2).
type ChangeKind string

const (
	ChangeInsert ChangeKind = "insert"
	ChangeUpdate ChangeKind = "update"
	ChangeDelete ChangeKind = "delete"
)

// Lifetime is a table's lifetime key as carried on change records (§9.3):
// NsGen, Table, DropGen — Version excluded as everywhere.
type Lifetime struct {
	NsGen   [16]byte
	Table   string
	DropGen int64
}

// ChangeRecord is one entry of the engine-owned durable change log (§6.2,
// §9.3), minted inside the write transaction it describes. Owner is INTERNAL
// authorization metadata stamped from the row: delete events cannot be
// scope-filtered from a row that no longer exists, and historical replay
// cannot consult current row state — so the label rides the record instead.
// It never appears in public payloads unless the row itself would be visible;
// public records project cursor/table/row_id/kind only.
type ChangeRecord struct {
	Cursor   Cursor
	Table    string
	RowID    int64
	Kind     ChangeKind
	Owner    string   // "" when the row carries no owner label
	Lifetime Lifetime // the minting table's lifetime key
}

// ChangeRange identifies the change-log records a single write transaction
// minted (§6.2, §9.3): the per-namespace cursor positions of the first and
// last record, plus the count. Every write result — InsertResult,
// UpdateResult, DeleteResult — carries one; the zero value means the write
// minted no records. The records are read back through the change-log reads'
// paging, NEVER materialized as a slice on the write path: a bulk update
// (filter `1=1`) or a confirmed delete has no match-count cap, and a routine
// mutation must not be able to exhaust server memory constructing a duplicate
// in-memory copy of the durable log it just wrote. Counts stay scalar;
// waiters are woken with the range alone. Positions are engine-internal —
// client-facing cursors are the opaque tokens ChangesSince/Listen hand out.
type ChangeRange struct {
	First int64
	Last  int64
	Count int64
}

// ChangeReplay is the paged replay half of a Listen registration (§6.2,
// §9.3). Replay is paged with the same conventions as ChangesSince —
// authorization included: every replay record passes the session's liveAuthz
// before Next exposes it, so §9.3's per-event visible-set rule holds for the
// replay half exactly as for live delivery — never one slice, because an
// old-but-retained cursor must not let a client force an unbounded
// allocation, and preserving the atomic registration boundary throughout.
//
// The engine does not invoke the session's notify callback until Next has
// drained the replay (reported done): records committing in the interim
// buffer in a bounded queue and are delivered live afterwards, so the
// concatenation replay-then-live is exactly cursor order and a client
// persisting only its last-delivered cursor can never skip older records.
// Boundary dedup across the two halves is the engine's, inside the atomic
// registration. If the caller drains slower than writes arrive and the
// interim buffer bounds, the engine closes the session — through Listen's
// closed callback — with the teaching reconnect recipe: resume from the
// persisted cursor; the log is durable, the buffer never is the durability
// mechanism.
type ChangeReplay struct {
	// Next returns the next page of replay records in cursor order plus the
	// cursor to resume from. The page carrying the final replay records
	// returns done=false; the following call returns done=true with no
	// records — the registration boundary — after which notify delivers live
	// records. Engines must set Next.
	Next func(ctx context.Context) (records []ChangeRecord, next Cursor, done bool, err error)

	// Resume returns the replay's standing cursor — the exact position a
	// caller that stops paging, or never pages, resumes from: the
	// registration-minted cursor before the first page, then each served
	// page's boundary. The handler's terminal frames carry it so a stream
	// ended before its first page still teaches a reconnect that skips
	// nothing. Engines must set it.
	Resume func() Cursor
}

// InsertResult is the shared outcome of the record-writing paths (§6.2):
// Insert (Ids + Replayed; IdempotencyKey replays report Replayed=true),
// UpsertByKey and Upsert (Ids plus their Inserted/Updated counts), and the
// ChangeRange the transaction minted. Fields unused by a path stay zero.
type InsertResult struct {
	Ids      []int64
	Replayed bool
	Inserted int64
	Updated  int64
	Changes  ChangeRange
}

// UpdateResult is the outcome of Update (§6.2): the affected count AND the
// ChangeRange minted by the same transaction — not a bare count, because the
// op layer cannot reconstruct what a mutation changed without leaking engine
// storage above the seam, and a count alone cannot notify (§9.3).
type UpdateResult struct {
	Updated int64
	Changes ChangeRange
}

// QueryResult is the outcome of Query and GetRows (§6.2, §7): rows plus
// truncated, computed against the projected response-byte budget inside the
// engine.
type QueryResult struct {
	Rows      []map[string]any
	Truncated bool
}

// SearchResult is the outcome of both searches (§7): rows plus truncated,
// always computed over the caller's visible set. SkippedVectors counts rows
// whose stored vector could not be scored — a corrupt blob, a dimension
// mismatch, a non-finite component, or a non-BLOB an out-of-band writer left
// in the column — so those rows are absent from Rows and a nonzero count
// means the search is partial (vector searches only; zero for full-text).
// Execution names the path that served the search (§7): SearchVector MUST
// set it — exact or ann — including when an ANN-capable engine falls back to
// exact for this one query (an index still building, a scoped query that
// cannot be safely prefiltered); Capabilities is static and cannot describe
// per-query fallbacks, and §7's per-response execution field is emitted from
// here, never invented above the seam. Zero ("") on fulltext results — the
// execution-path concept is vector-only (§7).
type SearchResult struct {
	Rows           []map[string]any
	Truncated      bool
	SkippedVectors int
	Execution      VectorExecution
}

// VectorQuery is one vector-search request (§6.2, §7). Column selects the
// searched space: "" lets the engine resolve the table's default (the
// vectorize _embedding space when one exists, else the first declared vector
// column). Vec is the query vector — caller-supplied raw, or the product of
// embedding a text query against the TableState snapshot before this call.
// EmbedModel is the embedding provider's identity for text queries ("" when
// the caller supplied the vector directly); the engine pins it against the
// table's embed space. Filter/Args restrict candidates the same way the
// fulltext filter does; MinScore thresholds the cosine (nil = no threshold).
type VectorQuery struct {
	Column     string
	Vec        []float32
	EmbedModel string
	Filter     string
	Args       []any
	MinScore   *float64
}

// VectorExecution is the closed enum naming how the engine executes vector
// search (§6.2, §7) — capabilities' vector_execution field.
type VectorExecution string

const (
	// VectorExact is brute-force exact execution — the conformance reference
	// path.
	VectorExact VectorExecution = "exact"
	// VectorANN is approximate nearest-neighbor execution within a documented
	// recall bound (§7's accelerator exception) — always declared, never
	// silent.
	VectorANN VectorExecution = "ann"
)

// EngineCapabilities is the engine's static self-description (§6.2): the
// seam-level source for the capabilities op and describe_server's auth:on
// extension (§2). Its serialization is pinned so two conforming adapters
// report the same facts under the same names — the conformance suite
// compares it — and the op layer publishes it verbatim, never invents it.
// Unknown future fields are additive (§8.1 rules); the enum values are
// closed.
type EngineCapabilities struct {
	// VectorExecution is never absent: exact or ann.
	VectorExecution VectorExecution `json:"vector_execution"`
	// ANNRecallBound is explicitly null when VectorExecution is exact, and a
	// number (e.g. 0.98) iff ann — never omitted.
	ANNRecallBound *float64 `json:"ann_recall_bound"`
	// Notifications reports whether Listen is implemented.
	Notifications bool `json:"notifications"`
	// Subscribe reports whether streams are available.
	Subscribe bool `json:"subscribe"`
}

// Capabilities is the SQLite adapter's self-description, real since slice 4d
// (§6.2, §7): SearchVector executes brute-force exact — the conformance
// reference path — so vector_execution is "exact" and ann_recall_bound is
// explicitly null, never omitted. Notifications and subscribe flipped to
// true with Listen's body (6b, together with the /v1/subscribe route): the
// registry (notify.go) now backs waiters and live streams, and the
// capability surface, the registered route, and the implemented listener
// changed together, never contradicting each other. A conforming engine must
// never report a capability it does not have, whatever the cost of saying
// false.
func (s *Store) Capabilities() EngineCapabilities {
	return EngineCapabilities{
		VectorExecution: VectorExact,
		ANNRecallBound:  nil,
		Notifications:   true,
		Subscribe:       true,
	}
}
