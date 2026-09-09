# The builder

Notes for whoever operates or changes cork's image build path. Everything here
concerns the **orchestrator** host: the one machine that runs `cmgrd`, builds
images and pushes them to the registry. Workers only pull.

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
| Inline cache | pushed images carry BuildKit cache metadata; a rebuild imports from the generation it displaces | `executeBuild` and `cacheRefsFor`, `cmgr/docker.go` |
| Purge after push | the builder's local copy of a build's images is dropped once they are in the registry | `cmgr/purge.go` |

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
  serializes updates against each other, but nothing stops the platform from
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
