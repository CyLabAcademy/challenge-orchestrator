# syntax=docker/dockerfile:1
# The class box: one container that is the whole deployment.
#
# cork's other fleet puts the orchestrator, the builder, the registry and the
# workers in separate containers, because that is what they are in production.
# A class deployment is the opposite claim -- one machine, one docker daemon,
# cork talking to it over the local socket -- and the only honest way to test
# that claim is to build the machine it describes.
#
# So dockerd and corkd live here together, and the scenario runs here too
# (run-single.sh execs it). That makes localhost mean what it means on a real
# one-box install: corkd reaches docker at /var/run/docker.sock with no
# DOCKER_HOST, instances publish on this host, and the operator's commands run
# beside them, the way an instructor's would over ssh.
#
# Build context is the repository root:
#   docker build -f e2e/single.Dockerfile .
# Declared before any FROM so the dind stage below can interpolate it: an ARG
# after a FROM belongs to that stage alone and reads empty in a later FROM.
ARG DIND_VERSION=29.6.0

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
      -ldflags "-X github.com/CyLabAcademy/challenge-orchestrator/cork.version=${CORK_VERSION} -X main.version=${CORK_VERSION}" \
      -o /out/ ./cmd/corkd ./cmd/cork

FROM docker:${DIND_VERSION}-dind
# bash/curl/jq/nc for the scenario, python3 to run a challenge's own solve
# script against a live instance. A real class box would not carry these;
# they are the operator, not the deployment.
RUN apk add --no-cache bash curl jq netcat-openbsd python3
COPY --from=build /out/corkd /out/cork /usr/local/bin/
# As in the release tarball and the multi-host image: the pre-rename names are
# symlinks to the new ones until they are dropped.
RUN ln -s corkd /usr/local/bin/cmgrd && ln -s cork /usr/local/bin/cmgrd-cli
COPY e2e/single-entrypoint.sh /usr/local/bin/single-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/single-entrypoint.sh"]
