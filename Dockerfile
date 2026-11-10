# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/openlinker-registry ./cmd/openlinker-registry

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/openlinker-registry /usr/local/bin/openlinker-registry
ENV REGISTRY_LISTEN=:8080 REGISTRY_STORAGE_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/openlinker-registry"]
