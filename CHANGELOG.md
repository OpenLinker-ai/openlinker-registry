# Changelog

The registry has its own version numbers; see [RELEASE.md](./RELEASE.md).

## Unreleased

- Projection (OpenLinker Core migration, R6): gateways marked `projection`
  (`PATCH /v1/admin/gateways/{consumer}`) may, with a user assertion carrying
  the new `project` action, ingest Core bundles as hidden immutable versions
  (`PUT /v1/projections/skills/{name}/versions/{v}`), apply a revisioned
  complete state to an entry (`PUT /v1/projections/skills/{name}`,
  `PUT /v1/projections/mcp/{name}`), validate bundles and preflight
  namespaces. Projected entries live in the owner's `ai.openlinker.*`
  namespace (a generated `ai.openlinker.u<id>` when the owner has none,
  reserved from self-service), are managed by that gateway and owner only,
  skip the initial review but keep later moderation holds, and can hide whole
  entries or single versions from everyone but administrators. States with
  an older `(revision, phase)` change nothing; a hidden state for a missing
  entry leaves a tombstone. `LicenseRef-openlinker-platform-terms` is accepted
  for projected versions only. Migration `0007`.

- Gateways: an API token registered as a gateway can read on behalf of its
  users with a signed, single-use, request-bound Ed25519 user assertion
  (`OpenLinker-User-Assertion`). Admin endpoints `/v1/admin/gateways`; role
  grants accept `openlinker:<consumer>:<uuid>` and `github:<id>` subjects;
  `GET /v1/whoami` shows the asserted user.
- `GET …/artifact?purpose=import` lets a gateway user take a private copy of
  an indexed version whose license withholds redistribution.
- `REGISTRY_ASSERTION_AUDIENCE` (default: the public URL).
- Migration `0005` (gateways, gateway keys, used assertion ids).
- Review queue (`REGISTRY_REVIEW`, on by default in public mode): the first
  entries of a namespace that is not yet trusted, and every indexed entry and
  each of its new versions, wait for an administrator. An entry under review is
  readable only by its namespace members (never through a gateway's shared
  namespace roles) and administrators, on every read path; replication metadata
  presents it as `private`, so downstream mirrors do not copy it.
  `GET /v1/admin/review`, `POST /v1/admin/review/skills/{ns}/{name}` and
  `…/mcp/servers/{ns}/{name}` (`approve` or `reject` the `review_revision` that
  was reviewed, optionally trusting the namespace), `PATCH /v1/admin/namespaces/{ns}`.
- Reports: `POST /v1/reports` for signed-in callers; distinct open reports
  reaching `REGISTRY_REPORT_THRESHOLD` (default 3) send an approved entry back
  to review (only while review is on; otherwise reports are only recorded).
  `GET /v1/admin/reports`, `POST /v1/admin/reports/{id}/resolve`.
- Namespace self-service through a gateway: `POST /v1/namespaces` for
  `ai.openlinker.<handle>` (one per user, public instance) and
  `io.github.<login>` (login looked up by the asserted numeric GitHub id;
  personal accounts only).
- Claims: `POST /v1/skills/{ns}/{name}/claim` moves an entry indexed from a
  GitHub repository owned by the asserted personal account into its
  `io.github.<login>` namespace. Indexed versions now record the repository's
  numeric id, and claims are checked against that id; older entries cannot be
  claimed. The old coordinates answer `308` and stay reserved; writes
  authorized before a claim cannot land after it.
- Migration `0006` (review state, trusted namespaces, reports, redirects). All
  existing entries are approved.

## v0.1.0 — 2026-11-18

The first formal release. The registry behaves as in `v0.1.0-rc.1`; the
changes since then are to how it is built:

- Built with Go 1.27: `go.mod` now requires it, so the image
  (`golang:1.27-bookworm`), the release archives and CI all use the same Go
  release. In `v0.1.0-rc.1` everything was built with Go 1.26.
- `github.com/jackc/pgx/v5` 5.13.0 (was 5.12.0).
- Workflows use `actions/checkout@v7` and, in CI as already in the release
  workflow, `actions/setup-go@v7`.

## v0.1.0-rc.1 — 2026-11-17

Pre-release, and the first tagged version. There is no formal release yet. It
contains:

- Skills: hosted uploads and GitHub imports, GitHub-indexed entries (a
  locator, manifest and digests instead of stored content), immutable
  versions, canonical archives, visibility (`public`, `internal`,
  `private`), API tokens, namespaces and roles, audit log.
- Discovery indexes for `npx skills add` (site-wide and per namespace).
- MCP servers: hosted `server.json` documents under `/v1/mcp`, and the
  official MCP Registry read API under `/v0.1` and `/v0`. A document carrying
  a literal secret value is rejected.
- Upstreams (pull only): forwarding to and mirroring of other
  openlinker-registry instances and the official MCP Registry, plus
  mirroring of discovery indexes. Mirrored skill content is verified against
  its recorded digests before it is served; upstream MCP documents are
  validated and have credentials removed, and their digest is computed by
  this registry.
- Read, write and forwarding rate limits, counted per token subject for
  authenticated callers and per client address for anonymous ones (behind
  trusted proxies, the address the proxy reports), and a per-address lockout
  for repeated failed authentication.
- Built-in read-only pages for private instances.
- `openlinker-registry version`, a release workflow started by a `v*` tag
  push (multi-platform image and archives with SHA-256 files), and the
  packaging script it uses.

Database migrations `0001` to `0004` are applied at startup and are
forward-only.
