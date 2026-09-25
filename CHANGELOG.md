# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.2.0] — 2026-09-25

### Added

**Launch latency is measured, and can be exported to CloudWatch.** cork timed
nothing it did, so how long a player waited for a container was knowable only
from the logs of one launch at a time, never in aggregate. Every launch is now
timed at the stage boundaries that already existed: the image check and pull,
the wait for a launch slot, the network create, and the container create and
start.

`cork worker-list` summarizes each worker from a ring on its daemon
(`launch=p50 1.2s/p90 4.1s/max 8.4s n=340 failed=12`), and `GET /workers`
carries the same figures under `launch`. That needs no configuration, and works
where CloudWatch does not — an e2e run, the test VM, an operator on the box
mid-incident.

Setting `CORK_EMF_ENDPOINT` to a local CloudWatch agent's embedded-metric-format
socket additionally sends one record per launch, carrying the stage breakdown
and what the daemon was doing at the time: launches queued, slots busy, whether
the image had to be pulled. The records hold raw values, so CloudWatch computes
true percentiles from them rather than a mean that hides the tail a player
feels. A worker is named by its player-facing name, which an autoscaled fleet
keeps permanent per slot, so a machine replaced from the AMI keeps its series.

cork keeps no time series, and the export cannot slow a launch: the launching
goroutine only reads the clock, inserts into the ring and makes a non-blocking
queue send, while a separate goroutine owns the socket. A full queue drops
rather than waits, and an agent that is slow, wedged or absent is never waited
on. Off unless an endpoint is configured; see `corkd --help` for
`CORK_EMF_ENDPOINT`, `CORK_EMF_LOG_GROUP` and `CORK_EMF_NAMESPACE`.

### Fixed

**A challenge's seccomp profile may contain `=`.** Cork passed security options
to Docker in the deprecated `key:value` form, but Docker splits an option at its
first `=` whenever it has one, so a profile with an `=` anywhere in its JSON was
cut inside the profile and every launch failed with `invalid --security-opt 2`.
A per-syscall `comment` is enough: Docker's profile format allows it and cork's
validation ignores it, so such a profile passed validation and then never
launched. Options now go as `seccomp=` and `no-new-privileges=true`, and workers
stop logging Docker's colon-separator deprecation warning on every launch.

`docker inspect` on new containers shows the `=` form; containers already
running keep the colon form, which Docker still accepts, so nothing migrates.

**A raced container removal no longer fails a rebuild.** When two teardowns of
the same instance overlap — a stop the platform asked for, and a rebuild's own —
the loser is told the removal is already in progress and moves on. But the
container holds its endpoint for a few milliseconds more, so the network removal
that follows was refused, and the whole `update-schema` failed with it. That
conflict is now recognised as the race it is: the network is left to
docker-reaper, which already clears a `cmgr-<id>` cork no longer records. A
network still held for any other reason fails the stop exactly as before.

**A worker is no longer ejected over one slow docker call.** Any call that hit
`CORK_WORKER_CONTROL_TIMEOUT` took its worker out of placement, and a slow daemon
is ordinary: while dockerd deletes an image, docker-reaper evicting one for
space, every container create, remove and image check on that worker waits for
it. A timeout now has cork ping the daemon. No answer ejects the worker at once,
as before; an answer keeps it in placement until `CORK_WORKER_TIMEOUTS_TO_EJECT`
separate stalls (3) land within `CORK_WORKER_TIMEOUT_WINDOW` (2m). One stall
counts once, however many calls it timed out together. Refused and reset
connections still eject at once.

A launch that timed out that way answers 503 with Retry-After, so the platform
places it again, rather than a 500. A stop that timed out that way succeeds, as a
stop on an ejected worker always has: the records are cleared and docker-reaper
removes whatever the teardown did not finish.

## [1.1.2] — 2026-09-23

### Fixed

**A launch reports the player-facing address as `hostname`**, the key the
platform reads it by, restoring an interface contract this drifted away from.
With a challenge server's `dynamic_public_hostname` set, the platform stores the
whole launch response and looks up `hostname` to resolve the `{{server}}` and
`{{http_base}}` tokens in a challenge description; cork reported the same value
only as `worker_public`, so those tokens went unresolved and players were shown
a literal `{{server}}`.

