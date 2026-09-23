# Deploying dolmen

This guide is for whoever runs a dolmen server that more than one person or machine reaches. For a
single agent on one machine, the defaults are already right: authentication off, loopback only.
Flags and environment variables are listed in the README's
[Configuration](../README.md#configuration) table; the normative rules are in
[`design/identity-and-engines.md`](design/identity-and-engines.md) §1–§2.

## Choosing where identity comes from

With `-auth on`, every request must authenticate, and identity can come from four sources. Enable
any mix of them; a request is authenticated when any enabled source accepts it, and nothing after
that point knows or cares which one did.

| Source | Enable with | Suits |
|---|---|---|
| Bootstrap admin key | `DOLMEN_ADMIN_KEY` | the first start, and recovery |
| Gateway headers | `-trusted-proxies` | an existing authenticating proxy (an OAuth proxy in front of Entra or GitHub, a service-token sidecar) |
| API keys | `create_key` | machines: CI jobs, services, agents that cannot sign in |
| Sign-in | `DOLMEN_AUTH_OIDC_ISSUER` or `DOLMEN_AUTH_OIDC_PRESET=github` | people, with no gateway at all |

Two combinations cover most deployments:

- **Gateway tier.** A proxy authenticates people and asserts who they are; dolmen trusts that
  assertion from the proxy's address only. Add API keys for machines that do not pass through the
  proxy.
- **Native tier.** No proxy. People sign in through dolmen, which runs the OIDC flow itself and
  issues a signed token; machines use API keys.

When a request carries a bearer credential and headers both, the bearer wins. Its shape decides
how it is read: a `dlm_` prefix is an API key, a signed-token shape is a sign-in token, anything
else is compared against the admin key. A bearer that fails the reading its shape selects is `401`;
it is never retried as something weaker.

## TLS

dolmen serves plain HTTP and never terminates TLS itself. The admin key, API keys and sign-in tokens
are all bearers: whoever reads one off the wire acts as its principal for as long as it stays
valid, and the sign-in page hands its token to the browser in the response body. Terminate TLS in
front of dolmen, at the gateway in the gateway tier or at a reverse proxy or load balancer in the
native tier. Keep the hop from there to dolmen on a private network, because it carries those
credentials in the clear, and in the gateway tier the identity headers that dolmen trusts by peer
address alone.

Behind a proxy, the URLs dolmen advertises, the sign-in callback among them, take their scheme from
`X-Forwarded-Proto` or `Forwarded`. When the proxy sends neither, set `-base-url` to the `https`
URL; [Reverse proxy / sub-path hosting](../README.md#reverse-proxy--sub-path-hosting) has the
details.

## First start and hand-over

Every deployment starts the same way:

1. Start with `DOLMEN_AUTH=on`, a generated `DOLMEN_ADMIN_KEY`, and whatever sources you want.
2. Using the admin key, grant `admin` on `*` to a real principal: a person's principal as `whoami`
   reports it after they sign in, a principal your gateway asserts, or an API key's principal.
3. Remove `DOLMEN_ADMIN_KEY` from the environment and restart. Grants persist; the bootstrap
   identity does not.

The server refuses to start when it cannot be administered. It needs at least one identity source,
and a **usable root administrator**: the admin key, or a durable grant of `admin` on `*` to a
principal that an enabled source can actually produce. A grant to a group does not count, since
group membership is asserted per request and never stored, with one exception: a group that an
active API key carries, because the key registry proves that membership locally.

Reachability can only be checked for API keys, whose registry is local. For the gateway and
sign-in sources it is **assumed**: dolmen cannot see which principals your gateway will assert or
which accounts your identity provider will still let in. Retiring the account that holds root
`admin` is an operator error no startup check can catch, so keep at least two root
administrators, or an API key held somewhere safe. One mistake is detectable: after
`DOLMEN_AUTH_OIDC_ISSUER` changes, a root grant qualified by the old issuer can no longer be
reached through sign-in, and when no other enabled source reaches it either, startup refuses and
names the grant.

Whatever the lockout, the recovery is the same: set `DOLMEN_ADMIN_KEY` and restart. Revoking the
last usable root grant or key is refused while no replacement exists, so the key is only needed
when the identity itself went away.

## Running behind a gateway

The gateway asserts identity with two headers:

- `X-Dolmen-Principal`: the principal, printable ASCII without spaces, up to 256 characters,
  matched exactly.
- `X-Dolmen-Groups`: optional, comma-separated group names under the same rules, up to 128
  characters each.

dolmen accepts them only from peers inside `-trusted-proxies`, and decides that from the
**immediate TCP peer address**, never from `X-Forwarded-For`, which any client can forge. Behind a
load balancer, list the balancer's addresses. Headers from any other peer are dropped before the
request is handled.

That puts four obligations on the gateway:

- **Overwrite the headers; never pass a client's through.** A proxy that forwards a client-supplied
  `X-Dolmen-Principal` lets anyone claim any identity.
- **Send only the groups that matter.** A group list longer than `-max-groups` (default 128) fails
  the identity with a `401` instead of dropping a group that might carry a grant. Entra users
  routinely carry more than 200 groups, so filter at the gateway rather than raising the limit
  blindly.
- **Terminate streams when the session ends.** A `subscribe` stream authenticated by headers keeps
  the identity it opened with, since dolmen holds no gateway session state. When a user's session
  ends or their groups change, the gateway must close the upstream connection.
  `-max-subscription-age` (default `30m`) is the backstop: at the bound the stream closes with a
  teaching message and the client reconnects from its cursor, which re-asserts the headers.
  Setting it to `0` removes the backstop and is not recommended with the gateway source on.
- **Never assert `dolmen-admin`.** That principal is reserved for the admin key; asserting it
  is a `401`.

## Running sign-in

Register `<base-url>/v1/auth/callback` as the redirect URI with your identity provider and send
people to `/v1/auth/begin`. dolmen derives the callback URL it presents from the request's host
and forwarding headers; when that does not match the registered URI, set `-base-url`.

- **Tokens are stateless.** They are Ed25519-signed and valid for `DOLMEN_AUTH_OIDC_TOKEN_TTL`
  (default `168h`). dolmen stores no users and no sessions, so there is no per-device revocation.
  To sign everyone out, call `rotate_signing_key` with `{"retire_previous":true}`. Without that
  flag, tokens already issued stay valid for their lifetime, which is what a routine rotation wants.
- **Principals are qualified by issuer**, as `oidc:v1:<issuer-digest>:<sub>`, and so are groups.
  Grant on what `whoami` reports, never on an email address. Changing the issuer starts a new,
  ungranted population. Entra sends group claims as object GUIDs, so grant on those or sync names.
- **Streams end with the token.** A stream opened with a token closes when the token expires, and
  the client resumes with a fresh one.
- **Replicas share the signing keys** through the registry. A rotation takes effect immediately
  where it was made and within the 30-second keyring refresh elsewhere. The deployment's issuer
  id is minted on first start; pin it with `DOLMEN_AUTH_OIDC_DEPLOYMENT_ID` if it must stay stable.

## Separating tenants

The same grants drive both of dolmen's tenancy mechanisms, so choose by what must stay private.

- **Structural: a sub-namespace per team.** `acme/team-a` is its own database or schema. A grant on
  it covers that subtree only, and `query` cannot reach across it. Use it when teams must never see
  each other's schema or data.
- **Logical: `row_access: "own"` on a shared table.** Everyone holds the same data verbs on the
  table and each sees the rows they wrote, while a holder of `read` sees them all. Use it when
  people share a table but keep private rows, such as usage telemetry where every user appends with
  `create` and only the developer holds `read`.

Do not give each user a namespace. It multiplies namespaces, and it rules out the shared tables the
logical mechanism exists for.

## What stays open, and what stays local

`/livez`, `/readyz`, `/healthz`, `/version`, `/skills*` and `/v1/openapi.json` answer without a credential in every
mode. They carry no row data: probes, and the documents clients discover the API from.

`dolmen mcp`, the stdio transport, refuses to start with `-auth on`. A pipe carries no per-request
credential, and treating whoever launched the process as an administrator would be a silent
bypass. Stdio is reachable only by its parent process, so run it with auth off.

## Timeouts

dolmen bounds each connection and each operation, and every bound has a knob: `-read-timeout`,
`-write-timeout`, `-idle-timeout` and `-op-timeout` default to two minutes, and `-migrate-timeout`
leaves migrations unbounded. An operation past its bound is stopped, rolled back if it had not
committed, and answered `504` with code `timeout`. Two interactions with a proxy in front:

- **The proxy's upstream timeout must outlast dolmen's.** Give it at least `-op-timeout` plus a
  minute, since `wait_for` holds a request for up to 60 seconds on top of the operation bound, and
  as long as the largest migration you run. Otherwise the proxy answers first, and the client gets
  a bare gateway error instead of dolmen's teaching one. `subscribe` streams need the proxy's read
  timeout above the 20-second keepalive.
- **dolmen's idle timeout must outlast the proxy's.** A load balancer that reuses a keep-alive
  connection dolmen has just closed turns the next request into a `502`, so set `-idle-timeout`
  above the balancer's own idle timeout.

## Backups

Back up with `dolmen backup`, which snapshots every namespace and the grant registry while the
server keeps serving, and restore with `dolmen restore`, which verifies a backup before it writes
anything and never overwrites a namespace. The README's
[Backup and restore](../README.md#backup-and-restore) section has the commands.

## Change retention

`-change-retention` (default `168h`) bounds how long a cursor from `changes_since`, `wait_for` or
`subscribe` stays usable, and the change log keeps records for up to twice that. A client that
reconnects later gets a teaching error and restarts from the head. `0` disables pruning:
records accumulate and cursors never expire, which costs disk and nothing else.

## Open namespaces and file descriptors

Each namespace is a SQLite file, and an open one holds a single write connection plus a read pool
of up to 16 connections, two of which stay open while idle. A connection keeps about three file
descriptors: the database, its write-ahead log, and the shared-memory index. An idle namespace
therefore costs around nine descriptors, and a busy one up to about fifty.

`-max-open-namespaces` (default `128`) caps how many namespaces stay open. Opening one past the cap
first closes the least recently used idle namespace, and that namespace reopens on its next
request at the cost of a few milliseconds. A namespace in use is never closed: an operation holds
its namespace until it returns, and a `subscribe` stream holds it until the stream ends. The cap
therefore bounds the idle namespaces, and the open count can exceed it by the number in use at
that moment. Size the process's descriptor limit for the cap times nine, plus fifty for each
namespace you expect to be busy at once.

No connection has an idle timeout while its namespace is open; the read pool only closes
connections beyond the two it keeps. A read-only connection cannot recreate the shared-memory file
SQLite removes when the last connection to a database closes, so the write connection stays open
for as long as the namespace does.

## Durability

`-sync` (default `full`) sets what an acknowledged write survives. Every namespace runs in WAL
mode, so a crash never leaves a namespace inconsistent; the setting only decides which of the
latest commits a crash can take with it.

| Mode | A process crash (kill, panic, OOM) | Power or OS failure |
|------|------------------------------------|---------------------|
| `full` | loses nothing acknowledged | loses nothing acknowledged |
| `normal` | loses nothing acknowledged | may lose the last commits acknowledged before it |

`full` syncs the write-ahead log on every commit; `normal` syncs it only at checkpoints, which
makes small writes faster. Choose `normal` only where the last moments of writes can be replayed
from elsewhere. The mode is logged at startup, and a namespace whose writer does not report the
chosen mode when it opens is refused rather than served with weaker durability. The Go library
always uses `full`. The grant registry (`_grants.db`) always uses `full` too: a revoked
grant or key must stay revoked after a power loss.

## Probes

`/livez` answers `200 {"status":"ok"}` while the process runs; `/healthz` is the same probe under
its old name. Point liveness checks at it.

`/readyz` answers `200 {"status":"ready","embedding":{...}}` when the server is safe to route
to, and `503 {"status":"not_ready","reasons":[...],"embedding":{...}}` while it drains for
shutdown, when the data directory is not writable, or when a namespace found unreadable at
startup still cannot be read; repairing or removing the file clears that without a restart. On
PostgreSQL it checks that the database answers and the catalog schema exists. The probe creates no namespace and scans no table. `embedding` names the provider and
reports `configured` or `none` without calling it, because only embedding operations depend on
it: a failing provider degrades vector writes and text vector search, not readiness.

## Shutting down

On SIGTERM or an interrupt the server stops accepting connections and `/readyz` turns
`not_ready`, and each open `subscribe` stream ends with a close frame carrying its resume cursor.
Running requests get `-shutdown-grace` (default `60s`) to finish. Past it they are cancelled,
which rolls back any open transaction, including a migration's, so a write either committed
before the answer or did not happen. The store is then closed, and a close or checkpoint error
is reported alongside the drain result: the process exits non-zero if either failed. Each phase
is logged with the number of requests still running.

