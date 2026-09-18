# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[1.0.1]: https://github.com/CyLabAcademy/challenge-orchestrator/releases/tag/v1.0.1
[1.0.0]: https://github.com/CyLabAcademy/challenge-orchestrator/releases/tag/v1.0.0