`worker_public` stays, and `worker` stays as the private orchestration address
that nothing player-facing should read. `hostname` is the same value from the
same read-time lookup, so the two cannot disagree — a test asserts it.


## [1.1.1] — 2026-09-23

Three fixes found while deploying a 534-challenge corpus onto a fresh build
plane. All three are about a failure telling you what it was.

### Fixed

**A symlinked `CORK_DIR` is refused rather than quietly emptying the catalogue.**
The challenge directory was validated with `stat`, which follows a link, but is
walked with `lstat`, which does not — so a symlinked tree passed validation and
then inventoried nothing. An empty inventory is not an error, so every challenge
on record counted as removed: ones with no builds were deleted and the run exited
0, while ones with builds hit a foreign-key constraint that rolled the delete
back and reported `FOREIGN KEY constraint failed` instead of the real problem.
Measured: 8 challenges on record, one `update` through a symlink to the same
tree, 0 left. Serve a tree from elsewhere with a bind mount, which is
indistinguishable from a real directory to both the check and the walk.

**Build and artifact failures name the challenge they belong to.** Every error
path in `executeBuild` reported only its underlying cause, and those causes do
not identify anything: `failed to build image: process "/bin/sh -c make main"
did not complete successfully: exit code: 2` is the same sentence for every
challenge of a type, and `could not cache artifacts: artifact "source.tar.gz"
is …` names a file dozens of challenges could produce. A deploy that reported
seven of the first offered no way to tell which seven short of re-running the
whole thing under `--verbose`. Each now carries the challenge id, build id and
seed.

### Changed

**Base pins are documented as `<CORK_DIR>/.base-pins.json` with `CORK_BASE_PINS`
left unset**, so they travel with the tree they are committed alongside — which
is what `cork-build --dir` needs when a runner builds from a checkout in its
workspace. BUILDER.md previously advised a non-dotted file at an explicit path,
reasoning that a dotfile goes uncommitted; it does not, and an absolute path goes
on resolving to the old location when the tree moves, which refingerprints every
build rather than failing. Behaviour is unchanged; the advice is what moved.

## [1.1.0] — 2026-09-21

### Added

**A second health axis on every worker.** `GET /workers` gains `reachable`
(`ok` / `unresponsive` / `down`), from the worker's docker daemon, and `load`
(`ok` / `overloaded` / `unknown`), from its telemetry agent, alongside `since`
and `reason`. `health` stays as a derived, deprecated string: `down` or
`unresponsive` when the worker is not reachable, `overloaded` when its load
says so, otherwise `ok`. New in that field's vocabulary is `unresponsive`,
which older clients never saw.

**A real probe against dockerd**, rather than only sampling it with real
traffic. `/_ping` on every tick, plus a one-container list on a slower one,
since a daemon can answer a ping while containerd is wedged. A wedged daemon on
an idle worker used to be found by the next launch, thirty seconds after a
student asked for it.

**Eleven `CORK_WORKER_*` settings** for the probe cadence, timeouts, miss
thresholds and recovery backoff, each with its `CMGR_` fallback like the rest.
`corkd --help` lists them.

**A retryable answer for a fleet that is merely rebooting.** A launch refused
because every worker is unresponsive is now a 503 with `Retry-After`; every
worker being `down` remains a 500, because a fleet an operator took down is not
going to resolve itself.

### Changed

**Losing a worker is no longer permanent.** The two axes were one value before,
which meant a telemetry agent restarting during a deploy could take a healthy
box — dockerd fine, instances serving — out of the fleet until an operator ran
`worker-add`. An unknown load now places: a box whose sidecar died is almost
always still serving.

`unresponsive` is cork's own verdict and reverses itself: the worker keeps
being probed, and once its daemon answers it reconnects and reconciles before
rejoining placement, the same sequence `worker-add` performs. Repeated failures
are retried more slowly, and that is forgiven after a spell of running clean.
A reboot or a `systemctl restart docker` therefore costs seconds of placement
rather than an operator's attention.

