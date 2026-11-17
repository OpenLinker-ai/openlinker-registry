# Changelog

The registry has its own version numbers; see [RELEASE.md](./RELEASE.md).

## Unreleased

No version has been released yet. `main` contains:

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
  mirroring of discovery indexes. Mirrored skill content is verified against its recorded digests
  before it is served; upstream MCP documents are validated and have
  credentials removed, and their digest is computed by this registry.
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
