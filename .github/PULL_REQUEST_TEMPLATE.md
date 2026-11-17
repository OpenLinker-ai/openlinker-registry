## Summary

-

## Scope

- [ ] Keeps the registry independent: no dependency on another OpenLinker module, `GOWORK=off`.
- [ ] Does not execute skill content, run MCP servers, or store secret values.
- [ ] Every new read path enforces visibility on the server, with a test showing a hidden entry looks like a missing one.
- [ ] Does not include secrets, private URLs, private registry content, or local `.env` values.

## Validation

- [ ] `make check` passes, and `go test -count=1 -race ./...` passes with `REGISTRY_TEST_DATABASE_URL` set.
- [ ] Store changes are in both implementations and in the shared conformance suite, or the stores are unaffected.
- [ ] English and Chinese READMEs were updated together, or no documentation change is needed.
- [ ] `CHANGELOG.md` has an entry under `Unreleased`, or the change is not user-visible.

## Notes

Link related issues. Call out breaking behavior, new database migrations, and
anything a deployment behind a reverse proxy must change.
