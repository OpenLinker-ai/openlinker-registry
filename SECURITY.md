# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's "Report a vulnerability"
form on this repository (Security → Advisories). Do not open a public issue.
Include the affected version or commit, the configuration that matters
(instance mode, upstreams, reverse proxy) and steps to reproduce.

## What the registry guarantees

- **Integrity.** Hosted artifacts are addressed by sha256. Indexed and
  mirrored content is verified against its recorded digest (and, for mirrored
  skills, its file manifest) before any byte is served.
- **Visibility.** `private` and `internal` entries are filtered on the server
  on every read path, including discovery indexes and the MCP Registry API.
- **No execution.** Archives are unpacked in memory with limits on entry
  count and size; paths are validated; nothing from a skill or a `server.json`
  is executed.
- **Outbound requests.** Requests to GitHub and to upstream registries never
  follow redirects and are size-capped. Upstream addresses are checked after
  DNS resolution; non-public addresses are refused unless explicitly allowed.
  Upstream tokens are read from the environment, never stored, and sent only
  to the upstream's own origin.
- **Secrets.** Only the sha256 of an API token secret is stored. A
  `server.json` carrying a literal secret value is rejected (or, when
  mirrored, has the value removed).

## What it does not guarantee

The registry proves where bytes came from and that they were not modified. It
does not review content: a skill or an MCP server listed here is not vouched
for as safe. Mirrored entries are as trustworthy as the upstream they came
from.

## Operating notes

- Keep `REGISTRY_ADMIN_TOKEN` out of day-to-day use; issue scoped tokens.
- Terminate TLS in front of the registry and set `REGISTRY_PUBLIC_URL` to the
  https URL so the page session cookie is marked `Secure`.
- Set `REGISTRY_TRUSTED_PROXIES` to your reverse proxy so rate limits apply
  per client rather than per proxy.
- When the registry shares an origin with another site, leave the built-in
  pages off and strip cookies at the proxy, as the public OpenLinker instance
  does.
