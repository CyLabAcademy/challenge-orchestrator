package cmgr

import (
	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Purging the builder's local images after a build is pushed.
//
// The orchestrator builds every challenge image and pushes it to the registry;
// workers pull from there. Nothing then reads the builder's own copy, so
// keeping it means the image store grows with the whole fleet -- challenges
// times seeds times retained generations -- on a volume it shares with the
// challenge checkout, the database and the artifact bundles.
//
// What makes this safe is the move to BuildKit. Under the legacy builder the
// layer cache WAS untagged images, so reclaiming image space destroyed the
// shared apt and pip layers and the next build re-ran every install in the
// fleet. BuildKit keeps its cache in a separate store that image removal
// cannot reach, which was measured: after removing every cmgr.managed image,
// overlay2 was unchanged and the next build downloaded nothing. (That
// measurement predates the inline cache: a build now does resolve its cache
// refs against the registry. The conclusion it supports -- that removing images
// does not destroy the layer cache -- is unaffected.) Only
// `docker builder prune` drops the bases, which is builder.gc's business, not
// this file's.
//
// Nothing else needs the local copy either:
//
//   - a build's images are read exactly twice, both inside executeBuild: by the
//     artifact extraction container, and by the push;
//   - launches call ensureImages, which pulls from the registry for whichever
//     daemon hosts the instance -- including the local one, so an instance that
//     falls back to local placement (no workers registered) still works;
//   - the retention paths only ever REMOVE images and already tolerate their
//     absence (pruneReplacedImages and destroyImages both log a missing tag at
//     debug and continue).
//
// The "builder" host's image is purged along with the rest, and it is the one
// exception to "every image is in the registry": it is never pushed and never
// launched (startContainers skips it), existing only for the extraction
// container, which has already run by the time this is called. So dropping it
// is free today -- but it cannot be recovered by a pull, only by a rebuild.
// The rollback sketched at the extraction container in docker.go would re-run
// extraction against the PrevChecksum image without building, and would have
// to reckon with that.
//
// Registry mode only, and that is a hard gate rather than a default: without a
// registry the images are never pushed, ensureImages does not pull, and the
// builder's copy is the only one there is. Purging there would break every
// launch.
const PURGE_AFTER_PUSH_ENV string = "CORK_PURGE_AFTER_PUSH"

// initPurgeAfterPush reads CORK_PURGE_AFTER_PUSH. On unless explicitly
// disabled, matching CORK_DB_WAL's spelling of the same idea; ignored outright
// when no registry is configured.
func (m *Manager) initPurgeAfterPush() {
	m.purgeAfterPush = true
	if v, ok := LookupEnv(PURGE_AFTER_PUSH_ENV); ok && (v == "false" || v == "0" || v == "off") {
		m.purgeAfterPush = false
	}

	switch {
	case m.challengeRegistry == "":
		// Not a warning: this is the single-host shape, where there is nowhere
		// to push and the local images are what instances run.
		m.purgeAfterPush = false
		m.log.debug("not purging built images: no registry configured, so the builder's copies are the only ones")
	case m.purgeAfterPush:
		m.log.info("purging built images from the builder after they are pushed")
	default:
		m.log.warnf("%s is off: images accumulate on the builder and nothing reclaims them", PURGE_AFTER_PUSH_ENV)
	}
}

// purgeBuiltImages drops the builder's local copies of a finalized build's
// images. Call it after finalizeBuild, never before: the build row must be
// committed first, so that a crash in between leaves a valid build whose
// images are merely absent from the builder and recoverable by a pull.
//
// Never returns an error and never fails a build. The build already succeeded
// and its images are already in the registry; a copy that could not be dropped
// is wasted disk, which is the problem this exists to reduce, not a reason to
// fail work that is done.
//
// Deliberately not guarded by contentReferenced, unlike the untagging paths
// that share imageMu. Those decide whether content may be destroyed, and a
// wrong answer loses it. Here every image is in the registry, so the worst a
// removal another build wanted can cost is one pull.
func (m *Manager) purgeBuiltImages(bMeta *BuildMetadata) {
	if !m.purgeAfterPush || m.challengeRegistry == "" {
		return
	}

	// Shared with the untagging paths so a removal cannot land between another
	// remover's reference check and its own ImageRemove.
	m.imageMu.Lock()
	defer m.imageMu.Unlock()

	// Force stays false: an image a container still holds is skipped rather
	// than torn out from under it. That should not happen on a builder, and if
	// it does the image is worth keeping.
	iro := client.ImageRemoveOptions{Force: false, PruneChildren: true}
	var kept int
	for _, image := range bMeta.Images {
		imageName := m.instanceImageName(bMeta.Challenge, bMeta, image)
		// Bounded, unlike the other removers on this path. Without it a wedged
		// containerd -- which fails removals while builds keep succeeding --
		// hangs on the daemon's own dial timeout while imageMu is held.
		// pruneReplacedImages and destroyImages still pass m.ctx and would
		// hang the same way; bounding this one stops the BUILD path from
		// being the thing that wedges, which is the path that runs per build.
		ctx, cancel := m.controlCtx()
		_, err := m.cli.ImageRemove(ctx, imageName, iro)
		cancel()
		if err != nil {
			switch {
			case errdefs.IsNotFound(err):
				m.log.debugf("built image already gone: %s", imageName)
			case errdefs.IsConflict(err):
				// Still referenced -- another build row shares this
				// content-addressed tag, or a container holds it.
				kept++
				m.log.debugf("keeping built image, still referenced: %s", imageName)
			default:
				kept++
				m.log.warnf("could not purge built image %s: %s", imageName, err)
			}
			continue
		}
		m.log.debugf("purged built image %s", imageName)
	}

	// At INFO, because the alternative is a builder that fills up silently.
	// cmgrd runs at INFO, so the per-image lines above are invisible there;
	// without this the operator sees only the startup line promising that
	// purging happens. Conflicts are the case that actually bites: with
	// instances placed on the builder's own daemon every image with a live
	// instance returns 409, and nothing reclaims anything.
	if kept > 0 {
		m.log.infof("kept %d of %d built images for %s (still referenced or not removable); they are not reclaimed by anything else",
			kept, len(bMeta.Images), bMeta.Challenge)
	}
}
