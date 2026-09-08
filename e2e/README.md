# cork end-to-end simulation

A compose project that runs the whole cork fleet on one machine and drives
the real launch sequence through it: `cmgrd` built from this checkout, the
docker daemon it builds on, a zot registry, and two docker workers with the
telemetry agent, all wired with the PKI that `config-examples/gen-docker-certs.sh`
mints. It replaces nothing: the `cmgr test` suite that CI used to run built
and solved every example against one daemon and is gone since 4c81a10; this
exercises what that never could, placement across workers, the registry round
trip, worker health, rebuilds, and teardown across boxes, with four challenges
and no solvers.

```
                 docker compose run --rm e2e  (scenario.sh)
                               │ cmgrd-cli / curl
                               ▼
 host:4200 ──▶ cork (cmgrd) ── DOCKER_HOST ──▶ builder (dind, tcp 2375) ── push (CN=cmgr) ──▶ zot.internal:443 (mTLS)
                 │                                                                              ▲
                 │ mTLS :2376 (CN=cmgr, server name academy-docker-worker)                      │ pull (CN=worker)
                 │ telemetry :2136                                                              │
                 ├──▶ worker-a 172.28.0.11 (dind) + worker-a-telemetry ─────────────────────────┤
                 └──▶ worker-b 172.28.0.12 (dind) + worker-b-telemetry ─────────────────────────┘
 host:20000-20029 ──▶ worker-a instance ports          host:21000-21029 ──▶ worker-b instance ports
```

| compose service | production box | provisioned by |
| --- | --- | --- |
| `cork` | orchestrator: `cmgrd` + `cmgrd-cli` | ansible role `cork` |
| `builder` | the orchestrator's local docker daemon (image builds, pushes) | ansible role `docker` |
| `zot` | image registry | ansible role `zot` |
| `worker-a`, `worker-b` | challenge workers: dockerd behind mTLS | ansible role `multihost_docker` |
| `worker-*-telemetry` | cork-telemetry on each worker (same network namespace) | `multihost_docker`, `telemetry.yml` |
| `certgen` | the offline PKI step | `gen-docker-certs.sh` on an operator machine |

Every box only receives the certificate files it gets in production, from a
volume per box and trust domain, so the CN-based split between the two CAs
is exercised for real: the builder pushes as `cmgr`, workers pull as `worker`,
the CA private keys sit in a volume nothing else mounts.

## Running it

Needs a recent Docker Engine with compose v2 (developed against Docker Desktop
29.7 with the WSL2 backend on Windows; plain Linux engines work the same),
roughly 5 GB of disk, and internet access for base images and the apt runs
inside challenge builds. The workers and the builder are `docker:dind`
containers and therefore run privileged.

