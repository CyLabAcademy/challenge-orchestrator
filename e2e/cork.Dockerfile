# syntax=docker/dockerfile:1
# cork (cmgrd + cmgrd-cli) built from this checkout, for the e2e compose
# stack. Build context is the repository root:
#   docker build -f e2e/cork.Dockerfile .
FROM golang:1.26-alpine AS build
# CGO for go-sqlite3, as in the release workflow (there against glibc).
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG CORK_VERSION=e2e
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -v \
      -ldflags "-X github.com/CyLabAcademy/challenge-orchestrator/cmgr.version=${CORK_VERSION} -X main.version=${CORK_VERSION}" \
      -o /out/ ./cmd/...

FROM alpine:3.22
# bash/curl/jq/nc are for the e2e scenario, which runs from this same image.
# openssl backs the stalled-registry fixture (an s_server that never answers)
# and sqlite the database-busy step (it holds SQLite's write lock from outside
# cmgrd). Neither is used by cmgrd itself.
RUN apk add --no-cache ca-certificates bash curl jq netcat-openbsd openssl sqlite
COPY --from=build /out/corkd /out/cork /usr/local/bin/
# The release tarball ships the pre-rename names as symlinks to the new ones
# (see .github/workflows/release.yml); the image does the same, so the
# scenario can check that they still answer.
RUN ln -s corkd /usr/local/bin/cmgrd && ln -s cork /usr/local/bin/cmgrd-cli
COPY e2e/cork-entrypoint.sh /usr/local/bin/cork-entrypoint.sh
EXPOSE 4200
ENTRYPOINT ["/usr/local/bin/cork-entrypoint.sh"]
CMD ["corkd"]
