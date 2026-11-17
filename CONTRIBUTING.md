# Contributing

## Ground rules

- The registry stays independent: no dependency on any other OpenLinker
  module, and only the Go standard library plus the modules already in
  `go.mod`. `GOWORK=off` is set by the Makefile; keep it set when you run `go`
  by hand.
- The registry never executes skill content, never runs MCP servers and
  never stores secret values.
- Every read path enforces visibility on the server. A new read path needs a
  test showing that an entry the caller may not see is indistinguishable from a
  missing one.
- Published versions are immutable. A change that needs to rewrite one is the
  wrong change.
- Every mutation writes its audit row in the same transaction.

## Before you open a pull request

```sh
make check        # gofmt, go vet, go test
REGISTRY_TEST_DATABASE_URL='postgres://registry:registry@127.0.0.1:5432/registry_test?sslmode=disable' make test-pg
```

Store changes go into both implementations (`internal/store` in memory,
`internal/pgstore` on PostgreSQL) and into the shared conformance suite
(`internal/store/storetest`). HTTP behavior is covered end to end in
`internal/apitest`, which runs against both stores.

Database migrations are forward-only files in `internal/pgstore/migrations`;
never edit one that has been released.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not open a public issue for a vulnerability.