The workers add the `nft` binary to that image (`worker.Dockerfile`) and ask
for docker's nftables firewall backend in `worker/daemon.json`, which is where
production is going: with iptables, the time to set up a challenge network
grows with the number of networks already on the box. `DIND_VERSION` (default
29.6.0) sets the docker version for the workers and the builder together —
29.6.0 is the first release that execs `nft` instead of linking libnftables,
which aborted once a netlink socket landed past fd 1024 (moby#52873). The
fleet-wait step fails if a worker comes up on iptables anyway.

```sh
cd e2e
docker compose up -d --build   # PKI, registry, builder, workers + telemetry, cork
docker compose run --rm e2e    # the scenario
```

`./run.sh` runs the regular mode; `./run.sh --full` (or `E2E_FULL=1`) adds the
steps that are only worth their seconds when nobody is waiting: the registry's
failure paths, artifact delivery, multi-container challenges, cold pulls,
docker-reaper and container resource limits. A regular run names the full-mode
steps it did not take, and its verdict says which mode it was, so the two are
never confused for one another.

Full mode also converges a **second schema**, `schema-full.yaml`, alongside the
first. It holds one challenge, the multi-container `aptitude-and-privileges`,
and is separate for two reasons: a schema is built when it is added, so putting
that challenge in `schema.yaml` would charge regular mode for two `openssh`
images it never launches; and running two schemas at once is itself the shape
production uses -- a schema per event -- which nothing else here exercises.

`./run.sh` does both and stamps the cork image with `git describe`. A scenario
run takes three to four minutes on a fast connection, most of it building and
rebuilding the example challenges on the builder and waiting out the two 30 s
health timeouts (the first `up` also pulls the base images and compiles cork,
a few minutes more). The scenario sweeps `cmgr.managed` containers and
`cmgr-*` networks from the workers when it starts, so do not run it against a
fleet whose instances you want to keep.

The scenario exits non-zero at the first failed check, with the step it was in.
Look at `docker compose logs -f cork` alongside it: every placement decision,
pull, and health transition is logged there.

## What the scenario checks

1. **Fleet up.** cmgrd, zot (with cork's client certificate), the builder, and
   each worker's dockerd (with cork's certificate, pinned to the shared server
   name) and telemetry endpoint answer. Any `cmgr.managed` containers and
   `cmgr-*` networks a failed earlier run left on a worker are swept, as
   docker-reaper would; instance ids are reused after a schema removal, so
   leftovers would otherwise collide.
2. **Worker registration.** `cmgrd-cli worker-add <ip> <public name>` for each
   worker; both reach health `ok` from telemetry polling and the public
   address is recorded.
3. **Challenge scan.** `cmgrd-cli update` over the challenge volume (seeded
   from `../examples`); the schema's challenges are listed.
4. **Schema converge.** `cmgrd-cli add-schema schema.yaml` builds four
   challenges on the builder daemon, pushes them to zot, and starts the one
   persistent instance. The flag-only challenge gets a flag and lookup data but
   no instance; the persistent instance sits on a worker, reports that worker's
   public address and a port inside `CMGR_PORTS`, its container is on that
   worker's dockerd, and the service answers.
5. **Registry.** Every build's instance image is in zot under its
   content-addressed tag (`s<seed>-<checksum>-<host>`), as read back with the
   read-write identity.
6. **On-demand launches.** Four `POST /builds/<id>` calls with `user_id` and
   `env`, as the platform makes them. They land two per worker; each worker
   now holds the image (pulled from zot with the read-only identity, unless a
   previous run left it there); each instance is reachable at
   `worker_public:port`, serves its own `CMGR_USER_ID`, `CMGR_CUSTOM_VAR`, and
   the build's flag, and its container is on the recorded worker.
7. **remote-make.** An on-demand instance of the compiled challenge answers
   over TCP, then stops: container and network gone from the worker.
8. **Stop path.** `DELETE /instances/<id>` removes the containers and network
   on the worker; a second delete is a 204.
9. **cmgrd restart** (outer socket). cork is restarted; the workers come back
   from the database and are polled to `ok`, existing instances are still
   known with their worker and still served, new launches work.
10. **Update, generation 2.** The scenario edits the on-demand and the
    persistent challenge's sources and runs `cmgrd-cli update --prune-old`.
    Both rebuild: the build keeps its id and flag, gets a new checksum with the
    old one as `prev_checksum`, and zot holds both the new tag and the
    generation-1 tag (the rollback target). All on-demand instances are torn
    down on their workers and not restarted; the persistent instance is
    restarted in place with new containers serving the new content; a fresh
    launch serves generation 2.
11. **Update, generation 3 with `--prune-old`.** A second edit of the
    on-demand challenge only rebuilds that one: generation 2 becomes the
    rollback target, generation 1 is deleted from
    zot, the generation-2 instance is torn down, and four fresh launches
    across both workers serve generation 3.
12. **worker-down.** After `cmgrd-cli worker-down`, new launches avoid that
    worker; stopping an instance that lives on it clears cmgrd's records but
    leaves the container running on the worker (docker-reaper's job in
    production, so the scenario reaps it itself); `worker-add` brings the worker
    back.
13. **Telemetry silence** (outer socket). The worker's telemetry sidecar is
    stopped; cmgrd marks the worker down after 30 s while its instances keep
    running, keeps it down when telemetry returns (down is sticky), and
    `worker-add` recovers it.
14. **Overloaded** (outer socket). The sidecar is replaced by a responder that
    always reports overloaded: the worker is skipped for placement but not
    down, a stop on it still tears down over docker, and it returns to `ok` by
    itself once the real sidecar is back.
15. **Every worker unavailable.** Both overloaded: `POST /builds/<id>` is a
    503 with `Retry-After: 1` (outer socket). Both administratively down: a
    500. `worker-add` restores both.
16. **Hung dockerd** (outer socket). The worker container is paused while its
    telemetry keeps answering; a stop on an instance there fails after the
    30 s control timeout and marks the worker down, a second stop is a 204
    that clears the records, and after unpausing, `worker-add` recovers it.
17. **worker-remove.** The worker is purged: it disappears from the list, its
    instance records are gone, its containers are still running (reaped by the
    scenario), and it can be re-added clean.
18. **Base image pins** (full mode). `cmgrd-cli pin-refresh` resolves every base
    the corpus names to a digest — `ubuntu:24.04` to a `sha256:`, and
    `cmgr/examples-guestfish-base`, which the `disks` example builds locally and
    no registry serves, reported and left unpinned without discarding the ones
    that did resolve. A rebuild then runs through the pins: cork rewrites `FROM`
    in the build context, the challenge `Dockerfile` on disk is checked to be
    untouched, and the image is pushed. The pushed image config is fetched back
    out of zot and required to carry `moby.buildkit.cache.v0`. Late on purpose:
    a refresh moves the pin fingerprint, which enters every build's content
    identity, so the steps that compare generations must not straddle it.
19. **Inline cache import** (full mode). The step above proves only that the
    metadata is *published*. This one proves it is *used*: the builder's build
    cache is pruned until it holds zero records, one more generation is built,
    and **every layer but the last** is required to carry the same digest as
    before while the last is required to have moved. Re-running an instruction
    produces a different digest — a layer tar carries the mtimes that run wrote
    — so with the cache verifiably empty, identical digests can only have come
    from the registry. The step asserts the image's exact layer count first,
    because the comparison is only meaningful if it spans layers cork *built*:
    base layers arrive from the registry either way. Without this, a `CacheFrom`
    that names nothing importable passes every other assertion in the suite.
20. **Teardown.** Remaining instances are stopped, `remove-schema` destroys the
    builds, the workers hold none of the instance containers or networks, the
    builder and zot hold no tag of any generation, and the edited sources are
    restored.

Regular mode asserts that a launched container really went through the
interceptor, not merely that dockerd was configured with it: `/etc/hosts`,
`/etc/hostname` and `/etc/resolv.conf` are mounted read-only in a live
instance's container, and still mounted (docker points a container on a
user-defined network at its embedded resolver by writing into that
bind-mounted `resolv.conf`, so removing the mount would break resolution of
sibling containers by name). Nothing else in the fleet makes those three
read-only, so it fails if the interceptor is out of the create path.

