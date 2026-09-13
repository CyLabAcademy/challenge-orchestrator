# The builder

Notes for whoever operates or changes cork's image build path. Everything here
concerns the **orchestrator** host: the one machine that runs `cmgrd`, builds
images and pushes them to the registry. Workers only pull.

That machine can also be told not to build at all. `CMGR_BUILD_PLANE=external`
starts `cmgrd` with no docker daemon and no challenge tree: everything in this
document then happens wherever the images are built, and each build reaches
the daemon through `PUT /challenges/<id>` (issue #18): the challenge as scanned
there and its builds as left there, with the images already in the registry,
verified before anything is recorded (every build's content checksum
recomputed from the inputs the payload names, every image tag looked up in the
registry). It is JSON and nothing else — artifact bundles stay on the build
plane, and `has_artifacts` records that a build published one rather than
carrying it. A build whose row already serves the generation handed over is
not built over
again: handing the same thing over twice leaves the images and the
flag as they are, and converges only what runs them, since the `instance_count`
the hand-over carries is the schema's and may have moved. `update`, manual
builds and the pins commands answer 409, a schema converge only checks that
every build it wants has already arrived, and a launch with no worker
registered fails instead of running locally. `add-schema` has no successful
path there, since the builds handed over are the schema: `update-schema` is
the operation. The default, `local`, is the daemon this document describes.

## The shape of a deployment

One build plane, one registry, one orchestrator per event, and each
orchestrator's own workers. The [README](README.md#the-shape-of-a-deployment)
draws it and says why each count is what it is; two of those counts are what
the rest of this document rests on:

- **One build plane**, because it is the only thing that sees every schema —
  which is what makes the exclusivity rule below enforceable at all.
- **One registry**, shared and content-addressed, because that is what makes
  the same content the same tag wherever it was built, and so what lets a
  schema move between orchestrators without rebuilding anything.

A single-host deployment collapses all of it onto one box and drops the
registry: `cmgrd` builds on its own docker daemon and runs what it builds,
which is the `local` build plane this document otherwise describes.

### Which orchestrator serves a schema

A schema names its destination, and the build plane resolves that name
against the mapping in `CORK_DESTINATIONS`:

```yaml
# the schema
name: spring-ctf
destination: event
flag_format: flag{%s}
```

```yaml
# CORK_DESTINATIONS, written by the ansible that stands the orchestrators up
event:   https://event.example.net:4200
library: https://library.example.net:4200
```

A name, not an address, so a typo is refused against the list instead of
dialled, and moving an orchestrator is a change to one file rather than to
every schema bound to it. A schema that names no destination means the only
one configured, and is refused once there is more than one: a single
orchestrator deployment need say nothing, and an event cannot land on the
year-round orchestrator because a line was forgotten.

`--server` overrides the mapping entirely and sends everything to the
address given, which is what a test or a one-orchestrator deployment can do.

**One content-addressed tag cannot be served by two orchestrators.** Two
schemas naming the same challenge at the same seed and flag format resolve
to the same tag no matter where they are bound, and whichever drops its
schema first would retire it from under the other. The build plane is the
only thing that sees both, so it refuses that pairing before building.

It can only refuse what it is shown, though: the check covers the schemas of
one `build`, and nothing is recorded of a schema's destination between runs
(the database keys builds by schema name and stores no destination at all).
So **build the schemas that share a challenge together** -- `cork-build build
library.yaml event.yaml ...` -- and the rule is enforced. Build them in
separate runs and neither run can see the other's binding.

### The commands, and what each one means

| | destination | content | images | on the orchestrator |
|---|---|---|---|---|
| `build` | as named | built and pushed | pushed | handed over, then **converged** |
| `migrate-schema` | **must differ** | unchanged | **untouched** | released, then handed over and converged |
| `remove-schema` | wherever it lives | gone | **retired** | builds and instances dropped |

Each of them finishes what it starts. A hand-over records builds; it does not
decide what runs them -- the instance counts travel with it, but only a
converge (`POST /schemas/<name>`, which is `update-schema`) acts on them,
starting what a schema asks for and stopping what it no longer does. So
`build` hands over **and** converges, on the destination it resolved: the
whole point of naming a destination rather than an address is that the
address is never typed, and a deploy that ended at the hand-over would have
required typing one for the second half.

`migrate-schema` moves a schema to the destination its file now names. It
releases the schema from the orchestrator serving it with `?retire=false` --
the content is changing hands, not ending, and the tag the new orchestrator
resolves is the one the old one was serving -- and then hands it over, where
every image is adopted from the registry. Nothing that is served is rebuilt,
because a build's identity does not depend on which orchestrator serves it.
(A challenge with a `builder` host is the one qualification: that image is
never pushed, so it is built again to extract the flag and artifacts from,
and then thrown away as always. What the workers pull is still adopted.) A
migration that takes minutes is a migration that went wrong.

It releases before it hands over, so the two never both hold the schema. A
failure in between leaves it served nowhere, which re-running fixes.

`remove-schema` is the destructive one, on both sides: the orchestrator drops
its builds and retires their tags, and the build plane drops its own rows for
them. It takes a **name**, not a schema file — nothing else about the schema
survives it, and which orchestrator has it is found by asking them rather
than read off a file. That matters beyond tidiness: a `DELETE` for a schema a
daemon has no rows for removes nothing and answers 204, so trusting a file
whose `destination` had been edited but not migrated would report a removal
that removed nothing while the event went on running. If nothing is serving
it, the build plane's own rows are dropped and it says so.

There is no `reset`. Clearing a plane is a loop over the names it knows:

```sh
cork-build list-schemas | xargs -rn1 cork-build remove-schema
```

which is visible, interruptible, and reports each removal — where one command
that nuked everything would be the most dangerous thing in the tree, standing
in for a single line. Reclaiming the registry is a separate job and not
cork's: nothing here sweeps tags that no row names (see "Limitations"), and
zot's own garbage collection is the channel for it.

Both halves are necessary. A build row that still carries a flag is one
the converge considers done, so a removal that left it behind would push
nothing the next time the schema was built -- and the hand-over would then be
refused for a tag the orchestrator had already retired.

### Which tool answers what

`cork-build` holds the challenge tree and is where every schema operation
happens, so it is where cmgr's build and schema commands live: the deploying
ones above, plus `update`, `list`, `search`, `info`, `list-schemas`,
`show-schema`, `system-dump`, `dockerfile` and `convert-to-custom`. Those
are questions about what a challenge *is* and what has been built, and the
tree and this plane's database are where the answers are.

`cmgrd-cli` is a thin HTTP client for one daemon and stays exactly that.
What is *running* — instances, workers, a live launch or stop — belongs to
an orchestrator and only it can answer. It keeps the build and schema
commands as well, and they are not redundant: a class deployment runs no
build plane at all, and on that box `cmgrd-cli update` and `cmgrd-cli
add-schema` are the operator's whole interface. Against an orchestrator on
an external build plane they are refused rather than missing — `update`, a
manual `build` and the `pin-*` pair with a 409, and `add-schema` with
"schema already exists", since the hand-over has already brought the schema
into being (`schemaExists` is a query over the builds table). That last one
is a 500 today rather than the 409 it reads like, because `schemaStatus`
maps only `ErrExternalBuildPlane` to a conflict.

Three of cmgr's commands went nowhere, and deliberately. `freeze` pre-built
a base layer and pushed it; base pins and the shared write-once registry
replaced it, and do the job better (see "Why pinning exists"). `test`,
`playtest` and `check` drove a solver framework that a production build
plane has no use for; it was removed with the `cmgr` binary itself. `start`,
`stop` and a manual `build`/`destroy` stay on the orchestrator side: a build
outside a schema has no destination, so there is nothing here to route it
to, and what runs is the orchestrator's business.

## cork-build, the build plane on its own

`cork-build` is this document's machine as a binary: cmgr's build path with
no orchestrator attached. It reads the schema files an event is defined by,
scans `CMGR_DIR`, builds and pushes every build those schemas name, and hands
the finished builds to one or more orchestrators over `PUT /challenges/<id>`.

```
cork-build build event.yaml practice.yaml          # to the destinations they name
cork-build --server https://orchestrator:4200 build event.yaml
cork-build pins
```

It is the same scan and the same converge cmgrd runs, so it reads the same
environment: `CMGR_DIR`, `CMGR_REGISTRY` (which must name the registry the
orchestrator's workers pull from), `CMGR_BASE_PINS`, `CMGR_PURGE_AFTER_PUSH`,
`CMGR_LOGGING` (`--verbose` is the flag form and wins), `CORK_DESTINATIONS`,
and docker's own variables.

The same library also reads the settings about *serving*, and every one of
them is inert here, because nothing runs on a build plane:
`CMGR_CONCURRENT_LAUNCHES`, `CMGR_PORTS`, `CMGR_INTERFACE`,
`CMGR_ENABLE_DISK_QUOTAS`, `CMGR_PRUNE_AGE` and the six `CMGR_WORKER_*`
tunables. Each is named in the log at startup if it is set — the mirror of
the notes an external daemon makes about `CMGR_DIR` and `DOCKER_HOST` — since
a unit file grown from an orchestrator's is how they arrive, and
`CMGR_CONCURRENT_LAUNCHES` in particular would otherwise report launch slots
on a host that will never take a launch.

What it does not do is run anything: every schema is converged here with its
builds on demand, which launches no instance, and the `instance_count` the
schema really asks for travels in the hand-over for the orchestrator to
converge to.

Its database is bookkeeping rather than a source of truth, but it is not
disposable while anything it built is in service. Keeping it between runs
saves rebuilding what has not changed. Losing it costs a re-derivation, in
which every image already in the registry is adopted rather than built again
(the registry is write-once, so a tag that is there is the content it names)
— but a re-derivation draws new build ids, and an orchestrator records each
build under the id this plane gave it, since that is the id its artifact
bundle is named by and the only id the platform has to address those files
with. So a hand-over of a re-derived build is refused, naming the id it is
already on record under.

Keep this database for as long as any schema it built is being served.

If it is lost anyway, recover the whole plane rather than one schema. A fresh
database numbers from 1, so the ids it draws are the low ones — which on an
orchestrator serving several schemas from this plane are held by whichever
schema was handed over first. Releasing only the schema being rebuilt frees
the wrong ids: the hand-over is refused naming the schema that holds them
(nothing is written, and no tag is touched), and the way out is releasing
that one too. So release every schema that orchestrator holds from this
plane, then build them together.

Release without retiring where you can — `DELETE /schemas/<name>?retire=false`,
the call `migrate-schema` makes — because the tags then survive and the
rebuild adopts them instead of building and pushing again. `remove-schema`
retires, which is correct for a removal and expensive here.

And clear that destination's artifact directory, or move it aside, before
building into it. Bundles are named by build id, so a plane numbering from 1
writes over the bundles of whatever already holds ids 1..N there.

Which is why it only ever pushes to the registry. Retiring a tag -- taking
out a generation nothing needs any more -- means knowing every row that still
names it, and this database knows only what this build plane built, while an
orchestrator may still be serving builds these schemas have stopped naming.
So dropping a seed here, or changing a flag format, reclaims nothing in the
registry: the orchestrator's own `update-schema` releases those builds and
untags them, against the database that does know what references them.

One thing a kept database does not carry over: a pin refresh. A build is
stamped with its identity when it is made, and refreshing the pins rebuilds
nothing (below), so builds made before a refresh still carry the identity
they were made under while the hand-over would state the fingerprint in
force now. `cork-build` compares the two before it sends anything and
refuses the run naming the builds that disagree; build them again from a
database that does not hold them.

Two things it and the orchestrator must agree on. The registry, since the
hand-over is refused for an image the orchestrator cannot find there. And the
version: the orchestrator recomputes every build's identity with its own copy
of the challenge templates, so a template that changed between the two makes
every identity disagree and every hand-over invalid. `cork-build` asks each
orchestrator its version before it sends anything and says so when they
differ.

The base image pins live here too, which is why `pins` is a `cork-build`
command: an orchestrator on an external build plane answers 409 to the pin
endpoints, having no tree to read the bases from and nothing to build with
them. Refresh before a build, not after -- a moved base reaches a challenge
the next time that challenge is built.

## What the build path does now

cork asks the Docker API for **BuildKit** explicitly (`version=2`) on every
build. It does not inherit the daemon's default builder, because that default is still the
deprecated classic builder and neither `buildx` being installed nor
`features.buildkit` in `daemon.json` changes it (both were tested; what the API
sends is what decides).

That switch is not about build speed. The classic builder keeps its layer cache
**as untagged images in the image store**, so cache lifetime and image lifetime
are the same thing: any image prune destroys the cache, and the next build
re-runs every `apt-get install` in the fleet. BuildKit keeps a separate cache
store, which is what makes "reclaim disk" and "keep the shared setup layers"
independent goals rather than opposing ones.

Three things then sit on top:

| Piece | What it is | Where |
|---|---|---|
| Base image pinning | rewrites `FROM name:tag` to `FROM name@sha256:…` **in the build context tar**, never on disk | `cmgr/basepins.go` |
| Pin fingerprint | the pin map's checksum is folded into a build's content identity | `contentChecksum`, `cmgr/docker.go` |
| Type template | the built-in Dockerfile of a `flag-only`, `remote-make` or `static-make` challenge is folded into the identity too, so a cork release that changes one changes the identity of future builds (never a rebuild by itself; custom challenges carry their Dockerfile in their source) | `templateChecksum`, `cmgr/docker.go` |
| Inline cache | pushed images carry BuildKit cache metadata; a rebuild imports from the generation it displaces | `executeBuild` and `cacheRefsFor`, `cmgr/docker.go` |
| Purge after push | the builder's local copy of a build's images is dropped once they are in the registry | `cmgr/purge.go` |
| Write-once publish | a build is pushed only once it has validated, and a tag already in the registry is never pushed over | `publishImages` and `registryTagExists`, `cmgr/docker.go`, `cmgr/registry.go` |

## Where artifact bundles go

A build's `artifacts.tar.gz` is extracted on the plane that built it and
stays there. It is written to `CMGR_ARTIFACT_DIR`, in a subdirectory named
for the schema's `destination:` — exactly that name, with nothing prepended,
so the directory an operator sees is the destination they wrote in
`CORK_DESTINATIONS`. A schema with no destination (a single-host deployment,
or a `--server` run) keeps the artifact directory itself.

That layout is what lets one build plane serve several orchestrators: an
artifact server watches one directory per destination and publishes each
under that destination's own prefix, so a challenge's files land where its
own event's players look for them, and retiring an event is deleting one
prefix. Nothing about this reaches an orchestrator — a hand-over carries no
bytes, an orchestrator ignores `CMGR_ARTIFACT_DIR`, and cork serves no
artifacts at all.

A bundle is removed when its build is destroyed, and it is *searched for*
rather than computed: `remove-schema` takes a name and never learns a
destination, and `migrate-schema` reads the destination a schema is moving
*to*, not the one its bundles were written under. Within one plane's database
a build id is drawn once, so at most one file answers to the name
(`removeArtifactBundle`, `cmgr/filesystem.go`). A migration therefore moves a
bundle into the new destination's directory and takes the old one out.

That uniqueness is a property of one database, not of the directory. A plane
rebuilt from scratch numbers from 1 again while every destination's directory
still holds bundles 1..N, so a name is no longer evidence of whose file it is.
That is why the stray prune touches only the artifact directory itself and why
a relocation never moves a file out of another destination's directory — and
why building on a re-derived plane into an artifact directory that still holds
another schema's live bundles would overwrite them. Recover the whole plane at
once, or clear that destination's directory first.

The publish order is registry, then artifact archive, then build row: each
store is written only once the one before it holds the generation, so nothing
ever names a generation the workers cannot pull; a build that fails
validation leaves no tag behind, and one that fails after pushing takes the
tags it pushed back out. A tag names its content, so one that is already
there is left alone rather than overwritten — and adopted: every image is
resolved against the registry before anything is built, an identity the
registry already serves is not built here at all (the one the build extracts
from is pulled), and the row (flag, lookups, artifacts) is derived from the
registry's image, so what the row says and what the workers run cannot be two
different images of one identity. The one residual is a challenge with a
`builder` stage: that stage is never in the registry, so its extraction is
always from a fresh local build while the challenge image is the registry's.

The existence check goes through the local daemon (the same certs.d material
and credentials it pushes and pulls with), so a registry dockerd can push to
is one cork can ask; a registry that cannot be asked fails the build rather
than pushing on a guess. Only tag deletes need cmgrd's own client
certificate, as before.

The repair for a tag that is wrong (a build input the checksum does not
cover) is to remove it and build again — and "again" needs a trigger, since
the identity is unchanged and nothing is stale: `--prune-old` on the next
rebuild of the challenge, or `remove-schema` and `add-schema`, whose fresh
rows build and push what the registry then lacks.

## When the registry is down

**Re-run the command. There is nothing else to do.**

That is worth stating plainly because the instinct after an outage is to go
looking for drift to reconcile, and there is none to find. The reason is
structural: cork can leave a tag that no row names, but never a row naming a
tag that is not there. A row is written only once the push has succeeded, and
every hand-over re-asks the registry before recording anything. The
divergence only ever runs one way, towards garbage.

So what each failure leaves behind:

| | while the registry is down | left behind |
|---|---|---|
| resolving identities, and the check before a push | the build fails before anything is built | nothing |
| the push itself | local images removed; the tags it had pushed are deleted, and those deletes fail too | tags no row names |
| retiring a tag on destroy or prune | best-effort, warned about, the operation still succeeds | tags no row names |
| a hand-over's registry check | refused, 500, nothing recorded | nothing |
| a worker's pull before a launch | 503 with Retry-After | nothing |

Every operation is idempotent, because identity is content-addressed and the
registry is write-once: a build that failed recorded nothing, a hand-over
that was refused recorded nothing, and a removal that leaked tags had already
dropped its rows. Reads are retried a few times over about a second
(`registryAttempts`) so a dropped connection is not an outage; a registry
that is really down fails the operation while you are still watching, and
says so with what to re-run.

The one thing to know about the leaked tags: **they are adopted, not
rebuilt.** Build the same identity again and cork resolves it against the
registry, finds the tag, and pulls and re-extracts rather than building —
which is why a post-outage rebuild is fast. That is safe by construction:
`validateBuild` runs *before* `publishImages`, so nothing reaches the
registry without having validated, and the only failure after a push is the
artifact promotion, which adoption redoes anyway.

What the leak actually costs is registry disk. Nothing here sweeps tags no
row names (see "Limitations"), so a long outage with a lot of churn is a
reason to run zot's garbage collection afterwards — hygiene, not repair.

## Why pinning exists

BuildKit re-resolves a mutable tag against the registry on essentially every
build, where the classic builder just used whatever copy was already local. On a
fleet where `ubuntu:24.04` is the base of roughly two thirds of the challenges,
that is a Docker Hub round trip per challenge per pass — rate-limit exposure,
and a silent dependency on whatever Hub is serving that minute.

A digest needs no resolution. Rewriting the reference on its way into the build
context gets that without touching several hundred challenge `Dockerfile`s, and
without giving up the readable tag in the source.

The important consequence, and the one that surprises people:

> **The fleet was never actually floating.** The classic builder made zero tag
> lookups when the base was already local, so each builder was frozen on
> whatever copy of `ubuntu:24.04` it happened to hold. Pinning does not take
> away a freshness property that existed; it replaces an accidental freeze with
> a deliberate one that has a knob.

That knob is `pin-refresh`.

## Moving a base

```
cmgrd-cli pin-list        # what is pinned, and how many challenges use each
cmgrd-cli pin-refresh     # re-resolve every base to what the registry serves now
```

`pin-refresh` is the **only** moment cork consults a mutable tag. It reads
manifests (`DistributionInspect`), it does not pull layers: over the real corpus
that is 32 lookups in about 47 s and zero bytes of image data. The resolves are
serial, and the pass has an overall budget derived from
`CMGR_WORKER_CONTROL_TIMEOUT`, so a registry that accepts connections and then
stalls cannot hold a `POST /pins` open indefinitely. A reference that fails to
resolve **keeps the digest it already had** — a refresh never un-pins a base
because the network hiccuped — and the run reports the failure so it can be
retried.

**A refresh rebuilds nothing, and no later `update` rebuilds anything on its
account either.** Change detection compares the challenge directory's own
checksums, which a refresh does not touch, so every challenge comes back
`Unmodified`. A moved base reaches a challenge the next time that challenge is
rebuilt for its own reasons.

So the working order is **refresh, then run the batch update**: the challenges
in that batch land on the new base, and every challenge the batch did not touch
stays on the base it was last built with. Rolling the *entire* fleet onto a new
base means rebuilding the entire fleet, which cork has no single command for
(see Limitations).

The fingerprint still earns its place: it makes a build against a moved base a
different content identity from a build against the old one, so the two can
never collide on a tag, the rollback generation stays meaningful, and a stale
image is never reused for content it no longer matches.

## What gets pinned

`pin-refresh` scans the challenge directory for a `Dockerfile` **sitting beside
a `problem.md`** — that is exactly the set cork builds. A challenge's `app/` or
`bot/` subdirectory, and any directory of hand-maintained base images, are
deliberately skipped; pinning their bases would put references into the
fingerprint that no build consumes, and moving one of those would then re-stamp
the identity of the whole fleet for nothing.

Within a Dockerfile, these `FROM` references are left alone:

- an earlier stage in the same file (`FROM builder AS final`)
- a reference that already carries a digest (`@sha256:…`)
- anything containing `$` (`FROM ubuntu:${UBUNTU_TAG}`), since a build arg is
  not resolvable at scan time. There are none in the corpus today; the rule
  exists so that adding one is not a silent mis-pin
- `scratch`, which is not an image and cannot be resolved
- every `FROM` in a Dockerfile that contains `<<` **anywhere**. Not just the
  heredoc body: the whole file is refused and left unpinned, because a heredoc
  makes the lines inside it script rather than Dockerfile syntax (`from redis
  import Redis` matches the `FROM` pattern) and two attempts at telling them
  apart with a regexp were each wrong. The rule is blunter than the syntax --
  `RUN echo 'cout<<flag'` is refused too -- and cork says which challenge
- a `FROM` split across a line continuation, which has no single line to
  rewrite

`--platform=` and similar flags are preserved, as is the `AS` clause.

Run against the real corpus, that comes to **32 distinct external bases over 397
challenge Dockerfiles**, resolved in 47 s with zero image pulls. The
distribution is very long-tailed — `ubuntu:24.04` alone accounts for 326 of the
424 references, and 19 bases are used exactly once — which is worth knowing
because it is easy to reach for the wrong subset. The head of the distribution
is where *shared layers* live, but pinning is paid per build: a base used by one
challenge still costs a tag resolution on every build of it. Pin all of them.

## Do / don't

**Do**

- Run `pin-refresh` immediately before a batch update, not after.
- **Re-run `update` after one that failed.** A build whose rebuild failed keeps
  serving the generation it had, while the challenge row already carries the
  new source; each build records the source generation it was produced from
  (`builds.sourcechecksum`), so the difference is reported as `Stale` by every
  `update` and `update --dry-run` until a rebuild succeeds, and `update`
  rebuilds exactly those builds. Nothing has to be edited to make it happen.
- **Put the whole challenge fleet into maintenance for the rebuild.** cork
  serializes updates and schema operations against each other (one at a time,
  the rest wait), but nothing stops the platform from
  requesting launches of a build while that build is being replaced — and a
  launch in flight can collide with the update's own teardown of the instances
  it is displacing (`removal of container ... is already in progress`), which
  fails the update. There is no gate for this inside cork; it is the platform's
  to close, by not asking for launches during the pass.
- **Commit the pin file to the challenge repository**, at the corpus root, and
  point `CMGR_BASE_PINS` at it. cmgrd's own default is
  `<CMGR_DIR>/.base-pins.json` — a dotfile, which nobody commits and a
  `git clean -xdf` erases. A committed `base-pins.json` is versioned with the
  corpus it pins, restored by a fresh clone, and reviewable as a diff: the pin
  bump becomes a commit rather than an untracked file on one machine. It sits
  outside every challenge directory, so it perturbs no source checksum, and
  cork's scan ignores it. `pin-refresh` rewrites it in place; commit the result.
- Keep `storage-driver: overlay2`. The containerd image store breaks the cache
  path: a *pulled* image is not usable as a cache source there (locally built
  ones are), which quietly removes the shared apt/pip layers.
- Let BuildKit's own GC bound the cache (`builder.gc` in `daemon.json`). The
  image store is a separate budget with a separate mechanism.
- Size `builder.gc` so its `minFreeSpace` is a figure the disk can actually
  reach. See "Where the base images live" — that setting is the only thing that
  evicts base layers, so one that can never be satisfied sends every build back
  to Docker Hub.
- Measure the builder's disk with `du -sh /var/lib/docker/*`, not
  `docker system df`. See below: both `system df` and `buildx du` under-report,
  badly.

**Don't**

- Don't run `pin-refresh` while builds are in flight. A build stamps its content
  checksum and pins its build context in two separate reads of the pin map; a
  refresh landing between them produces an image tagged with the old identity
  and built on the new base.
- Don't rebuild under live traffic. See the maintenance point above; this is the
  one operational hazard in the update path that cork cannot close for you.
- Don't hand-edit the pin file to anything but a `sha256:` value. `cmgrd`
  refuses to start on a malformed pin file, deliberately: silently falling back
  to mutable tags is the exact failure pinning exists to prevent.
- Don't delete the pin file expecting a no-op. An empty map fingerprints as `0`,
  which restores the pre-pinning content identity of every build — the next
  rebuild of each challenge produces a differently tagged image.
- Don't run docker-reaper's `images` sweep on the builder. It is asynchronous
  and can evict an image between cork's build and its push. Its container and
  network sweeps match nothing on this host anyway (the extraction container
  carries no `cmgr.dynamic` label).
- Don't enable the OCI runtime shim on the builder. It is a worker-side control
  and only makes itself the default runtime here.

## Where the base images live

Measured on Docker 29.8 / overlay2, because it decides what a builder may safely
reclaim.

A pinned base **never becomes an image**. After a cold build of
`FROM debian@sha256:…`, `docker image inspect` on that reference says ABSENT,
`docker images -a` lists only the built image, and nothing is dangling. Its
layers are in `overlay2`, held by BuildKit's cache rather than by any image.

That has three consequences.

**Purging the images cork builds is free.** Removing every
`cmgr.managed=true` image left `overlay2` unchanged at 88 MB, and the next build
downloaded nothing and did not even *resolve* against the registry. Only
`docker builder prune -af` dropped it back to 4.5 MB, after which the same build
re-fetched four layers. So a post-push sweep of cork's images does not cost the
shared work, and there is no set of "keep these" images to curate — which is
what a build-push-purge loop on the builder needs to be safe.

**`builder.gc` is therefore the only control over base residency.** An
over-aggressive policy is not merely a cold cache; it is every build pulling
from Docker Hub again. A `minFreeSpace` larger than the filesystem can ever
offer prunes the whole cache on every pass, silently, and looks like the cache
simply not working.

**Neither `docker system df` nor `docker buildx du` accounts for it.** With
84 MB of base and setup layers resident, `system df` reported `Images 0B` and
`Build Cache 133B`, and `buildx du` agreed at 133 B. Cache records backed by
overlay2 snapshots show as roughly zero in both. Capacity planning from those
numbers will understate the builder by orders of magnitude; use `du`.

One operational note for a builder that purges after every build: containerd can
wedge (`dial unix:///run/containerd/containerd.sock: timeout`), after which every
image removal fails while builds keep working. The failure mode is silent
accumulation, not an error, and `systemctl restart containerd` clears it.

## Purging after the push

`CMGR_PURGE_AFTER_PUSH` — on by default in registry mode, off with
`false`/`0`/`off`, and refused outright without a registry.

Once a build's images are in the registry, nothing reads the builder's copies.
Keeping them makes the image store grow with the whole fleet — challenges times
seeds times retained generations — on a volume shared with the challenge
checkout, the database and the artifact bundles. Purging after `finalizeBuild`
bounds it to roughly one build in flight instead.

The section above is what makes this safe rather than a trade: image removal
does not touch BuildKit's cache, so the shared `apt` and `pip` layers survive
and the next build downloads nothing. That is also why there is no "keep these"
list to maintain — a question this design would otherwise have to answer, and
the reason earlier attempts reached for a catalogue of hand-maintained base
images.

Nothing else needs the local copy:

- a build's images are read exactly twice, both inside `executeBuild`: by the
  artifact extraction container and by the push;
- launches go through `ensureImages`, which pulls from the registry for
  whichever daemon hosts the instance — **including the local one**, so an
  instance that falls back to local placement (no workers registered) still
  works;
- the `builder` host's image is never pushed and never launched
  (`startContainers` skips it); it exists only for extraction;
- the retention paths only ever *remove* images and already tolerate absence.

**Without a registry this is refused, not merely defaulted off.** Single-host
cmgr never pushes and `ensureImages` does not pull, so the builder's copy is the
only one there is; purging would break every launch.

Turning it off is how you measure. An unpurged pass over the real corpus is the
only way to learn what the image store actually costs at fleet scale, which no
number in this document currently rests on.

## Limitations

1. **No fleet-wide rebuild.** `update` rebuilds only what the challenge
   directory says changed, plus whatever an earlier rebuild failed to replace
   (see Do). There is no force-rebuild-everything path, so a base bump
   propagates challenge by challenge as each one is next touched.
2. **The fingerprint is coarse.** The whole pin map is hashed, so moving any one
   base changes the content identity of every build, not just the ones on that
   base. Per-challenge precision was deferred. It costs nothing while nothing
   rebuilds on the fingerprint alone (limitation 1), but the two have to be
   resolved together.
3. **The pin file is not in the database.** An orchestrator that comes back
   without it comes back unpinned, and unpinned is a different fingerprint.
   Committing it to the challenge repository (see Do) is what makes this a
   non-issue; a machine-local file is not.
4. **Inline cache is registry-only.** Without `CMGR_REGISTRY` there is nowhere
   to publish cache metadata, and `CacheFrom` entries would be read as registry
   references — a bare `challenge:tag` normalizes to `docker.io/library` and
   would send a Hub lookup per image per build. Both are switched off in that
   mode, leaving single-host cmgr as it was.

## Measured

24 challenges shaped like the real fleet, two seeds each, Docker 29.8, overlay2,
Hub traffic counted at a pull-through mirror. "Status quo" is `main`; "shipped"
is this branch.

| Phase | Status quo (classic) | Shipped (BuildKit + pins + inline) |
|---|---|---|
| Pin setup | — | 1.0 s / 0 B |
| Cold build | 232 s / 113 MB | 196 s / 113 MB |
| Incremental | 7 s | 15.5 s |
| After an image sweep | 235 s | 10.4 s |
| After all images removed | 209 s / 113 MB | 10.2 s |
| After the build cache is wiped | 214 s / 113 MB | 36–45 s / 9 KB |

Those rows were measured before purge-after-push existed, so the disk figures
they came with described a builder that kept every image it built; they are
omitted rather than restated, and no claim in this document rests on them (see
the Purging section). The timings are unaffected. The incremental case is
genuinely slower —
BuildKit's per-build overhead is real and shows up when there is nothing to do.
Every other row is the point: image lifetime and cache lifetime are no longer
the same thing, and a builder that lost its cache recovers from the registry in
seconds instead of re-running the fleet's apt installs.

That "incremental" row is a no-op pass over 24 challenges, where the overhead is
all there is to measure. A rebuild that has actual work is the opposite story:
in the e2e fleet, one challenge rebuilt after a source edit took **3 s against
the classic builder's 59 s**, because BuildKit reuses the unchanged prefix of
the Dockerfile where the classic builder re-ran it. Do not read the 15.5 s as
"BuildKit is slower"; read it as "BuildKit has a fixed cost per build that a
no-op cannot amortize".

The inline cache is verified rather than assumed: the image config cork pushes
to the registry carries a `moby.buildkit.cache.v0` key, which the e2e asserts by
fetching the config blob back out of zot.

**What the inline cache is and is not.** Tags are content-addressed, so the tag
a build is about to produce cannot exist yet whenever the source changed — which
is the only reason `update` rebuilds anything. Offering only that tag as a cache
source, as this first did, meant every rebuild asked the registry for something
that could not be there and imported nothing. `cacheRefsFor` therefore offers
the **generation this build displaces** first: it is already in the registry and
its layers are the ones the new build shares. The build's own tag is still
offered second, because that is the entry that hits when the same content is
built again — a builder recovering from a reclaimed cache, or a source revert,
which is exactly what the "wiped build cache" row above measures.

Note what this does *not* claim. Cross-challenge sharing — the 326 challenges on
`ubuntu:24.04` reusing each other's `apt` layer — is served by BuildKit's
**local** cache and by `builder.gc` keeping it, not by the registry. The inline
cache is what makes a builder that lost that local cache cheap to restore.

Both halves are tested, and separately. The publish side is the config-blob
assertion above. The import side prunes the builder's cache to zero, builds one
more generation, and requires the shared `apt` layer's digest to be unchanged
while the top layer moves — re-running `apt` yields a different digest, since
the layer tar carries the mtimes that run wrote, so with the cache empty an
identical digest can only have come from the registry. That is the assertion a
`CacheFrom` naming nothing importable fails, and it is the one that would have
caught this the first time.

Caveats: one run per arm, arms batched rather than interleaved, and the corpus
is synthetic. The real 445-Dockerfile repository has been exercised for the pin
scan and refresh only.
