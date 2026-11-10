# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/openlinker-registry ./cmd/openlinker-registry
# The runtime image has no shell, so the data directory is prepared here.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/openlinker-registry /usr/local/bin/openlinker-registry
# /data must exist and be owned by the nonroot user (65532) before VOLUME so
# a fresh named volume inherits that ownership and the store can write to it.
COPY --from=build --chown=65532:65532 --chmod=0750 /out/data /data
ENV REGISTRY_LISTEN=:8080 REGISTRY_STORAGE_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/openlinker-registry"]
