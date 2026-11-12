# openlinker-registry

A standalone registry for Agent Skills and MCP servers. One binary, one
PostgreSQL database, a directory for blobs.

It **stores** what you publish to it:

- **Hosted skills**: immutable, content-addressed versions you upload or import
  from a fixed GitHub commit.
- **Indexed skills**: a locator, file manifest and digest for content that
  stays at its source (a GitHub directory, or an entry of another site's
  discovery index).
- **MCP servers**: `server.json` documents, registered by the namespace owner
  or indexed by someone else, with versions and status.

It **forwards to and mirrors other registries** (upstreams): another
openlinker-registry, any registry that speaks the official MCP Registry API
(including `registry.modelcontextprotocol.io`), or a site that publishes an
[agentskills.io discovery index](https://schemas.agentskills.io/discovery/0.2.0/schema.json).
A read of something that is not stored locally can be forwarded to an
upstream; upstreams can also be mirrored on a schedule. Content only flows
in: nothing stored here is ever pushed to an upstream.

It **serves standard clients**: discovery indexes for `npx skills add`, and
the official MCP Registry read API under `/v0.1` (and `/v0`).

The same binary serves a public instance (anonymous reads) and private
deployments (authenticated reads by default, with built-in web pages).

> Status: source only. Not released. See [Not included](#not-included).

## Quick start (private instance)

```sh
cd deploy
cp env.example .env        # set REGISTRY_POSTGRES_PASSWORD and REGISTRY_ADMIN_TOKEN
docker compose up -d --build
```

Open `http://localhost:8080/ui/` and sign in with the admin token. Then, with
`ADMIN=$REGISTRY_ADMIN_TOKEN` and `R=http://localhost:8080`:

```sh
# A namespace and a publishing token for it.
curl -sS -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"internal.team","kind":"internal"}' $R/v1/admin/namespaces
curl -sS -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"ci","scopes":["skills:read","skills:write"],"roles":[{"namespace":"internal.team","role":"publisher"}]}' \
  $R/v1/admin/tokens          # the token is shown once

# Store a skill and an MCP server.
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/markdown' \
  --data-binary @SKILL.md "$R/v1/skills/internal.team/my-skill/versions?version=1.0.0"
curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data-binary @server.json $R/v1/mcp/servers/internal.team

# Forward to and mirror the official MCP Registry.
curl -sS -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"official","kind":"mcp-registry","base_url":"https://registry.modelcontextprotocol.io",
       "proxy_on_miss":true,"latest_only":true,"sync_interval_seconds":3600}' $R/v1/admin/upstreams
```

## Boundary

- Skill and MCP content is metadata and bytes. The registry never runs an MCP
  server, never executes skill content and never stores secret values such as
  authentication header values.
- No dependency on any OpenLinker module (SDK, Core, Cloud, CLI, Node or
  Plugin). Dependencies are the Go standard library, `jackc/pgx/v5` and
  `gopkg.in/yaml.v3`. OpenLinker Core is expected to talk to the registry over
  HTTP only.
- The registry never executes skill content. It proves where bytes came from
  and that they were not modified; it does not vouch for their safety.
- Authorization, scheduling and runs stay in OpenLinker Core.

## Building and running

```sh
make build          # bin/openlinker-registry (GOWORK=off is set by the Makefile)
make check          # gofmt check, go vet, go test
make docker         # local image
```

```sh
export REGISTRY_DATABASE_URL='postgres://registry@localhost:5432/registry?sslmode=disable'
export REGISTRY_PUBLIC_URL='https://example.com/registry'
export REGISTRY_ADMIN_TOKEN="$(openssl rand -base64 32)"
bin/openlinker-registry -base-path /registry -mode public
bin/openlinker-registry migrate     # apply migrations and exit
```

Migrations are embedded and applied at startup unless `REGISTRY_AUTO_MIGRATE=false`.

### Configuration

Every flag has an environment variable (`REGISTRY_<NAME>`); flags win.
Secrets are read from the environment only.

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `REGISTRY_LISTEN` | `-listen` | `:8080` | Listen address |
| `REGISTRY_BASE_PATH` | `-base-path` | `""` | Path prefix such as `/registry` |
| `REGISTRY_DATABASE_URL` | `-database-url` | required | PostgreSQL DSN |
| `REGISTRY_STORAGE_DIR` | `-storage-dir` | `./data` | Local content-addressed blob store |
| `REGISTRY_PUBLIC_URL` | `-public-url` | required | External URL of the registry root; used in discovery URLs. When a base path is set, its path must end with the base path |
| `REGISTRY_MODE` | `-mode` | `private` | `public` or `private` |
| `REGISTRY_ALLOW_ANONYMOUS_READ` | `-allow-anonymous-read` | `true` if public, else `false` | Anonymous read access |
| `REGISTRY_ALLOW_PUBLIC_VISIBILITY` | `-allow-public` | `true` | Allow `public` visibility |
| `REGISTRY_ADMIN_TOKEN` | env only | unset | Bootstrap admin token (>= 32 chars) |
| `REGISTRY_GITHUB_TOKEN` | env only | unset | Optional read-only GitHub token (sent to the API host only) |
| `REGISTRY_GITHUB_API_URL` | `-github-api-url` | `https://api.github.com` | GitHub API base |
| `REGISTRY_GITHUB_RAW_URL` | `-github-raw-url` | `https://raw.githubusercontent.com` | Raw content base |
| `REGISTRY_GITHUB_WEB_HOST` | `-github-web-host` | `github.com` | Host accepted in source URLs |
| `REGISTRY_GITHUB_TIMEOUT` | `-github-timeout` | `20s` | Per-request timeout |
| `REGISTRY_MAX_UPLOAD_BYTES` | `-max-upload-bytes` | 16 MiB | Upload body limit |
| `REGISTRY_MAX_FILES` / `_MAX_FILE_BYTES` / `_MAX_TOTAL_BYTES` | flags | 256 / 2 MiB / 16 MiB | Skill limits |
| `REGISTRY_CACHE_MAX_AGE` / `_CACHE_MAX_BYTES` | flags | `720h` / 1 GiB | Evictable cache for indexed content |
| `REGISTRY_VERIFY_INTERVAL` | `-verify-interval` | `1h` | Cached indexed content is served only this long after the last successful source verification (`0` = verify on every read) |
| `REGISTRY_ARTIFACT_GC_GRACE` | `-artifact-gc-grace` | `24h` | Unreferenced hosted artifacts (left by failed publishes) older than this are deleted hourly. GC and publishes of the same digest are serialized by an in-process per-digest lock, so one registry process must own a storage directory |
| `REGISTRY_UI` | `-ui` | `true` if private, else `false` | Built-in web pages under `/ui/` |
| `REGISTRY_UPSTREAM_TOKEN_<NAME>` | env only | unset | Bearer token for the upstream `<name>` (upper-cased, `-` as `_`). Never stored in the database; sent only to that upstream's own origin |
| `REGISTRY_UPSTREAM_ALLOW_CIDRS` | `-upstream-allow-cidrs` | empty | Private networks upstreams may live in. Public addresses are always allowed; everything else is refused at connect time |
| `REGISTRY_UPSTREAM_ALLOW_HTTP` | `-upstream-allow-http` | `false` | Allow `http://` upstream URLs |
| `REGISTRY_UPSTREAM_TIMEOUT` | `-upstream-timeout` | `30s` | Per-request timeout for upstreams |
| `REGISTRY_BACKGROUND_SYNC` | `-background-sync` | `true` | Run scheduled upstream syncs in this process |
| `REGISTRY_RATE_LIMIT_READ` / `_WRITE` / `_FORWARD` | flags | 600 / 60 / 30 | Requests per client per minute: reads, writes, and misses forwarded to upstreams (`0` = unlimited). Clients are the token subject, or the address when anonymous. Limits are per process |
| `REGISTRY_TRUSTED_PROXIES` | `-trusted-proxies` | empty | Reverse proxies whose `X-Forwarded-For` identifies the client |

## API summary

All paths are relative to the base path. Authentication is
`Authorization: Bearer olr_<id>_<secret>` (or the bootstrap admin token).
Errors are `{"error":{"code","message"[,"source_url"]}}`.

### Read

| Method and path | Notes |
|---|---|
| `GET /v1/skills?q=&namespace=&mode=&compatible=&limit=&cursor=` | List/search, filtered by caller visibility; unlisted and yanked skills excluded |
| `GET /v1/skills/{ns}/{name}` | Detail and versions |
| `GET /v1/skills/{ns}/{name}/versions/{v}` | Manifest, license, source/locator, check result, content access |
| `GET …/versions/{v}/artifact` | Stored artifact (`SKILL.md`, canonical tar, or Core bundle JSON) |
| `GET …/versions/{v}/archive.tar.gz` | Deterministic `.tar.gz` of the canonical tar (used by discovery) |
| `GET …/versions/{v}/files/{path}` | One UTF-8 file as `text/plain` |
| `GET /.well-known/agent-skills/index.json` | Site index: only entries with an admin-assigned global alias |
| `GET /ns/{ns}/.well-known/agent-skills/index.json` | Namespace index |
| `GET /healthz`, `GET /readyz` | Liveness, database readiness |

Visibility: `public` (anyone), `internal` (any authenticated caller with a
read or write scope), `private` (callers with a role in the namespace).
Invisible entries return 404 on every read path.

### Publish (publisher role and `skills:write`)

| Method and path | Notes |
|---|---|
| `POST /v1/skills/{ns}/{name}/versions?version=&visibility=&listed=&license=` | Body is the artifact. `Content-Type`: `text/markdown` (single SKILL.md), `application/x-tar`, `application/gzip`, `application/zip`, or `application/vnd.openlinker.skill-bundle.v1+json` (canonical Core bundle; digest-compatible with Core) |
| `POST /v1/skills/{ns}/github-imports/preview` | `{"url","ref"}` → commit, tree SHA, manifest, digests. No writes |
| `POST /v1/skills/{ns}/github-imports` | Hosted import; requires `commit` and `expected_tree_sha` from the preview |
| `POST /v1/skills/{ns}/indexes/preview`, `POST /v1/skills/{ns}/indexes` | Same, but indexed: only locator, manifest and digests are stored |
| `POST /v1/skills/{ns}/{name}/deprecate`, `…/yank`, `…/versions/{v}/yank` | Optional `{"reason"}` |
| `PATCH /v1/skills/{ns}/{name}` | Owner role: `{"visibility","listed"}` |

### MCP servers

| Method and path | Notes |
|---|---|
| `GET /v1/mcp/servers?q=&namespace=&mode=&version=&limit=&cursor=` | One row per server at its latest version (`version=all` for every version) |
| `GET /v1/mcp/servers/{ns}/{name}`, `…/versions/{v}` | Detail with versions; one version (`latest` allowed) |
| `POST /v1/mcp/servers/{ns}?visibility=&listed=&indexed=` | Body is a `server.json`; its name must be `{ns}/…`. `indexed=true` marks an entry not published by the server's author. Publisher role |
| `POST /v1/mcp/servers/{ns}/{name}/status`, `…/versions/{v}/status` | `{"status":"active\|deprecated\|deleted","message"}` for all versions or one |
| `PATCH /v1/mcp/servers/{ns}/{name}` | Owner role: `{"visibility","listed"}` |
| `GET /v0.1/servers?search=&cursor=&limit=&updated_since=&version=&include_deleted=` | Official MCP Registry list: one record per version ordered by name then version; `version=latest` for latest only; `updated_since` includes deletions |
| `GET /v0.1/servers/{serverName}/versions`, `…/versions/{version\|latest}` | Official read endpoints; `serverName` is URL-encoded (`io.github.a%2Fb`); a decoded slash is accepted too |
| `POST /v0.1/publish` | Body is a `server.json`; authorized by a registry token with the publisher role in the name's namespace |

Every `/v0.1` path is also served under `/v0`. Responses follow the official
shapes (`{"servers":[{"server":…,"_meta":{"io.modelcontextprotocol.registry/official":…}}],"metadata":{"nextCursor","count"}}`,
errors `{"error":"…"}`); this registry's own facts (mode, upstream, digest) are
under `_meta["ai.openlinker.registry/entry"]`.

### Admin (`admin` scope or bootstrap token)

`POST /v1/admin/namespaces`, `GET /v1/admin/namespaces`,
`POST /v1/admin/namespaces/{ns}/roles`,
`POST /v1/admin/tokens` (returns the plaintext once), `GET /v1/admin/tokens`,
`POST /v1/admin/tokens/{id}/revoke`,
`PUT /v1/admin/skills/{ns}/{name}/alias`,
`POST /v1/admin/skills/{ns}/{name}/versions/{v}/approve-license`, `GET /v1/admin/audit`.
`GET /v1/whoami` describes the caller.

Upstreams: `GET|POST /v1/admin/upstreams`, `GET|PATCH|DELETE /v1/admin/upstreams/{name}`,
`POST /v1/admin/upstreams/{name}/sync[?wait=true]`.

## Upstreams: forwarding and mirroring

```json
{"name": "hub", "kind": "openlinker-registry", "base_url": "https://registry.example.com",
 "proxy_on_miss": true, "sync_interval_seconds": 3600, "visibility": "internal",
 "namespace_allow": ["com.example.*"], "namespace_deny": [], "priority": 100}
```

| Kind | Upstream | What is mirrored |
|---|---|---|
| `openlinker-registry` | Another openlinker-registry | Skills (through `/v1`) and MCP servers (through `/v0.1`) |
| `mcp-registry` | Any registry with the official MCP read API | MCP servers; `latest_only` keeps one version per server |
| `agent-skills` | A site with `/.well-known/agent-skills/index.json` | Its skills, into the local `target_namespace`; each digest becomes version `sha-<12 hex>` |

- **Forward on miss** (`proxy_on_miss`): a direct read of a skill or MCP
  server that is not stored here is looked up on the upstreams, in priority
  order, and the answer is mirrored. Listings and search only show what is
  already here, so use a sync interval for a complete catalogue.
- **Sync** (`sync_interval_seconds`, or `POST …/sync`): MCP servers are synced
  incrementally with `updated_since`; skills are listed in full and details
  fetched only for those that changed. Status changes, yanks and deletions
  propagate. One run per upstream is active at a time, across replicas.
- **Bytes are pulled through and verified.** Mirroring a skill records its
  metadata; the artifact is fetched from the upstream on first read, checked
  against the recorded sha256 digest and file manifest, then cached. A
  mismatch is recorded as `source_changed` and nothing is served.
- **A mirror serves no more than its source.** Whether artifact bytes and
  file text may be served is taken from the upstream's own answer.
- **Ownership.** Mirrored entries keep their upstream namespace, which
  becomes read-only here and belongs to that one upstream. A namespace that
  exists locally is never forwarded or overwritten; conflicts are counted in
  the sync result. Mirrored entries are `public` or `internal`, never private.
- **Outbound safety.** Upstream URLs must be https and resolve to public
  addresses unless explicitly allowed; the connected address is checked after
  DNS resolution, redirects are not followed, responses are size-capped.
  Tokens are sent only to the upstream's own origin. URLs found inside an
  upstream's data are fetched under the same policy, without the token unless
  they are on the upstream's origin.
- **MCP documents** from upstreams are validated leniently; literal secret
  values are removed before storing, so the stored digest is this registry's.

## Built-in pages

Private instances serve read-only pages under `/ui/`: skills, MCP servers
(with a client configuration snippet), and upstream status for administrators.
English and Chinese, chosen by `Accept-Language`. No scripts and no third-party
resources. Sign-in takes an API token and keeps it in an `HttpOnly`,
`SameSite=Strict` cookie scoped to `/ui/`; the JSON API never reads that
cookie. The public instance leaves pages to the site in front of it
(`REGISTRY_UI=true` enables them when the registry has its own origin).

## Key rules

- **MCP servers.** Names follow the official pattern `<namespace>/<name>`; the
  namespace part (lowercased) must be a namespace of this registry, and names
  that differ only by letter case are rejected. Published documents are
  validated against the official `server.json` rules (2025-12-11), stored in
  canonical form, and identified by the sha256 of that form. Versions are
  immutable; only status changes. "Latest" is the highest semantic version
  that is not deleted, or the most recently published when versions are not
  semantic. A document with a literal secret value is rejected.
- **Names.** Skill names are slugs (`[a-z0-9]+(-[a-z0-9]+)*`, max 64).
  Namespaces are lowercase reverse-DNS: `io.github.<login>` (bound to the
  numeric GitHub id, which must be unique), `ai.openlinker.<handle>` (public
  instance only), `internal.<team>` (private instance only) or a reverse
  domain (verification not implemented yet).
- **Immutability and audit.** Versions never change; only review, check and
  yank fields may be updated, enforced by database triggers (UPDATE, DELETE and
  TRUNCATE). The audit table is append-only, and every mutation writes its
  audit row in the same transaction: if auditing fails, the request fails.
- **Archives.** Uploads are bounded by file count, per-file and total size,
  and a cap on every decompressed byte (headers, PAX/GNU metadata, padding).
  Gzip streams must end cleanly with a valid checksum; trailing data is
  rejected.
- **Caching.** Only anonymous requests for public content on an instance with
  anonymous reads get `Cache-Control: public, max-age=60`; everything else is
  `private, no-store`. Responses carry `Vary: Authorization`.
- **GitHub.** URLs take the ref as one path segment; refs containing `/` are
  rejected (use a commit SHA). The registry resolves to a 40-hex commit, lists
  `git/trees/{commit}:{path}?recursive=1`, rejects truncated trees, symlinks
  and submodules, enforces limits before downloading, fetches raw bytes from the
  raw host and verifies each file's git blob SHA-1 over the raw bytes. Imports
  must present the previewed `commit` and `expected_tree_sha`. Outbound requests
  go only to the configured API and raw hosts and never follow redirects.
- **Canonical tar.** Paths sorted bytewise, explicit directory entries, files
  0644 / directories 0755, uid/gid 0, empty user/group names, mtime 0, no PAX
  or GNU headers, over-long paths rejected, two trailing zero blocks. Written by
  the registry's own encoder so bytes cannot drift with Go releases.
- **Licenses (indexed content).** Redistributable SPDX: full text and cached
  artifact served. `custom`: only after admin approval. `unknown`: metadata,
  manifest and source link only. Hosted `public` skills need an SPDX license or
  `custom` with non-empty text (`license_text` or a root LICENSE file), which is
  stored and returned as `license.text`; `unknown` can only be `internal` or
  `private`. Indexed entries take the license from the SKILL.md frontmatter.
  Indexed content is re-verified against the source once the verification
  interval has passed, even if the bytes are cached.
- **Discovery.** Only `public`, listed, active skills whose latest non-yanked
  version is eligible: hosted entries, single-file indexed entries (raw URL at
  the commit), and multi-file indexed entries whose license permits
  redistribution and whose last source check did not fail. No URL contains
  credentials or signed parameters. A namespace index for an unknown namespace
  is the same empty index as for a namespace without public entries.
  Duplicate-source conflicts do not name the existing entry.

## Not included

OpenLinker Core integration (catalog proxy, gateway assertions, import into
Core), GitHub OAuth login and namespace self-service, GitHub Actions OIDC,
enterprise OIDC single sign-on, DNS/HTTP namespace verification, claims and
shares, review queues and abuse reports, offline bundles, publisher
signatures, S3 storage (the interface exists; blobs are on local disk, so one
process owns a storage directory), rate limits shared between replicas,
indexing a single arbitrary URL by hand (discovery indexes are synced as
upstreams), write access from the built-in pages.

## Tests

`go test ./...` runs unit tests and the end-to-end HTTP suite on an in-memory
store with a fake GitHub server (no network). Set
`REGISTRY_TEST_DATABASE_URL` to also run the same suite and trigger tests on
PostgreSQL; each test uses its own schema and drops it afterwards.

## License

Apache-2.0
