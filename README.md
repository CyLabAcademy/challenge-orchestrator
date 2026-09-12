# challenge-orchestrator (cork)

challenge-orchestrator is a custom fork of the cmgr platform originally developed by the US Army Cyber Institute. It is custom built to handle the needs of the Cylab Security Academy infrastructure.

## Introduction

cmgr is at the same time both a challenge development tool and challenge hosting platform. cmgr, by default, utilizes the system's local docker engine to run challenges. However, with the docker api reachable over TLS and the presence of a dedicated background daemon binary,cmgr may be used to host CTF competitions or platforms. However, this doesn't change the fact that cmgr maintains a single connection to a single docker API endpoint, be it local or remote. In order to handle the greater number of users on the Cylab Academy platform, cork was devised to grant cmgr multi-host capability

## Objectives

Cork accomplishes these following goals

- Unify cmgr instances so that all challenges and worker servers are controlled by one manager in one place.
- Multi-host awareness to allow for horizontal scaling. This removes many of the serialization blocks present on the docker engine.
- Provide a container launch/teardown speed greater than that of many commercial container orchestrators and backends (nomad, kubernetes, podman) by running on pure docker.

## Design

Cmgr produced two binaries: cmgr, which is the challenge development tool, and cmgrd, the challenge hosting daemon. Both relied on the cmgr go module, shared between both. Unfortunately, this is not a flexible or extensible framework, as changes to the daemon will impact the challenge development tool. Thus, the decision was made to separate the two functions.

