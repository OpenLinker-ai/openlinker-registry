# Changelog

The registry has its own version numbers; see [RELEASE.md](./RELEASE.md).

## Unreleased

No version has been released yet. `main` contains:

- Hosted skills: upload, GitHub import and index, immutable versions,
  canonical archives, visibility (`public`, `internal`, `private`), API
  tokens, namespaces and roles, audit log.
- Discovery indexes for `npx skills add` (site-wide and per namespace).
- MCP servers: hosted `server.json` documents under `/v1/mcp`, and the
  official MCP Registry read API under `/v0.1` and `/v0`. A document carrying
  a literal secret value is rejected.
- Upstreams (pull only): forwarding to and mirroring of other
  openlinker-registry instances, the official MCP Registry and discovery
  indexes, with digest verification before any byte is served.
- Rate limits per client address behind trusted proxies, and a lockout for
  repeated failed authentication.
- Built-in read-only pages for private instances.
- `openlinker-registry version`, a release workflow started by a `v*` tag
  push (multi-platform image and archives with SHA-256 files), and the
  packaging script it uses.

Database migrations `0001` to `0004` are applied at startup and are
forward-only.