That check deliberately says nothing about *how* the interceptor is invoked,
because it cannot: docker's generated wrapper still runs the interceptor, so
the spec is still rewritten and the mounts are read-only either way. The
non-exec'ing wrapper — the thing that leaks shims — is caught in the
fleet-wait step instead, which requires dockerd to have registered our own
wrapper path with no `runtimeArgs`. A generated wrapper registers a path under
`/var/lib/docker/runtimes/`, and that is the difference the fleet can see.

Full mode adds, among others: a **multi-container** launch (two containers on
one `cmgr-<id>` network, only the front box published, a per-stage `overrides:`
block giving each container its own CPU ceiling, the private `builder` stage
kept out of the registry, and the flag fetched over ssh from the back box
through the front one); and a **database-busy** step, the only coverage of
`ErrDatabaseBusy` in either handler, which holds SQLite's write lock with
`sqlite3` from outside cmgrd and requires a launch and a stop to be answered
`503` + `Retry-After` and to go through unchanged on the retry. That step is
the one place the scenario reaches behind the HTTP API, which is why the `e2e`
service mounts `cork-data` and why `e2e/cork.Dockerfile` carries `sqlite`
(`openssl` is there for the stalled-registry fixture, an `s_server` that
accepts a connection and never answers).

Steps marked *outer socket* (9, 13, 14, the 503 half of 15, and 16) need
`/var/run/docker.sock` in the `e2e` container and are skipped otherwise, or
with `E2E_CHAOS=0`; each prints `skip`, and the run still ends in ALL STEPS
PASSED, so check for skips when the socket matters. The chaos steps target
whichever worker does not host the persistent instance. If a run dies
mid-step, an EXIT trap unpauses the worker and restores the telemetry sidecar.
`schema.yaml`, `schema-full.yaml` and the challenge ids at the top of
`scenario.sh` go together.

## Poking at it by hand