`down` stays what it was — asserted by `worker-down`, lifted only by
`worker-add` — because an operator taking a box out of service knows something
the probes do not. Use it before a **termination**, not before a reboot: a
reboot now needs nothing, and marking a box down first opts out of the
recovery above.

**Retimed.** Probes every 5s (was 500ms), ejecting after 6 consecutive misses
(was 60). Same 30-second window, but misses are counted consecutively, and at
the old cadence a worker answering one poll in sixty never tripped the
threshold at all.

**`cork worker-list` prints both axes**, how long the worker has held the
reachable one, and why — a second line per worker when there is a reason to
show. Anything parsing that output positionally will need updating; the API is
the stable contract.

### Fixed

`worker-add` given no public address now keeps the one already stored instead
of clearing it. It doubles as the way to force an immediate reconnect, so its
one-argument form has to be safe to run on a worker that is already
registered — and that column is the address players are sent to.

## [1.0.1] — 2026-09-18

No code changes. 1.0.0 published no binaries — the workflow uploaded assets
after publishing, which an immutable release refuses — so that release is
permanent and empty. This is the first tag carrying binaries.

## [1.0.0] — 2026-09-18

The first release under the cork name.

cork is a fork of [cmgr](https://github.com/picoCTF/cmgr), the challenge manager
from the US Army Cyber Institute, rebuilt around one thing cmgr could not do:
run a CTF across many hosts. cmgr holds a single connection to a single docker
API, which caps a deployment at one box. cork places challenges across a fleet of
workers from one orchestrator, and splits building away from serving entirely.

Challenge files are unchanged. Anything cmgr or PCM produces, cork reads.

### Added

**Multi-host orchestration.** One orchestrator places instances across many
workers, each worker reached over mTLS to its docker daemon. Port assignments are
indexed by worker and port, workers can be marked down through the API, and
worker timing is tunable from the environment.

**A separate build plane.** `cork-build` holds the challenge tree and every
schema operation, builds images, pushes them to the registry, and hands finished
builds to the orchestrators that serve them. An orchestrator running with
`CORK_BUILD_PLANE=external` builds nothing and answers 409 to any command that
would. Schemas route to the orchestrator they name in `destination:`, and
`migrate-schema` moves one between orchestrators without rebuilding.

**The registry as the source of truth.** Tags are content-addressed, so identical
content is the same tag wherever it was built. The orchestrator asks the registry
whether a tag exists rather than trusting the plane that sent it, recomputes
every identity it is handed, and refuses a schema another orchestrator already
serves. Images are pushed write-once and never over an existing tag.

**Build identity and caching.** A build's identity covers its source generation
and the built-in Dockerfile, so a change to either rebuilds. BuildKit is selected
explicitly rather than inherited from the daemon, base images pin by digest
without editing challenge files, and an inline layer cache publishes to the
registry.

**Capacity and failure handling.** Launches are refused immediately when a
worker's backlog exceeds the launch wait, teardowns in flight are bounded, a busy
or wedged worker fails fast, and a build a failed rebuild left behind is
relaunched rather than stranded.

**A fleet simulation.** `e2e/` stands the whole deployment up under docker
compose — orchestrator, build plane, registry, two workers with telemetry, real
PKI — and drives a launch end to end. `e2e/run-single.sh` covers the one-box
shape.

**linux/arm64 builds**, published and tested, alongside linux/amd64.

### Changed

- **The binaries are `corkd`, `cork` and `cork-build`.** `corkd` is the daemon,
  `cork` a thin HTTP client against it, `cork-build` the build plane.
- **Settings are `CORK_<name>`.** Each still answers to its `CMGR_<name>`
  spelling when the `CORK_` name is unset, and `cork` reads `CMGRD_SERVER` when
  `CORK_SERVER` is unset. `corkd` lists any `CMGR_` names it finds at startup.
- **The default database file is `cork.db`**, previously `./cmgr.db`. Nothing
  moves an existing file, so a deployment that never set this starts against an
  empty database unless `CORK_DB` points at the old path. See *Upgrading*.
- **Artifact archives stay on the build plane**, written to a directory per
  destination under `CORK_ARTIFACT_DIR` and published by an artifact server
  beside it. A hand-over carries only `has_artifacts`.

The cmgr name deliberately survives in four places, each a contract rather than a
spelling: the `CMGR_` variables a challenge container sees, the docker labels and
object names (`cmgr.managed`, `cmgr-<id>`) that docker-reaper and the reconciler
match on, image tags, and `CN=cmgr` — the orchestrator's client-certificate
identity, which is deployed and would have to be reissued.

### Removed

- **Artifact serving.** `GET /builds/<id>/artifacts.tar.gz` and its per-file path
  are gone, along with the `artifacts` command. cork serves no artifacts; an
  orchestrator on an external build plane ignores `CORK_ARTIFACT_DIR` and says so
  at startup. **This is the one breaking change for an existing deployment.**
- **The challenge development commands.** No `test`, `playtest` or `check`, and
  no solver framework behind them. Use [cmgr](https://github.com/picoCTF/cmgr)
  or PCM to write and debug challenges; cork reads what they produce.
- **Telemetry**, now its own repository:
  [cork-telemetry](https://github.com/CyLabAcademy/cork-telemetry).

### Deprecated

Release tarballs carry `cmgrd` and `cmgrd-cli` as symlinks to `corkd` and `cork`,
and the same tarball is published under the pre-rename
`cmgr_<os>_<arch>.tar.gz` name. Those aliases and the `CMGR_` setting names are
scheduled for removal in **1.2.0**.

`CORK_ARTIFACT_DIR` is the one setting another program reads by its old name:
[cmgr-artifact-server](https://github.com/picoCTF/cmgr-artifact-server) looks up
`CMGR_ARTIFACT_DIR`. Setting only that name keeps both working today; once the
fallback goes, a build plane needs both.

### Fixed

- A schema's bundles move with it when it is given a destination.
- A challenge's seccomp profile survives the hand-over.
- Handed-over challenge ids are validated and their build ids adopted.
- A registry read that goes unanswered is retried, and says what to re-run.
- Schema operations serialize against rebuilds.
- Pruning only removes a bundle that can belong to the build in hand, and refuses
  a scan that did not happen.
- Every image is resolved against the registry before it is built.
- Stale builds rebuild on every persisted verdict.
- A challenge with several problems reports all of them at once, in a stable
  order. Validation collapsed to a single error at three levels, so an author
  who both named a missing artifact and published an unused one was told only
  about the unused file — never the typo that usually causes both — and fixed
  one mistake per rebuild. Which problem they saw also varied run to run.

### Upgrading from cmgr

1. Anything consuming `GET /builds/<id>/artifacts.tar.gz` must read bundles from
   the artifact server beside the build plane instead.
2. Rename settings to `CORK_*` when convenient — the `CMGR_*` names still work
   until 1.2.0, and `corkd` reports the ones it finds.
3. **Set `CORK_DB` before starting**, or move `cmgr.db` to `cork.db`. The default
   changed and nothing migrates the file, so a daemon that never set it comes up
   against an empty database and reports no error — every existing challenge,
   build and instance simply is not there.
4. A single-host deployment needs no build plane: leave `CORK_BUILD_PLANE` unset
   and `corkd` builds on its own docker daemon, as cmgr did.

[1.2.0]: https://github.com/CyLabAcademy/challenge-orchestrator/releases/tag/v1.2.0
[1.1.2]: https://github.com/CyLabAcademy/challenge-orchestrator/releases/tag/v1.1.2
[1.1.1]: https://github.com/CyLabAcademy/challenge-orchestrator/releases/tag/v1.1.1
[1.1.0]: https://github.com/CyLabAcademy/challenge-orchestrator/releases/tag/v1.1.0
[1.0.1]: https://github.com/CyLabAcademy/challenge-orchestrator/releases/tag/v1.0.1
[1.0.0]: https://github.com/CyLabAcademy/challenge-orchestrator/releases/tag/v1.0.0
