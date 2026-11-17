# Support

Use GitHub issues for reproducible bugs, documentation problems, and feature
requests that fit the registry's scope: storing, indexing, mirroring and
serving Agent Skills and MCP server definitions.

## Good issue topics

- publishing, reading, yanking and deprecating skills and MCP servers
- discovery indexes, `npx skills add` compatibility, and the MCP Registry API
  under `/v0.1` and `/v0`
- upstreams: forwarding, mirroring, synchronization and verification
- visibility, tokens, namespaces and roles
- rate limits and running behind a reverse proxy
- configuration, database migrations, the image and the release archives
- the built-in pages of a private instance

## Before opening an issue

- Search existing issues and recent commits.
- Confirm the problem on the latest `main` branch or a named release.
- Include the output of `openlinker-registry version`, or the commit you
  built, and the operating system and architecture.
- Include the configuration that matters: instance mode, base path, whether a
  reverse proxy is in front, and the kind of upstream involved.
- Provide reproduction steps, expected behavior, actual behavior, and the
  HTTP status and sanitized response body.
- Remove API tokens, upstream tokens, Authorization headers, database URLs,
  private URLs and private registry content.

## Not supported here

- vulnerabilities; follow [SECURITY.md](./SECURITY.md)
- reviews of the content a registry lists: the registry does not vouch for
  skills or MCP servers, see SECURITY.md
- OpenLinker Core, Cloud, CLI, Agent Node or Plugin behavior; use those
  repositories
- private deployment debugging without reproducible public details
