# Release Process

The registry has its own version numbers, as the OpenLinker CLI, Agent Node,
Plugin and SDK repositories do. A release is a `v*` tag pushed to this
repository; the tag push starts `.github/workflows/release.yml`.

## What a release publishes

1. The workflow first runs gofmt, `go vet` and the test suite with PostgreSQL
   on the tagged commit. Nothing is published if that fails.
2. A `linux/amd64` + `linux/arm64` image goes to
   `ghcr.io/openlinker-ai/openlinker-registry`, tagged `sha-<first 12 of the
   commit>` and the release tag. `latest` moves only for a tag without a
   pre-release suffix.
3. Archives for Linux, macOS and Windows (amd64 and arm64), each with a
   `.sha256` file, are attached to a GitHub release of the same tag. A tag
   with a suffix (`v0.1.0-rc.1`) becomes a pre-release and is not marked
   "Latest".

Running the workflow by hand, on a branch or on a tag, is not a release: it
publishes only the `sha-` image and keeps the archives as workflow artifacts.

## Pre-release checklist

1. `README.md` and `README.zh-CN.md` describe the same behavior.
2. `CHANGELOG.md` lists what changed under the version being released,
   including every breaking change and every new database migration.
3. The checks pass on the commit to be tagged:

   ```sh
   make check
   GOWORK=off REGISTRY_TEST_DATABASE_URL='postgres://registry:registry@127.0.0.1:5432/registry_test?sslmode=disable' \
     go test -count=1 -race ./...
   ```

   `GOWORK=off` matters when this repository is checked out inside the
   parent repository, whose `go.work` does not include it; the Makefile sets
   it only for `make`.

4. One archive builds, and its binary reports the version and commit it was
   given (on Linux or macOS, for the machine you are on):

   ```sh
   out="$(mktemp -d)"
   archive="$(bash scripts/package-release.sh v0.x.y "$(git rev-parse HEAD)" "$(go env GOOS)" "$(go env GOARCH)" "$out")"
   tar -xzf "$archive" -C "$out"
   "$out"/openlinker-registry-v0.x.y-*/openlinker-registry version
   ```

   The script only builds the archive and prints its path. CI does the same
   for Linux and checks the output.

5. Database migrations are forward-only. A release that adds one says so in
   the changelog; never edit a migration that has been released.
6. No token, database URL, private URL or private registry content is in the
   documentation, tests, fixtures or release notes.

## Tagging

Tag a commit on `main` whose CI passed:

```sh
git tag -a v0.x.y -m "Release v0.x.y"
git push origin v0.x.y
```

Use semantic versions. Before 1.0 a release may contain breaking changes, but
they must be called out in `CHANGELOG.md`. Use a pre-release suffix
(`v0.x.y-rc.1`) for test releases.

## The OpenLinker parent repository

The parent repository pins a commit of this repository as a submodule and
builds its own deployments from that commit; it does not depend on a published
release.

When the parent publishes a formal release of its own, its release script
looks at the commit it pins here and at the tags of this repository:

- If a tag with the parent's version name already exists here, it must point
  to the pinned commit. If it points anywhere else, the parent's release
  fails. Releasing the pinned commit under another name does not help: the
  two version lines must not use the same tag name for different commits.
- Otherwise, if the pinned commit already carries any `v*` tag, it is left
  alone.
- Otherwise the parent tags the pinned commit with the parent's version, and
  that tag push starts the release workflow here under the parent's version
  number.

So, to keep this repository's own version line, release-tag the commit the
parent pins before the parent's formal release, and never give a commit here
a version name that a later parent release will use.