```sh
docker compose exec cork cmgrd-cli worker-list
docker compose exec cork cmgrd-cli list
docker compose exec cork cmgrd-cli add-schema /opt/e2e/schema.yaml   # only inside the e2e service; from cork use a path under /challenges
curl -s localhost:4200/workers | jq
curl -s -X POST localhost:4200/builds/1 -d '{"user_id":"me"}' | jq
docker compose exec worker-a docker ps          # what really runs on a worker
docker compose exec cork curl -s --cacert /etc/docker/certs.d/zot.internal/ca.crt \
    --cert /etc/docker/certs.d/zot.internal/client.cert --key /etc/docker/certs.d/zot.internal/client.key \
    https://zot.internal/v2/_catalog
```

Instance ports are published from the workers as-is for worker-a
(`localhost:20000-20029`) and shifted by 1000 for worker-b
(`localhost:21000-21029`); inside the compose network the workers are
`worker-a`/`worker-b`, which is also what cmgrd reports as `worker_public`.

Failure modes worth trying while watching `cmgrd-cli worker-list`:

```sh
docker compose stop worker-b-telemetry    # 30 s of silence -> down (sticky)
docker compose pause worker-b             # next control call hangs 30 s -> down
docker compose kill worker-b              # next control call is refused -> down
docker compose exec cork cmgrd-cli worker-add 172.28.0.12 worker-b   # recovery, once the box is back
docker compose exec cork cmgrd-cli worker-remove 172.28.0.12         # purge it and its instance records
```

## Lifecycle

`docker compose down` keeps the volumes: the PKI, the challenge tree, cmgrd's
database, the builder's and workers' image caches, and zot's store. Bringing the stack back
is then a fleet restart: cmgrd reloads its worker table and instance records,
and the workers' dockerds restart the instance containers (they run with
`restart: always`, as in production). `docker compose down -v` wipes all of
it, including the PKI, which `certgen` re-mints on the next `up`.

`CHALLENGE_DIR=/path/to/challenges docker compose up -d` seeds the challenge
volume from a different tree (only when the volume is empty, so `down -v`
first to switch); the scenario itself is tied to the example challenges, so
drive other content with `cmgrd-cli`.

## How this differs from production

- The builder is reached over plain TCP inside the compose network instead of
  the orchestrator's unix socket, and it is a separate container.
- Worker dockerds run the subset of `daemon.json` that matters to cork: hosts,
  TLS, address pools, logging, the **nftables firewall backend** and
  **oci-interceptor as the default runtime**. Still absent: userns remap,
  cgroup parent, XFS quotas, docker-reaper, ufw. The address pool is
  `10.201.0.0/16` rather than `192.168.0.0/16` to stay clear of home LANs.
- The interceptor is built from source for musl (`worker.Dockerfile`) because
  the project releases only a glibc binary and these workers are Alpine, and
  `daemon.json` names **a wrapper script that execs**
  (`worker/oci-interceptor-runtime.sh`), not the binary — the same shape the
  `multihost_docker` role deploys, and for the same reason. Docker generates
  its own wrapper for any runtime whose `runtimeArgs` is non-empty and that
  generated wrapper does not `exec`, so it stays in the process tree between
  the containerd shim and runc; a cancelled `runc delete` then orphans the
  runtime and leaks its shim. Keep this file and the role's
  `oci_interceptor_runtime.sh.j2` in step: the point of running the interceptor
  here is that the fleet exercises the runtime chain production runs.
- Telemetry samples the docker VM's `/proc`, not a per-worker host, so it only
  reports overloaded when the whole machine is.
- The registry is `zot.internal` on 443 rather than `<host>:5000`: the address
  names the certs.d directory, and a mount target with a colon is refused by
  the daemon.
- No artifact server; artifacts land in the `cork-data` volume.
- The stack mounts the outer docker socket into the `e2e` service. It is
  host-root-equivalent and is used for five steps: restarting cork, stopping
  or replacing a telemetry sidecar (silence, overloaded, all overloaded), and
  pausing a worker's dockerd. Drop the mount or set `E2E_CHAOS=0` to run
  without it and lose exactly those.
- The docker socket mount aside, nothing here touches the host daemon: every
  challenge image and container lives inside the dind daemons.
- cork-telemetry is pinned to a release (`CORK_TELEMETRY_REF` in
  `telemetry.Dockerfile`); the ansible role installs the latest release.