Challenge development capabilities remain with [cmgr](https://github.com/picoCTF/cmgr) as well as [PCM](https://github.com/picoCTF/challenge-manager). Cork is compatible with any challenge files and builds compatible with cmgr.

### The shape of a deployment

One build plane, one registry, one orchestrator per event, and each
orchestrator's own workers.

```
                 ┌───────── BUILD PLANE (exactly one) ─────────┐
    operator ───▶│  cork-build + the challenge tree and schemas │
  every schema   │  its database is bookkeeping, not a record   │
  operation      └──────┬───────────────────────────┬───────────┘
                        │ push (write-once)         │ hand over, then
                        ▼                           │ converge, per schema
              ┌────────────────────┐                │ on its destination
              │  REGISTRY (one)    │                ├──────────────┐
              │  content-addressed │                ▼              ▼
              └────┬──────────┬────┘        ┌──────────────┐ ┌──────────────┐
                   │ pull     │ pull        │ ORCHESTRATOR │ │ ORCHESTRATOR │
                   │          │             │   "event"    │ │  "library"   │
                   │          │             └──────┬───────┘ └──────┬───────┘
                   ▼          ▼                    │ place          │ place
              ┌────────┐ ┌────────┐                │                │
              │ worker │ │ worker │◀───────────────┘                ▼
              └───┬────┘ └───┬────┘                            ┌────────┐
                  └─ players ┘                                 │ worker │
                                                               └────────┘
```

Everything flows one way. The operator acts on the build plane and nowhere
else; the build plane pushes images and hands builds over; workers pull and
serve. Nothing reports back up, which is why each side has to be able to
check rather than ask: the orchestrator recomputes every identity it is
handed and looks every tag up in the registry, instead of trusting the plane
that sent them.

Two payloads take two different routes, and it is worth keeping them
straight. **Images** go through the registry, and the workers pull them.
**Artifact archives** — the files a challenge gives its players — never move
at all: they are written on the build plane that built them, in a directory
per destination under `CMGR_ARTIFACT_DIR`, and an artifact server running
beside the build plane publishes them (picoCTF's uploads to S3 behind
CloudFront; a single box can serve them over HTTP). They never enter the
registry, never reach a worker, and never reach an orchestrator: a hand-over
is JSON, and all it says about them is `has_artifacts`, so the platform knows
to offer the download.

Cork itself serves no artifacts. cmgr had `GET /builds/<id>/artifacts.tar.gz`
and a per-file path under it; both are gone, along with `cmgrd-cli artifacts`.
An orchestrator on an external build plane ignores `CMGR_ARTIFACT_DIR`
entirely and says so at startup.

The counts are not arbitrary:

- **One build plane.** It is the control point, not a workhorse: every schema
  operation happens here, which is what makes the exclusivity rule in
  [BUILDER.md](BUILDER.md) enforceable at all.
- **One registry**, shared. Tags are content-addressed, so the same content
  is the same tag wherever it was built — which is what lets a schema move
  between orchestrators without rebuilding anything.
- **One orchestrator per event.** It is a singleton by construction: a
  SQLite database, in-process locks, and worker state held in memory. Two
  would not coordinate; they would corrupt.
- **Many workers**, each belonging to exactly one orchestrator.

The orchestrator builds with BuildKit, pins its base images by digest, and publishes an inline layer cache to the registry. [BUILDER.md](BUILDER.md) covers how that works, how to move a base image, and what not to do to the builder.

## Setup

There is no quickstart. This product is meant for deployment in production, not testing. If you're a challenge developer, please follow the readme in [cmgr](https://github.com/picoCTF/cmgr) instead.

**One box works, and is not what this is for.** Leave `CMGR_BUILD_PLANE`
unset and `cmgrd` builds on its own docker daemon and runs what it builds —
the shape legacy cmgr had, and the one a classroom or an instructor's box
wants. That path is supported on purpose rather than by accident:
`e2e/run-single.sh` stands the deployment up in a container and drives it on
every change. But everything above is absent from it — no build plane, no
registry, no destinations, no migrating a schema between orchestrators, and
no horizontal scale, which was the whole reason cork was forked from cmgr.
The deployment tooling being built targets the split shape, not this one.
Run one box if one box is genuinely the deployment; don't run it as a
smaller version of the real thing.

**Local development is worse served here than upstream, deliberately.** cork
dropped cmgr's development commands along with the `cmgr` binary: there is
no `test`, `playtest` or `check`, and no solver framework behind them. It
wants a docker daemon it can push from, and the multi-host shape wants two
CAs and five certificates before anything starts. If you are writing or
debugging a challenge, use [cmgr](https://github.com/picoCTF/cmgr) or
[PCM](https://github.com/picoCTF/challenge-manager) — cork is compatible
with what they produce, and reads the same challenge files.

To see the whole fleet run on one machine, `e2e/` holds a docker compose simulation of it (orchestrator, build daemon, registry, two workers with telemetry, real PKI) plus a scenario that drives the launch sequence through cmgrd and checks every box; see [e2e/README.md](e2e/README.md).

These instructions relate to manual setup and should be replaced with ansible/terraform later

### Keygen

Generate the keys using gen-docker-certs.sh. Make sure the zot IP is correct before running. It mints **two CAs** and five leaf certs, splitting trust into two domains so a leak in one cannot forge into the other (a stolen worker registry cert can read zot but cannot control any worker's dockerd).

Every file is named `<domain>-<role>-<kind>.pem`, where `domain` is `docker` or `zot`, `role` is `ca`/`server`/`client`/`worker`, and `kind` is `cert` or `key` (a CA is `<domain>-ca-cert` / `<domain>-ca-key`):

- **docker** — the mTLS domain between cork and the worker dockerds. `docker-ca` signs:
  - `docker-server-{cert,key}` — worker dockerd server cert (shared, SAN `academy-docker-worker`) -> workers present this to cork
  - `docker-client-{cert,key}` — cork's client cert (`CN=cmgr`) -> cork presents this to workers
- **zot** — the mTLS domain between registry clients and zot. `zot-ca` signs:
  - `zot-server-{cert,key}` — zot server cert -> zot presents this to clients
  - `zot-client-{cert,key}` — cork's client cert (`CN=cmgr`) -> orchestrator push + cork delete (read-write on zot)
  - `zot-worker-{cert,key}` — worker registry client cert (`CN=worker`, shared) -> workers pull (read-only on zot)

Both CA private keys (`docker-ca-key.pem`, `zot-ca-key.pem`) stay offline and are deployed to no box. The `cmgr` identity is two certs — `docker-client` and `zot-client`, one signed by each CA — because it acts in both domains; they deploy to different directories and are not interchangeable.

### Orchestrator

1. Install cork on the server by cloning this repository and building using `go build -v -ldflags "$(sh ci/version.sh --ldflags)" -o bin ./...` in the challenge-orchestrator directory
2. Install docker on the server. Required to build images. https://docs.docker.com/engine/install/ubuntu/
3. Install docker-reaper onto the server. This prevents stale containers and unused images from accumulating. https://github.com/picoCTF/docker-reaper. If you don't mind the orchestrator's local docker daemon, which is only used for building, being filled with build artifacts, then this can step can be skipped.
4. Configure cork's environment variables
5. Move the certificates over. The orchestrator holds two cert sets from different CAs: the **docker-ca** set (authenticates cork to the workers) in `$DOCKER_CERT_PATH`, and the **zot-ca** set (authenticates cork to the registry) in CERTS.D.
  ┌────────────────────────┬────────────────────────────────────┐
  │       bundle file      │             destination            │
  ├────────────────────────┼────────────────────────────────────┤
  │ docker-ca-cert.pem     │ $DOCKER_CERT_PATH/ca.pem           │
  │ docker-client-cert.pem │ $DOCKER_CERT_PATH/cert.pem         │
  │ docker-client-key.pem  │ $DOCKER_CERT_PATH/key.pem          │
  │ zot-ca-cert.pem        │ CERTS.D/ca.crt                     │
  │ zot-client-cert.pem    │ CERTS.D/client.cert                │
  │ zot-client-key.pem     │ CERTS.D/client.key                 │
  └────────────────────────┴────────────────────────────────────┘

  CERTS.D = /etc/docker/certs.d/<ZOT_IP:ZOT_PORT>

The two sets have different trust roots and are not interchangeable: the `$DOCKER_CERT_PATH` set chains to docker-ca (workers), the CERTS.D set chains to zot-ca (registry). cork reads them independently (`workers.go` vs `registry.go`), so no code change is needed — the split is entirely in which CA's files land at each path.
  
6. Pull the challenges repository (or any other repository holding challenges in cmgr's format)
7. Add challenges that cork will serve either manually or using a schema file
8. Add cork to a systemd service that automatically runs it in the background (No examples yet)

### Worker

1. Install the health test service. This can be found at https://github.com/CyLabAcademy/cork-telemetry 
2. Install docker onto the server. https://docs.docker.com/engine/install/ubuntu/
3. Install docker-reaper onto the server. This prevents stale containers and unused images from accumulating. https://github.com/picoCTF/docker-reaper
4. Copy over config-examples/worker/daemon.json -> /etc/docker/daemon.json. Make sure the values are correct
5. Copy over config-examples/worker/override.conf -> /etc/systemd/system/docker.service.d/override.conf. This fixes an issue where -H is used for both a file descriptor and in daemon.json. The FD is no longer necessary since we're binding to a tcp port anyways
6. Copy over the relevant certificates. The worker's dockerd uses the **docker-ca** set; its registry pull uses the **zot-ca** set.

  ┌────────────────────────┬──────────────────────────────────┐
  │       bundle file      │            destination           │
  ├────────────────────────┼──────────────────────────────────┤
  │ docker-ca-cert.pem     │ dockerd --tlscacert              │
  │ docker-server-cert.pem │ dockerd --tlscert                │
  │ docker-server-key.pem  │ dockerd --tlskey                 │
  │ zot-ca-cert.pem        │ CERTS.D/ca.crt                   │
  │ zot-worker-cert.pem    │ CERTS.D/client.cert              │
  │ zot-worker-key.pem     │ CERTS.D/client.key               │
  └────────────────────────┴──────────────────────────────────┘
DOCKER CERTS location (dockerd --tlscacert/--tlscert/--tlskey) is defined by daemon.json, by default /root/.docker_certs
CERTS.D location depends on the IP and Port of the registry from the perspective of the worker
CERTS.D = /etc/docker/certs.d/<ZOT_IP:ZOT_PORT>

server cert is SHARED across workers (signed against ServerName, by docker-ca)
workers NEVER get a cmgr cert of either CA (that's the orchestrator's identity)

### Registry

1. Install zot onto the server https://zotregistry.dev/v2.1.18/
2. config-examples/zot/config.json -> /etc/zot/config.json
3. config-examples/zot/zot.service -> /etc/systemd/system/zot.service
4. Transfer the certificates over appropriately

  REGISTRY BOX — zot (server identity only; holds no client cert)
  ┌─────────────────────┬───────────────────────────────────────────────────┐
  │     bundle file     │                    destination                    │
  ├─────────────────────┼───────────────────────────────────────────────────┤
  │ zot-ca-cert.pem     │ /etc/zot/zot-ca-cert.pem       (http.tls.cacert)  │
  ├─────────────────────┼───────────────────────────────────────────────────┤
  │ zot-server-cert.pem │ /etc/zot/zot-server-cert.pem   (http.tls.cert)    │
  ├─────────────────────┼───────────────────────────────────────────────────┤
  │ zot-server-key.pem  │ /etc/zot/zot-server-key.pem                       │
  └─────────────────────┴───────────────────────────────────────────────────┘
  zot trusts ONLY zot-ca — place the zot CA (not the docker CA) at http.tls.cacert.
  Note: zot-server-key.pem is delivered to the service via systemd `LoadCredential`
  in zot.service, so config.json reads it from /run/credentials/zot.service/.


## Configuration/Usage

There are two binaries an operator uses, and which one depends on where the
challenges are built.

**`cork-build`** is the build plane: it holds the challenge tree and every
schema operation, and hands finished builds to the orchestrators that serve
them. A deployment with `CMGR_BUILD_PLANE=external` is driven from here —
`cork-build build spring.yaml` builds, pushes, hands over and converges, all
against the destination the schema names. See [BUILDER.md](BUILDER.md) and
`cork-build --help`; the commands are:

| | |
| --- | --- |
| deploying | `build`, `add-schema`, `update-schema`, `remove-schema`, `migrate-schema` |
| reading this plane | `list-schemas`, `show-schema`, `list`, `search`, `info`, `system-dump` |
| the challenge tree | `update`, `dockerfile`, `convert-to-custom` |
| configuration | `destinations`, `pins`, `version` |

**`cmgrd-cli`** is a thin HTTP client for one orchestrator. What is *running*
— instances, workers, a live launch or stop — is the orchestrator's, and only
it can answer. It keeps the build and schema commands too, because a
single-host deployment runs no build plane at all and they are that
operator's whole interface — its own help, below, says which of them an
external orchestrator refuses and how.

```
Usage: ./cmgrd-cli [--server <url>] <command> [<args>]

A thin HTTP client for cmgrd: every command is an API call against the
server; nothing touches the database, docker, or the registry directly.

On a daemon running with CMGR_BUILD_PLANE=external, the commands that build
answer 409 -- update, build, and the pin-* pair -- because that daemon
builds nothing: cork-build does, from the machine holding the challenge
tree, and add-schema is refused there too since a hand-over is what brings a
schema into being. On a single-host deployment (the default) there is no
build plane to separate out and every command below is yours.

Deployment:
  update [--dry-run] [--verbose] [--prune-old] [<dir>]
      re-scan the challenge directory on the server (rebuilding changed
      challenges, and any build a failed rebuild left at an earlier
      generation) and print the resulting changes; <dir> must be inside the
      server's CMGR_DIR and defaults to all of it; --prune-old additionally
      removes the image generation each rebuild displaces from rollback
      retention, on the build daemon and in the registry
  update-schema <schema file>
      create or update the schema defined in the given yaml/json file and
      converge its builds/instances
  add-schema <schema file>
      like update-schema, but errors if the schema already exists
  remove-schema <schema>
      delete the schema and destroy its builds/instances
  list-schemas
  show-schema <schema>
      print the schema's full nested challenge/build/instance state

Challenges and instances:
  list [--verbose]
  search [--verbose] <tag> [<tag> ...]
  info <challenge>
  build [--flag-format <format>] <challenge> <seed> [<seed> ...]
  destroy <build>
  start <build>
  stop <instance>
  system-dump
      print the full nested state of every challenge

Workers:
  worker-add <ip> [<public address>]
      register a worker, or bring a down one back once it is rebooted or
      repaired (its instances come back with it: their containers restart on
      their own); the optional public address is what players are given for
      its instances; containers and networks cmgr created on it for instances
      it no longer records are removed first (as for every worker at cmgrd
      start). A daemon that stays unreachable while that runs is marked down
      and takes nothing until another worker-add; one that answers but leaves
      the cleanup unfinished takes placements anyway, with an error in the
      log, since what is left costs a launch here and there rather than the
      whole box
  worker-remove <ip>
      purge the worker and all of its instance records, for a box that is
      terminated and recreated rather than rebooted (nothing on the worker
      itself is touched; re-adding it cleans up); a persistent instance it
      hosted is only relaunched by the next update-schema
  worker-down <ip>
      mark the worker down, taking it out of placement but keeping its records
  worker-list

Base image pins:
  pin-list
      show the pinned base images and how many challenges use each
  pin-refresh
      re-resolve every base image the challenge directory names to the digest
      the registry serves now, and save it; this is the only time a mutable
      tag is consulted, and it rebuilds nothing by itself

Other:
  version
      print client and server versions

The server defaults to http://127.0.0.1:4200 and can also be set via the
CMGRD_SERVER environment variable.
```

The canonical way to operate cork is through schemas and remote HTTP requests.

First, connect the workers that you desire to cork using the worker-add commands. If the health test checks out, they should immediately switch to the okay status.

Then the schemas, and this is where the two deployments differ.

**With an external build plane**, everything goes through `cork-build` on the
machine holding the challenge tree. `cork-build build <schema file>` scans
the tree, builds and pushes what the schema names, hands each challenge to
the orchestrator the schema's `destination` points at, and converges the
schema there — one command, and no orchestrator address typed by hand. A
source change is the same command again. Moving a schema between
orchestrators is `migrate-schema`, which rebuilds nothing; taking one out of
service is `remove-schema <name>`. The orchestrator refuses `update` and
manual builds with a 409: it builds nothing.

**On a single host** (`CMGR_BUILD_PLANE` unset, the default), there is no
build plane to separate out, and `cmgrd-cli` is the whole interface: apply a
schema with `add-schema`, rebuild changed challenge content with `update`
(not `update-schema`), and run `update-schema` when the *schema* itself
changes.

Either way, your challenges are then built and ready to run. Send HTTP
requests for start/stop with the appropriate build IDs and watch them run.

To build or run challenges by hand, use `cmgrd-cli`'s `build`, `start` and
`stop` — they act on one orchestrator directly. Note that a manual build
belongs to no schema, so it has no destination and no build plane will route
it; on an external build plane they are refused for that reason.

## Environment Variables


| Variable                 | Meaning                                                                                                                            | Default                                                                  |
| ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
| CMGR_DB                  | SQLite DB Path                                                                                                                     | ./cmgr.db                                                                |
| CMGR_DIR                 | Challenge root directory                                                                                                           | .                                                                        |
| CMGR_ARTIFACT_DIR        | Where a build's artifact bundle is written. Build plane only — `cork-build` writes a subdirectory per destination under it, and that is what an artifact server publishes from. An orchestrator (`CMGR_BUILD_PLANE=external`) holds no bundles, ignores this, and says so at startup | .                                                                        |
| CMGR_INTERFACE           | The interface where the challenge ports on the workers are exposed on                                                              | 0.0.0.0                                                                  |
| CMGR_PORTS               | Range for challenge ports. Must be in format like 1024-65535                                                                       | unset (docker decides)                                                   |
| CMGR_ENABLE_DISK_QUOTAS  | Enable disk quotas                                                                                                                 | unset (off)                                                              |
| CMGR_PRUNE_AGE           | Maximum age of an on-demand instance (schema-managed instances are never pruned). Only affects the database, NOT the actual containers; docker-reaper cleans those | 1h                                                                       |
| CMGR_DB_WAL              | Enable WAL journaling for SQLite                                                                                                   | on                                                                       |
| CMGR_LOGGING             | Log verbosity for `cmgrd` and `cork-build`: `debug`, `info`, `warn`, `error` or `disabled`. There is no flag for it on `cmgrd`, so this is the only way to raise an orchestrator's logging; `cork-build --verbose` is the flag form and wins over it. A value that does not parse is warned about and not fatal | info |
| CMGR_CONCURRENT_LAUNCHES | Launch slots per daemon (1-16), and as many teardown slots: network creation plus container starts, which dockerd serializes internally on either firewall backend; no gain measured past 2 | 2                                                                        |
| CMGR_WORKER_POLL_INTERVAL | How often each worker's telemetry agent is polled                                                                                  | 500ms                                                                    |
| CMGR_WORKER_POLL_TIMEOUT | Per-poll timeout; must be under the poll interval (clamped to half of it otherwise)                                                | 250ms                                                                    |
| CMGR_WORKER_MAX_MISSES   | Consecutive failed polls before a worker is marked down (sticky until worker-add)                                                  | 60 (30s of silence)                                                      |
| CMGR_WORKER_CONTROL_TIMEOUT | Ceiling for one container/network call to a worker's dockerd; hitting it marks the worker down                                     | 30s                                                                      |
| CMGR_WORKER_PULL_TIMEOUT | Ceiling for one image pull before a launch; hitting it fails the launch as retryable (503) only. A restart during an update pulls under a 5m ceiling, or this value when it is longer | 30s                                                                      |
| CMGR_WORKER_LAUNCH_WAIT  | How long a launch waits for a launch slot before failing as retryable (503 with Retry-After); one that would evidently wait longer is refused at once | 10s                                                                      |
| CMGR_BASE_PINS           | JSON map of base image reference to digest. When present, `FROM name:tag` is rewritten to the digest in the build context, so the builder never re-resolves a mutable tag. See [BUILDER.md](BUILDER.md) | \<CMGR_DIR\>/base-pins.json (commit it at the corpus root; see BUILDER.md)                       |
| CMGR_PURGE_AFTER_PUSH    | Drop the builder's local copy of a build's images once they are in the registry, so the image store does not grow with the whole fleet. On unless set to `false`/`0`/`off`, and ignored entirely without a registry (there the local copy is the only one). See [BUILDER.md](BUILDER.md) | on (registry mode only)                                                  |
| CMGR_REGISTRY            | Registry host location                                                                                                             | unset, set with an IP                                                    |
| CMGR_REGISTRY_USER       | Unused                                                                                                                             | unset                                                                    |
| CMGR_REGISTRY_TOKEN      | Unused, identity set by TLS cert                                                                                                   | unset                                                                    |
| CMGR_REGISTRY_CERT_DIR   | Location of certificates used to communicate with registry                                                                         | /etc/docker/certs.d/\<registry\>                                         |
| CMGR_BUILD_PLANE         | Where challenge images are built: `local`, on the daemon `DOCKER_HOST` names from the tree in `CMGR_DIR`; or `external`, where something else builds, pushes to `CMGR_REGISTRY` and hands the finished builds to cmgrd, which then has no docker daemon and no challenge tree. External ignores `CMGR_DIR`, `CMGR_BASE_PINS`, `DOCKER_HOST` and `CMGR_PURGE_AFTER_PUSH` (each is named at startup if set), requires `CMGR_REGISTRY` and its client material (`CMGR_REGISTRY_CERT_DIR`, since destroy and prune untag in the registry alone), warns about a missing `DOCKER_CERT_PATH`, answers 409 to `update`, manual builds, the pins and a schema operation wanting a build that has not been handed over, fails a launch with no worker registered instead of running it locally, and takes each build through `PUT /challenges/<id>` (`cmgrd --help` documents the hand-over). See [BUILDER.md](BUILDER.md) | local                                                                    |
| DOCKER_HOST              | Location of the LOCAL docker daemon                                                                                                | unset. Defaults to local daemon. Does not need modification              |
| DOCKER_API_VERSION       | API version use                                                                                                                    | Automatically set, does not need modification                            |
| DOCKER_CERT_PATH         | Used to set the location for client certs to worker                                                                                | Expects /root/.docker_certs by default, but any directory can work. unset. |
| CORK_DESTINATIONS        | `cork-build` only. A yaml file mapping the destination names schemas use to the orchestrators they stand for (`library: https://host:4200`). A schema naming no destination means the only one configured, and is refused once there is more than one. Two names for one address are refused when the file is read. See [BUILDER.md](BUILDER.md) | unset |
| CMGRD_SERVER             | `cmgrd-cli` only, and the only variable it reads: the orchestrator to send requests to. `--server` overrides it                     | http://127.0.0.1:4200                                                    |

Everything above is read by the shared `cmgr` library, so `cmgrd` and
`cork-build` accept the same surface — but each ignores the other's half, and
says so at startup rather than silently. A build plane runs nothing, so
`CMGR_CONCURRENT_LAUNCHES`, `CMGR_PORTS`, `CMGR_INTERFACE`,
`CMGR_ENABLE_DISK_QUOTAS`, `CMGR_PRUNE_AGE` and the six `CMGR_WORKER_*`
tunables are inert on `cork-build`; an orchestrator on an external build
plane builds nothing, so `CMGR_DIR`, `CMGR_BASE_PINS`, `DOCKER_HOST` and
`CMGR_PURGE_AFTER_PUSH` are inert on it. Either way, a setting that is set
but ignored is named in the log when the process starts — a unit file grown
from the other role's is the usual way it happens.


## Issues/Improvements/Quirks

- The cork API has zero authentication or security. It relies completely on upstream gates to prevent flooding and abuse
- The telemetry server provides neither authenticity nor secrecy. It's literally just a value that says whether the server is overloaded or not though.
- If a launch fails, it is not retried on a different worker. Expectation is that the user will get frustrated and retry. This is expected behavior
- Internally, cork uses the cmgr name for everything. This is a good target for a patch
- There is no health check for zot, cork assumes that it is up at all times
- CMGR_INTERFACE is pointless, it controls the interface to bind to for ALL WORKERS, which is meaningless
- If a schema doesn't declare a flag format, it defaults to `%!(EXTRA string=...)`

# DEPRECATED - FOR REFERENCE ONLY
# cmgr

## Notice

This repo is a fork of [ArmyCyberInstitute/cmgr](https://github.com/ArmyCyberInstitute/cmgr) with
customizations to support our continued usage of cmgr at [picoCTF](https://picoctf.org/).

For specific details regarding changes from the upstream project, see the [release notes](https://github.com/picoCTF/cmgr/releases).

## Introduction

**cmgr** is a new backend designed to simplify challenge development and
management for Jeopardy-style CTFs.  It provides a CLI (`cmgr`) intended for
development and managing available challenges on a back-end challenge server
as well as a REST server (`cmgrd`) which exposes the minimal set of commands
necessary for a front-end web interface to leverage it to host a competition
or training platform.

## Quickstart

Assuming you already have Docker installed, the following code snippet will
download example challenges and the **cmgr** binaries, initialize a
database file that tracks the metadata for those challenges, and then run the
test suite to ensure a working system.  The test suite can take several minutes
to run and is not required to start working.  However, running the suite can
identify permissions and other errors and is highly recommended for the first
time you use `cmgr` on a system.

```sh
wget https://github.com/picoCTF/cmgr/releases/latest/download/examples.tar.gz
wget https://github.com/picoCTF/cmgr/releases/latest/download/cmgr_`uname -s | tr '[:upper:]' '[:lower:]'`_amd64.tar.gz
tar xzvf examples.tar.gz
cd examples
tar xzvf ../cmgr_`uname -s | tr '[:upper:]' '[:lower:]'`_amd64.tar.gz
./cmgr update
CMGR_LOGGING=info ./cmgr test --require-solve
```

**NOTE:** Published binaries cover `linux_amd64` and `darwin_arm64` (Apple Silicon). On Apple Silicon, change `amd64` to `arm64` in the cmgr tarball URL. Intel Mac and Linux ARM builds are not published — build from source for those platforms (see [Back-End](#back-end)).

At this point, you can start checking out problems by finding the challenge ID
of one you would like to play and running `./cmgr playtest <challenge>`.  This
will build and start the challenge and run a minimal webserver (`localhost:4200`
by default) that you can use to view and interact with the content.  You could
also launch the REST server on port 4200 with `./cmgrd` or launch all of the
examples from the CLI with `./cmgr test --no-solve` which will launch an
instance of each example challenge and print the associated port information.

## Configuration

**cmgr** is configured using environment variables.  In particular, it
currently uses the following variables:

- *CMGR\_DB*: path to cmgr's database file (defaults to 'cmgr.db')

- *CMGR\_DIR*: directory containing all challenges (defaults to '.')

- *CMGR\_ARTIFACT\_DIR*: directory a build's artifact bundle is written to
  (defaults to '.'). Read only where building happens: `cork-build`, or a
  `cmgrd` on a local build plane. An orchestrator on an external build plane
  takes no bundles, so it neither reads this nor creates the directory, and
  logs that it is ignoring it.

- *CMGR\_MAX\_ARTIFACT\_FILES*: maximum number of entries permitted in a
  challenge's artifact archive (defaults to 10000); build plane only, as the
  two below are

- *CMGR\_MAX\_ARTIFACT\_BYTES*: maximum total uncompressed size of a
  challenge's artifact archive (defaults to '5g')

- *CMGR\_MAX\_ARTIFACT\_FILE\_BYTES*: maximum uncompressed size of any single
  file within a challenge's artifact archive (defaults to '1g')

- *CMGR\_LOGGING*: logging verbosity for command clients (defaults to
'disabled' for `cmgr` and 'warn' for `cmgrd`; valid options are `debug`,
`info`, `warn`, `error`, and `disabled`)

- *CMGR\_INTERFACE*: the host interface/address to which published challenge
ports should be bound (defaults to '0.0.0.0') (_Note_: if the specified
address is not bound to the host running the Docker daemon, this value gets
silently ignored by Docker and the exposed ports will be bound to the loopback
interface.)

- *CMGR\_PORTS*: the range of ports that are dedicated for serving challenges;
cmgr will assume that it fully owns these ports and nothing else will try
to use them (i.e., not in ephemeral range or overlapping with a service
running on the host); format is '1000-1000'.  Ephemeral ports on a Linux host
can be enumerated with `cat /proc/sys/net/ipv4/ip_local_port_range` and adjusted
with `sysctl`.  Some programs (e.g., `docker`) will need to be restarted after
adjusting the kernel parameter.

- *CMGR\_ENABLE\_DISK\_QUOTAS*: enables the [disk
  quota](examples/specification.md#challenge-options) container option when set. Disk quotas
  are only functional when using the `overlay2` Docker storage driver and
  [pquota-enabled](https://access.redhat.com/documentation/en-us/red_hat_enterprise_linux/7/html/storage_administration_guide/xfsquota)
  XFS backing storage. Otherwise, the creation of containers with disk quotas will fail at runtime.
  When unset, any specified quotas are ignored.

- *CMGR\_PRUNE\_AGE*: the maximum age for on-demand challenge instances (i.e.,
  those started via `cmgr start` or the `POST /builds/<id>` API endpoint, as
  opposed to schema-managed instances). Instances older than this value are
  automatically removed from the database during the next `cmgr start` or
  API-triggered launch (with at most a 1-minute check interval). The value
  must be a valid Go duration string (e.g., `1h`, `30m`, `24h`). Defaults to
  `1h`. Set to `0` to disable automatic pruning entirely.

  **Important:** This value should be set *greater* than any external
  instance-stopping interval (e.g., a Celery task that calls `DELETE
  /instances/<id>` on a schedule).  If the prune age is shorter than the stop
  interval, the DB row will be removed before the stop request arrives,
  causing a 404 error. See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) for more
  details.

- *CMGR\_DB\_WAL*: controls whether SQLite
  [WAL journaling](https://www.sqlite.org/wal.html) is enabled for the cmgr database.
  WAL mode improves write throughput and reduces lock contention under high
  instance-launch concurrency, at the cost of creating two additional sidecar
  files (`<db>-wal` and `<db>-shm`) in the same directory as the database.
  **Do not enable on network-mounted filesystems** (NFS, SMB, etc.) as the
  required shared-memory locking is typically unsupported and may cause
  corruption. **Enabled by default.** Set to `false`, `off`, or `0` to disable.

- *CMGR\_CONCURRENT\_LAUNCHES*: the maximum number of concurrent container
  launches allowed. Higher values typically do not improve performance and
  can increase system load. Allowed values are `1` or `2`. (defaults to `2`)

Additionally, we rely on the Docker SDK's ability to self-configure base off
environment variables.  The documentation for those variables can be found at
[https://docs.docker.com/engine/reference/commandline/cli/](https://docs.docker.com/engine/reference/commandline/cli/).

## Developing...

### Challenges

One of our design goals is to make developing challenges for CTFs as simple as
possible so that developers can focus on the content and not quirks of the
platform.  We have specific challenge types that make it as easy as possible to
create new challenges of a particular flavor, and the documentation for each
type and how to use them are in the [examples](examples/) directory.

Additionally, we have a simple interface for creating automated solvers for
your challenges.  It is as simple as creating a directory named `solver` with
a Python script called `solve.py`.  This script will get its own Docker
container on the same network as the instance it is checking and start with
all of the artifact files and additional information provided to competitors in
its working directory.  (For artifact-only challenges — those that publish no
ports — there is no instance to connect to, so the solver container runs on
Docker's default network with no `challenge` host and works from the build's
artifacts; outbound network access is preserved.)  Once it solves the
challenge, it just needs to write the flag value to a file named `flag` in its
current working directory and **cmgr** will validate the answer and report it
back to the user.

In both the challenge and solver cases, we support challenge authors using
custom Dockerfiles to support creative challenges that go beyond the most
common types of challenges.  In order to support the other automation aspects
of the system, there are some requirements for certain files to be created
during the build phase of the Docker image and are documented in the `custom`
challenge type example.

Testing challenges is meant to be as easy as executing `cmgr test` from the
directory of an individual challenge or the directory containing all of the
challenges for an event.  This is intended to support quick feedback cycles
for developers as well as enabling automated quality control during the
preparation for an event.

### Front-Ends

Another design of this project is to make it easier for custom front-end
interfaces for CTFs to reuse existing content/challenges rather than forcing
organizers to port between systems.  To make this possible, `cmgrd` exposes a
very simple REST API which allows a front-end to manage all of the important
tasks of running a competition or training environment.  The OpenAPI specification
can be found [here](cmd/cmgrd/swagger.yaml).

**Note:** If your front-end needs to supply user state or dynamic configuration
to a challenge natively, the `POST /builds/<id>` endpoint optionally accepts a
JSON payload containing a `user_id` identifier and a map of `env` variables.
To prevent namespace pollution, all variables passed through this REST map are
prepended with a `CMGR_` prefix automatically before being injected into the container execution context.
In multi-container challenges, the `user_id` and all `env` variables are propagated
equally to **every** container in the build — there is no per-container filtering.

**Note:** When a build is rebuilt, its on-demand instances (those launched via
`POST /builds/<id>`) are stopped and removed, **not** restarted, since the
original `user_id` and `env` payload are not retained; a `GET` on one of them
afterwards is a 404. Front-ends that automate rebuilds must re-issue
`POST /builds/<id>` with the appropriate runtime configuration to bring those
instances back up. Persistent (schema-managed) instances are restarted
automatically as before, pulling the new image before the old containers go
(under a five-minute ceiling, or `CMGR_WORKER_PULL_TIMEOUT` when that is
longer);
one that cannot be restarted (its worker down, the pull or the start failing)
is removed like any stop on a down worker, and the same update relaunches it
through placement once the build's restarts are done.

**Note:** Challenge metadata includes a derived `delivery_type` field
(`"service"`, `"artifact_only"`, or `"flag_only"`) describing what competitors
receive: a running network service, only downloadable build artifacts, or only
a flag-submission prompt, respectively.  Only `service` challenges run
instances: builds of non-service challenges always report an empty instance
list, and requests to start an instance of them fail.  (Instances left over
from older cmgr versions, which still launched placeholders for artifact-only
challenges, are torn down on the next schema update or challenge rebuild.)
An empty instance list on a `service` build with a *positive* `instance_count`
indicates a deployment problem; on-demand builds (`instance_count` of -1)
legitimately have zero instances whenever no user session is active.
(`"flag_only"` challenges — declared via the `flag-only` challenge type — are
bare submission prompts whose builds carry only a flag and lookup data, with
no artifacts; payloads from older cmgr versions omit `delivery_type` entirely,
in which case every build has instances as before.)

**Note:** Runtime env injection (`user_id` / `env`) is currently only exposed
through the `cmgrd` REST API. The `cmgr` CLI commands (`start`, `playtest`,
etc.) always launch instances with no injected env, so challenges that depend
on `CMGR_USER_ID` or other runtime variables will not behave identically under
the CLI as they do in production. (TODO: surface these as CLI flags if needed.)

### Back-End

If you're interested in contributing, modifying, or extending **cmgr**, the
core functionality of the project is implemented in a single Go library under
the `cmgr` directory.
Additionally, the _SQLite3_ database is intended to function as a read-only
API and its schema can be found [here](cmgr/database.go).

In order to work on the back-end, you will need to have _Go_ installed and
_cgo_ enabled for at least the initial build where the _sqlite3_ driver is
built and installed.  To get started, you can run:

```sh
git clone https://github.com/CyLabAcademy/challenge-orchestrator
cd challenge-orchestrator
go get -v -t -d ./...
mkdir bin
go build -v -ldflags "$(sh ci/version.sh --ldflags)" -o bin ./...
go test -v ./...
```

## Acknowledgments

This project is heavily inspired by the
[picoCTF](https://github.com/picoCTF/picoCTF) platform and seeks to be a next
generation implementation of the _hacksport_ back-end for CTF platforms built
in its style.

## Contributing

Please carefully read the [NOTICE](Notice), [CONTRIBUTING](CONTRIBUTING.md),
[DISCLAIMER](DISCLAIMER.md), and [LICENSE](LICENSE) files for details on how
to contribute as well as the copyright and licensing situations when
contributing to the project.
