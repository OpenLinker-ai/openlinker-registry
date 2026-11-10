# openlinker-registry

A standalone registry for Agent Skills. It stores **hosted** skills (immutable,
content-addressed versions you upload or import from a fixed GitHub commit) and
**indexed** skills (a locator, file manifest and digest for content that stays
at its source), serves a read API, and publishes
[agentskills.io discovery v0.2.0](https://schemas.agentskills.io/discovery/0.2.0/schema.json)
indexes so standard clients such as `npx skills add` can install from it.

The same binary serves a public instance (anonymous reads) and private
deployments (authenticated reads by default).

> Status: **R1, first deliverable.** Not released, not deployed. See
> [Not in R1](#not-in-r1).

## Boundary

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
| `REGISTRY_PUBLIC_URL` | `-public-url` | required | External URL of the registry root **including** the base path; used in discovery URLs |
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

### Admin (`admin` scope or bootstrap token)

`POST /v1/admin/namespaces`, `POST /v1/admin/namespaces/{ns}/roles`,
`POST /v1/admin/tokens` (returns the plaintext once), `POST /v1/admin/tokens/{id}/revoke`,
`PUT /v1/admin/skills/{ns}/{name}/alias`,
`POST /v1/admin/skills/{ns}/{name}/versions/{v}/approve-license`, `GET /v1/admin/audit`.

## Key rules

- **Names.** Skill names are slugs (`[a-z0-9]+(-[a-z0-9]+)*`, max 64).
  Namespaces are lowercase reverse-DNS: `io.github.<login>` (bound to the
  numeric GitHub id, which must be unique), `ai.openlinker.<handle>` (public
  instance only), `internal.<team>` (private instance only) or a reverse
  domain (verification not implemented yet).
- **Immutability.** Versions never change; only review, check and yank fields
  may be updated, enforced by a database trigger. The audit table is
  append-only (trigger rejects UPDATE, DELETE and TRUNCATE).
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
  `custom` with text; `unknown` can only be `internal` or `private`. Indexed
  entries take the license from the SKILL.md frontmatter.
- **Discovery.** Only `public`, listed, active skills whose latest non-yanked
  version is eligible: hosted entries, single-file indexed entries (raw URL at
  the commit), and multi-file indexed entries whose license permits
  redistribution and whose last source check did not fail. No URL contains
  credentials or signed parameters.

## Not in R1

MCP endpoints (`/v1/mcp`, `/v0`), the built-in web UI for private instances,
GitHub OAuth login and namespace self-service, GitHub Actions OIDC, Core
gateway assertions, URL/well-known indexing (schema exists, importer does not),
claims, shares, upstream sync and mirroring, S3 storage (interface exists),
rate limiting and review queues, offline bundles, publisher signatures.

## Tests

`go test ./...` runs unit tests and the end-to-end HTTP suite on an in-memory
store with a fake GitHub server (no network). Set
`REGISTRY_TEST_DATABASE_URL` to also run the same suite and trigger tests on
PostgreSQL; each test uses its own schema and drops it afterwards.

## License

Apache-2.0
