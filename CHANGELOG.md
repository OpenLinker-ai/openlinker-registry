# Changelog

The registry has its own version numbers; see [RELEASE.md](./RELEASE.md).

## Unreleased

Nothing since `v0.1.0`.

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
