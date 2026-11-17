# GOWORK=off keeps this module independent of any enclosing go.work.
export GOWORK := off

BIN := bin/openlinker-registry
# Release builds set VERSION to the release tag; see .github/workflows/release.yml.
VERSION ?= dev
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)

.PHONY: build test test-pg vet fmt check docker clean

build:
	go build -trimpath -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o $(BIN) ./cmd/openlinker-registry

test:
	go test ./...

# Requires REGISTRY_TEST_DATABASE_URL, e.g.
# postgres://registry@127.0.0.1:5432/registry_test?sslmode=disable
test-pg:
	@test -n "$$REGISTRY_TEST_DATABASE_URL" || (echo "REGISTRY_TEST_DATABASE_URL is not set" && exit 1)
	go test -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt needed" && exit 1)
	go vet ./...
	go test ./...

docker:
	docker build --build-arg REGISTRY_VERSION=$(VERSION) --build-arg REGISTRY_COMMIT=$(COMMIT) -t openlinker-registry:dev .

clean:
	rm -f $(BIN)
