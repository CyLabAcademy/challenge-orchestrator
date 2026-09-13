# syntax=docker/dockerfile:1
# cork-telemetry, the worker health agent corkd polls on :2136, built from
# its upstream repository at a pinned tag. The multihost_docker role installs
# the same project's release binary.
FROM golang:1.26-alpine AS build
ARG CORK_TELEMETRY_REPO=https://github.com/CyLabAcademy/cork-telemetry
ARG CORK_TELEMETRY_REF=v0.0.1
RUN apk add --no-cache git
RUN git clone --depth 1 --branch "${CORK_TELEMETRY_REF}" "${CORK_TELEMETRY_REPO}" /src
WORKDIR /src
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /telemetry .

FROM alpine:3.22
COPY --from=build /telemetry /usr/local/bin/telemetry
ENV TELEMETRY_PORT=2136
# Reads only /proc/stat and /proc/meminfo; runs unprivileged as in production.
USER nobody
ENTRYPOINT ["/usr/local/bin/telemetry"]
