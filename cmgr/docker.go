package cmgr

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/go-units"
	"github.com/jmoiron/sqlx"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/strslice"
	"github.com/moby/moby/client"
)

//go:embed seccomp.json
var seccompPolicy string

func (m *Manager) initDocker() error {
	// Deliberately not client.FromEnv: DOCKER_CERT_PATH holds the worker mTLS
	// material (see workers.go) and must not switch this local unix-socket
	// client into HTTPS mode.
	cli, err := client.NewClientWithOpts(
		client.WithHostFromEnv(),
		client.WithVersionFromEnv(),
	)
	if err != nil {
		m.log.errorf("could not create docker client: %s", err)
		return err
	}

	m.cli = cli
	m.ctx = context.Background()

	var ping client.PingResult
	for attempt := 1; attempt <= 3; attempt++ {
		ping, err = cli.Ping(m.ctx, client.PingOptions{})
		if err == nil {
			break
		}
		if attempt < 3 {
			m.log.warnf("failed to ping docker engine (attempt %d/3): %s. retrying...", attempt, err)
			time.Sleep(1 * time.Second)
		}
	}
	if err != nil {
		m.log.errorf("could not connect to docker engine: %s", err)
		return err
	}

	m.log.infof("connected to docker (API v%s)", ping.APIVersion)

	// OSType is immutable for the daemon's lifetime and is only used to decide
	// whether to apply the linux seccomp profile. Fetch it once here so the hot
	// launch and solve paths never have to call the (heavy) Info endpoint.
	info, err := cli.Info(m.ctx, client.InfoOptions{})
	if err != nil {
		m.log.errorf("could not query docker engine info: %s", err)
		return err
	}
	m.hostOSType = info.Info.OSType

	// Per-daemon slots (see launch.go). dockerd serializes an instance's
	// network creation and container starts internally on either firewall
	// backend, so two slots measured best on iptables and nftables showed
	// no gain past two either; the range is open for re-measuring.
	m.launchConcurrency = m.envSlots("CMGR_CONCURRENT_LAUNCHES", 2)
	m.launchSemaphore = make(chan struct{}, m.launchConcurrency)
	m.log.infof("launch slots per daemon: %d", m.launchConcurrency)

	chalInterface, isSet := os.LookupEnv(IFACE_ENV)
	if !isSet {
		chalInterface = "0.0.0.0"
	}
	m.challengeInterface = chalInterface

	m.challengeRegistry, isSet = os.LookupEnv(REGISTRY_ENV)
	if isSet {
		authPayload := fmt.Sprintf(
			`{"username":"%s","password":"%s","serveraddress":"%s"}`,
			os.Getenv(REGISTRY_USER_ENV),
			os.Getenv(REGISTRY_TOKEN_ENV),
			strings.SplitN(m.challengeRegistry, "/", 2)[0],
		)
		m.authString = base64.StdEncoding.EncodeToString([]byte(authPayload))
	}

	m.portLow, m.portHigh, err = getPortRange()
	if err != nil {
		m.log.errorf("%s", err)
	}

	return err
}

func getPortRange() (int, int, error) {
	portRange := os.Getenv(PORTS_ENV)
	if portRange == "" {
		return 0, 0, nil
	}

	portStrs := strings.Split(portRange, "-")
	if len(portStrs) != 2 {
		return 0, 0, fmt.Errorf("malformed port range: '%s' does not contain '-' character", portRange)
	}

	var low int
	var high int
	var err error
	low, err = strconv.Atoi(portStrs[0])
	if err == nil {
		high, err = strconv.Atoi(portStrs[1])
	}

	if err != nil {
		return 0, 0, err
	}

	if low < 1024 || high > (1<<16) || high < low {
		err = fmt.Errorf("bad port range: %d-%d either contains invalid/privileged ports or includes 0 ports", low, high)
	}

	return low, high, err
}

func (b *BuildMetadata) makeFlag() *string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", b.Challenge, b.Format, b.Seed)))
	sumStr := fmt.Sprintf("%x", sum)

	flag := new(string)
	if len(sumStr) > 8 {
		sumStr = sumStr[:8]
	}
	*flag = fmt.Sprintf(b.Format, sumStr)
	return flag
}

func (b *BuildMetadata) getArtifactsFilename() string {
	return fmt.Sprintf("%d.tar.gz", b.Id)
}

func (i *InstanceMetadata) getNetworkName() string {
	return fmt.Sprintf("cmgr-%d", i.Id)
}

func (m *Manager) generateBuilds(builds []*BuildMetadata) error {
	if len(builds) == 0 {
		return nil
	}

	buildsComplete := true
	for _, build := range builds {
		buildsComplete = buildsComplete && (build.Flag != "")
	}

	cMeta, err := m.lookupChallengeMetadata(builds[0].Challenge)
	if err != nil {
		return err
	}

	// Note: DetectChanges re-parses and re-hashes the challenge directory on
	// every converge, even when all builds are already complete (below). That
	// cost is inherent to detecting drift here; it is paid per challenge on each
	// CreateSchema/UpdateSchema (e.g. cmgrd instance-count resizes). If it
	// becomes a bottleneck for large schemas, compare only this challenge's
	// stored SourceChecksum instead of running a full cross-schema DetectChanges.
	updates := m.DetectChanges(filepath.Dir(cMeta.Path))

	inList := func(list []*ChallengeMetadata) bool {
		for _, md := range list {
			if md.Id == cMeta.Id {
				return true
			}
		}
		return false
	}
	// The buckets are distinct: only Updated implies changed image content (and
	// hence stale images); Refreshed is a metadata/solve-script change with the
	// source checksum unchanged, so its images are current. modified drives the
	// pending-build error path below (anything not Unmodified is drift).
	sourceChanged := inList(updates.Updated)
	metadataChanged := inList(updates.Refreshed)
	removed := inList(updates.Removed)
	modified := sourceChanged || metadataChanged || removed

	if buildsComplete {
		// Nothing to build, but surface drift instead of returning silently:
		// converging a schema neither rebuilds nor refreshes existing builds —
		// only 'update' does — so name whatever 'update' would act on, without
		// overstating staleness for a metadata-only change.
		switch {
		case len(updates.Errors) > 0:
			m.log.warnf("errors detected in directory for '%s'; run 'update': %v", cMeta.Id, updates.Errors)
		case sourceChanged:
			m.log.warnf("source for '%s' has changed since last update; existing builds and images are stale until 'update' is run", cMeta.Id)
		case metadataChanged:
			m.log.warnf("metadata for '%s' has changed since last update; run 'update' to refresh it (images are unaffected)", cMeta.Id)
		case removed:
			m.log.warnf("source for '%s' can no longer be found; its existing builds are stale", cMeta.Id)
		}
		return nil
	}

	if len(updates.Errors) > 0 {
		err = fmt.Errorf("errors detected in directory for '%s' run 'update'", cMeta.Id)
		m.log.error(err)
		return err
	}

	if modified {
		err = fmt.Errorf("'%s' has changed since last update", cMeta.Id)
		m.log.error(err)
		return err
	}

	buildCtxFile, err := m.createBuildContext(cMeta, m.GetDockerfile(cMeta.ChallengeType))
	if err != nil {
		m.log.errorf("failed to create build context: %s", err)
		return err
	}
	defer os.Remove(buildCtxFile)

	for _, build := range builds {
		if build.Flag != "" {
			continue
		}

		err = m.openBuild(build)
		if err != nil {
			return err
		}

		err = m.executeBuild(cMeta, build, buildCtxFile)
		if err != nil {
			m.removeBuildMetadata(build.Id)
			return err
		}

		err = m.finalizeBuild(build)
		if err != nil {
			return err
		}
	}

	return nil
}

type dockerError struct {
	Error string `json:"error"`
}

// The failure line in a Docker JSON message stream carries both an "error"
// string and an "errorDetail" object; older daemons emitted "errorDetail"
// first while newer ones lead with "error", so match either prefix.
var dockerStreamErrRe = regexp.MustCompile(`{"error[^\n]+`)

// dockerStreamError scans a Docker JSON message stream (from build, push, or
// pull responses, which report failures in-stream rather than as API errors)
// and returns the reported failure, or nil if the stream reports none.
func dockerStreamError(messages []byte) error {
	errMsg := dockerStreamErrRe.Find(messages)
	if errMsg == nil {
		return nil
	}
	var dMsg dockerError
	if json.Unmarshal(errMsg, &dMsg) == nil && dMsg.Error != "" {
		return errors.New(dMsg.Error)
	}
	return errors.New(string(errMsg))
}

// contentChecksum derives the content identity of a build's images from the
// challenge source checksum and the flag format. The source content reaches
// the image through the build context (and the frozen base image); the flag
// format enters as a build arg. The seed also affects image content but is
// carried explicitly in the docker tag, so it is not folded in here.
//
// Caveat: the embedded per-type Dockerfile (m.GetDockerfile) is part of the
// build context but NOT part of this checksum. So a rebuild whose only change
// is the challenge type, or a cmgr release shipping a different built-in
// Dockerfile (e.g. a base-image bump), yields different image content under an
// unchanged tag. Within one database this is just the tag reuse the scheme
// already tolerates; across databases sharing a daemon it means identical
// tuples can resolve to differing content. Folding the type Dockerfile (or a
// build-machinery version) in here would close that, but the migration
// backfill cannot reconstruct a legacy row's historical Dockerfile, so it is
// deferred.
func contentChecksum(sourceChecksum uint32, format string) uint32 {
	h := crc32.NewIEEE()
	var src [4]byte
	binary.BigEndian.PutUint32(src[:], sourceChecksum)
	h.Write(src[:])
	h.Write([]byte(format))
	sum := h.Sum32()
	// 0 is reserved as the "unset / not-yet-migrated" sentinel: builds.checksum
	// and prevchecksum default to 0, the migration backfill keys on checksum=0,
	// and prevchecksum=0 means "no rollback generation". CRC-32 can legitimately
	// be 0 (e.g. sourceChecksum 0x05f16712 with format "flag{%s}"), which would
	// make a real generation indistinguishable from "none", so map that single
	// value to 1. This adds only the same negligible collision the CRC already
	// permits.
	if sum == 0 {
		sum = 1
	}
	return sum
}

// dockerId is the docker tag for one of the build's images. It is derived
// from portable build identity — seed plus content checksum — rather than the
// local autoincrement build id, so the same challenge content yields the same
// tag no matter which cmgr database built it, and a source change yields a
// new tag instead of mutating the old one. The "s" prefix keeps the tag valid
// when the seed is negative.
func (bMeta *BuildMetadata) dockerId(image Image) string {
	return fmt.Sprintf("s%d-%x-%s", bMeta.Seed, bMeta.Checksum, image.Host)
}

// migrateBuildChecksums backfills builds.checksum for rows still at the default
// value (0) and retags their local docker images from the legacy {buildid}-{host}
// tag form to the content-addressed form (see dockerId). The backfill derives
// each build's checksum from its challenge's current source checksum, which is
// what the images were built from as of the last update — the same assumption
// the pre-checksum code made when it reused a tag across rebuilds.
//
// It is resumable and idempotent: the work is keyed on checksum=0 (the
// "not yet migrated" marker) rather than on the column's existence, and each
// row's images are retagged BEFORE its checksum is stamped, so an interruption
// or transient docker error leaves the row at 0 to be retried on the next
// start. Called from initDatabase before m.db is assigned, so the handle is
// passed in explicitly; m.cli is nil when the database is initialized without
// docker (tests), in which case retagging is skipped.
func (m *Manager) migrateBuildChecksums(db *sqlx.DB) error {
	rows := []struct {
		Id             BuildId
		Seed           int
		Format         string
		Challenge      string
		SourceChecksum uint32 `db:"sourcechecksum"`
	}{}
	err := db.Select(&rows, `SELECT b.id, b.seed, b.format, b.challenge, c.sourcechecksum
		FROM builds AS b JOIN challenges AS c ON b.challenge = c.id
		WHERE b.checksum = 0;`)
	if err != nil {
		return err
	}

	for _, row := range rows {
		checksum := contentChecksum(row.SourceChecksum, row.Format)

		// Retag first: checksum=0 is the resume marker, so it is stamped only
		// once the images provably carry the new tag (or are shown to need no
		// retag). A docker or registry error skips just this row — it stays
		// at 0 and is retried on the next start — rather than aborting the
		// migration: a registry hiccup here must not stop cmgrd from booting.
		if err := m.retagLegacyImages(db, row.Id, row.Challenge, row.Seed, checksum); err != nil {
			m.log.errorf("could not migrate images for build %d (will retry next start): %s", row.Id, err)
			continue
		}

		if _, err := db.Exec("UPDATE builds SET checksum = ? WHERE id = ?;", checksum, row.Id); err != nil {
			return err
		}
	}

	// Rows still at checksum=0 — a retag/push failure above, or a build that
	// never joined a challenge (e.g. out-of-band DB edits with foreign keys
	// off) — would yield an invalid s{seed}-0 tag; surface them rather than
	// failing silently at launch time.
	var unmigrated int
	if err := db.Get(&unmigrated, "SELECT COUNT(1) FROM builds WHERE checksum = 0;"); err != nil {
		return err
	}
	if unmigrated > 0 {
		m.log.warnf("%d build(s) have no content checksum; their images cannot be resolved until migration succeeds on a later start or the challenge is rebuilt", unmigrated)
	}

	return nil
}

// retagLegacyImages moves a build's images from the legacy {challenge}:{id}-{host}
// tag form to the content-addressed {challenge}:s{seed}-{checksum}-{host} form.
// It is idempotent and tolerant of a missing source image (already retagged on
// a prior interrupted run, or a fresh daemon that will rebuild on demand): only
// a genuine daemon error is returned, so migrateBuildChecksums can defer
// stamping the checksum until the retag has succeeded or been shown
// unnecessary. Skipped without a docker client (tests).
func (m *Manager) retagLegacyImages(db *sqlx.DB, id BuildId, challenge string, seed int, checksum uint32) error {
	if m.cli == nil {
		return nil
	}

	hosts := []string{}
	if err := db.Select(&hosts, "SELECT host FROM images WHERE build = ?;", id); err != nil {
		return err
	}

	newMeta := BuildMetadata{Seed: seed, Checksum: checksum}
	for _, host := range hosts {
		oldRef := fmt.Sprintf("%s:%d-%s", challenge, id, host)
		newRef := fmt.Sprintf("%s:%s", challenge, newMeta.dockerId(Image{Host: host}))
		if m.challengeRegistry != "" {
			oldRef = fmt.Sprintf("%s/%s", m.challengeRegistry, oldRef)
			newRef = fmt.Sprintf("%s/%s", m.challengeRegistry, newRef)
		}
		// Registry mode: workers resolve instance images from the registry by
		// tag, so after a local retag the new-form tag must also be pushed or
		// instance starts would pull a tag zot has never seen. Builder images
		// stay local-only, matching executeBuild.
		needsPush := m.challengeRegistry != "" && host != "builder"

		if _, err := m.cli.ImageTag(m.ctx, client.ImageTagOptions{Source: oldRef, Target: newRef}); err != nil {
			if !errdefs.IsNotFound(err) {
				// A real daemon error, not a missing image: abort so the row
				// stays at checksum=0 and the retag is retried next start.
				return fmt.Errorf("could not retag legacy image %s as %s: %w", oldRef, newRef, err)
			}
			// Source absent: either already retagged (new ref present) or never
			// present locally (fresh daemon / pruned out-of-band). If a prior
			// interrupted run already produced the new tag, fall through so the
			// registry push below still happens; otherwise nothing to recover —
			// the next rebuild recreates the image under the new name.
			newPresent, _ := m.imagePresent(m.cli, "", newRef)
			if !needsPush || !newPresent {
				m.log.debugf("legacy image %s not present; skipping retag", oldRef)
				continue
			}
		} else {
			// Drop the legacy ref; the image survives under the new one. A missing
			// legacy tag here is fine (idempotent re-run after a prior removal).
			if _, err := m.cli.ImageRemove(m.ctx, oldRef, client.ImageRemoveOptions{Force: false, PruneChildren: false}); err != nil && !errdefs.IsNotFound(err) {
				m.log.warnf("could not remove legacy image tag %s: %s", oldRef, err)
			}
			m.log.infof("retagged image %s as %s", oldRef, newRef)
		}

		if needsPush {
			// Layers already live in the registry from the original push;
			// this uploads little more than the manifest under the new tag.
			// Abort on failure so the row stays at checksum=0 and the push is
			// retried next start (ImageTag above is idempotent).
			if err := m.pushImage(newRef); err != nil {
				return fmt.Errorf("could not push retagged image %s: %w", newRef, err)
			}
		}
	}
	return nil
}

func challengeToFreezeName(challenge ChallengeId) string {
	return strings.ReplaceAll(string(challenge), "/", "_")
}

// instanceImageName returns the docker tag for a build's per-host image. When
// CMGR_REGISTRY is set the name is registry-qualified so that images can be
// pushed after building and pulled before launching; cmgr and cmgrd must be
// configured with the same registry value for the derived names to agree.
func (m *Manager) instanceImageName(challenge ChallengeId, bMeta *BuildMetadata, image Image) string {
	name := fmt.Sprintf("%s:%s", challenge, bMeta.dockerId(image))
	if m.challengeRegistry != "" {
		name = fmt.Sprintf("%s/%s", m.challengeRegistry, name)
	}
	return name
}

// pushImage pushes the image to the configured registry under its
// content-addressed tag.
func (m *Manager) pushImage(imageName string) error {
	pushOpts := client.ImagePushOptions{RegistryAuth: m.authString}
	pushResp, err := m.cli.ImagePush(m.ctx, imageName, pushOpts)
	if err != nil {
		m.log.errorf("failed to push image '%s': %s", imageName, err)
		return err
	}
	messages, err := ioutil.ReadAll(pushResp)
	pushResp.Close()
	if err != nil {
		m.log.errorf("failed to read push response from docker: %s", err)
		return err
	}
	if streamErr := dockerStreamError(messages); streamErr != nil {
		err = fmt.Errorf("failed to push image '%s': %s", imageName, streamErr)
		m.log.error(err)
		return err
	}
	return nil
}

// pullImage pulls on the given daemon (workers pull with their own registry
// certs via certs.d, so no credentials travel with the request), including
// reading the pull stream, within timeout. A pull that exceeds it fails the
// launch as retryable (ErrPullTimeout) but never marks the worker down: the
// registry, not the worker, is the likelier culprit.
func (m *Manager) pullImage(cli *client.Client, imageName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	defer cancel()
	timedOut := func(err error) error {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %s after %s: %v", ErrPullTimeout, imageName, timeout, err)
		}
		return err
	}
	pullOpts := client.ImagePullOptions{RegistryAuth: m.authString}
	pullResp, err := cli.ImagePull(ctx, imageName, pullOpts)
	if err != nil {
		m.log.errorf("failed to pull image '%s': %s", imageName, err)
		return timedOut(err)
	}
	messages, err := ioutil.ReadAll(pullResp)
	pullResp.Close()
	if err != nil {
		m.log.errorf("failed to read pull response from docker: %s", err)
		return timedOut(err)
	}
	if streamErr := dockerStreamError(messages); streamErr != nil {
		err = fmt.Errorf("failed to pull image '%s': %s", imageName, streamErr)
		m.log.error(err)
		return err
	}
	return nil
}

func (m *Manager) freezeBaseImage(challenge ChallengeId, force bool) error {
	cMeta, err := m.lookupChallengeMetadata(challenge)
	if err != nil {
		return err
	}

	imageName := fmt.Sprintf("%s/%s:%x", m.challengeRegistry, challengeToFreezeName(challenge), cMeta.SourceChecksum)

	if !force {
		// Do some check here to see if it already exists
	}

	buildCtxFile, err := m.createBuildContext(cMeta, m.GetDockerfile(cMeta.ChallengeType))
	if err != nil {
		m.log.errorf("failed to create build context: %s", err)
		return err
	}
	defer os.Remove(buildCtxFile)
	buildCtx, err := os.Open(buildCtxFile)
	if err != nil {
		m.log.errorf("failed to seek to beginning of file for %s: %s", cMeta.Id, err)
		return err
	}
	defer buildCtx.Close()

	// Setup build options
	opts := client.ImageBuildOptions{
		Remove:     true,
		Tags:       []string{imageName},
		Target:     "base",
		NoCache:    force, // Require to use latest info on force
		PullParent: force, // Update parent image as well on force
		Labels: map[string]string{
			"cmgr.managed":   "true",
			"cmgr.challenge": string(challenge),
		},
	}

	// Build the image
	m.log.debugf("creating base image %s", imageName)
	resp, err := m.cli.ImageBuild(m.ctx, buildCtx, opts)
	if err != nil {
		m.log.errorf("failed to build base image: %s", err)
		return err
	}

	// Read the response because errors aren't propagated.
	messages, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		m.log.errorf("failed to read build response from docker: %s", err)
		return err
	}

	// Search the response for an error message
	if streamErr := dockerStreamError(messages); streamErr != nil {
		err = fmt.Errorf("failed to build image: %s", streamErr)
		m.log.error(err)
		return err
	}

	return m.pushImage(imageName)
}

func (m *Manager) executeBuild(cMeta *ChallengeMetadata, bMeta *BuildMetadata, buildCtxFile string) error {

	seedStr := fmt.Sprintf("%d", bMeta.Seed)

	// Stamp the build with the content identity its images are about to be
	// produced from; dockerId (and therefore every tag below) depends on it.
	bMeta.Checksum = contentChecksum(cMeta.SourceChecksum, bMeta.Format)

	baseName := fmt.Sprintf("%s/%s:%x", m.challengeRegistry, challengeToFreezeName(cMeta.Id), cMeta.SourceChecksum)
	pullOpts := client.ImagePullOptions{RegistryAuth: m.authString}
	var buildCache []string
	pullResp, err := m.cli.ImagePull(m.ctx, baseName, pullOpts)
	if err == nil {
		// Read the response because errors aren't propagated.
		messages, err := ioutil.ReadAll(pullResp)
		pullResp.Close()
		if err == nil {
			// Search the response for an error message
			if dockerStreamError(messages) == nil {
				m.log.infof("Successfully pulled base image '%s'", baseName)
				buildCache = append(buildCache, baseName)
			}
		}
	}

	images := []Image{}
	var buildImage string
	for _, host := range cMeta.Hosts {
		image := Image{Host: host.Name, Ports: []string{}}
		imageName := m.instanceImageName(cMeta.Id, bMeta, image)

		if host.Name == "builder" || (host.Name == "challenge" && buildImage == "") {
			buildImage = imageName
		}

		for _, portInfo := range cMeta.PortMap {
			if portInfo.Host == image.Host {
				image.Ports = append(image.Ports, fmt.Sprintf("%d/tcp", portInfo.Port))
			}
		}

		// Setup build options
		opts := client.ImageBuildOptions{
			BuildArgs: map[string]*string{
				"FLAG_FORMAT": &bMeta.Format,
				"SEED":        &seedStr,
				"FLAG":        bMeta.makeFlag(),
			},
			Remove:    true,
			CacheFrom: buildCache,
			Tags:      []string{imageName},
			Target:    host.Target,
			Labels: map[string]string{
				"cmgr.managed":   "true",
				"cmgr.challenge": string(cMeta.Id),
			},
		}

		// Call build
		buildCtx, err := os.Open(buildCtxFile)
		if err != nil {
			m.log.errorf("failed to seek to beginning of file for %s/%d: %s", cMeta.Id, bMeta.Id, err)
			return err
		}
		defer buildCtx.Close()

		m.log.debugf("creating image %s", imageName)
		resp, err := m.cli.ImageBuild(m.ctx, buildCtx, opts)
		if err != nil {
			m.log.errorf("failed to build base image: %s", err)
			return err
		}

		messages, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			m.log.errorf("failed to read build response from docker: %s", err)
			return err
		}

		if streamErr := dockerStreamError(messages); streamErr != nil {
			err = fmt.Errorf("failed to build image: %s", streamErr)
			m.log.error(err)
			return err
		}

		// Registry mode: workers pull instance images from the registry, so
		// every image a worker may run must be pushed at build time. The
		// "builder" image is only used locally for artifact extraction.
		if m.challengeRegistry != "" && image.Host != "builder" {
			if err := m.pushImage(imageName); err != nil {
				return err
			}
		}
		images = append(images, image)
	}

	if buildImage == "" {
		err := fmt.Errorf("aborting because no build image identified %s/%d", cMeta.Id, bMeta.Id)
		m.log.error(err)
		return err
	}

	// This container is created only to copy the built /challenge tree out of the
	// image; it is never started. Override the command so creation succeeds even
	// for images with no CMD/ENTRYPOINT (e.g. artifact-only custom Dockerfiles),
	// which Docker otherwise rejects with "no command specified".
	// NOTE: a future `rollback` would re-run everything from here down against
	// the PrevChecksum image (no docker build) to restore that generation's
	// artifacts, lookups, and DB state.
	cConfig := container.Config{Image: buildImage, Cmd: []string{"true"}}
	hConfig := container.HostConfig{}
	nConfig := network.NetworkingConfig{}

	if m.hostOSType == "linux" {
		m.log.debug("inserting custom seccomp profile")
		hConfig.SecurityOpt = []string{"seccomp:" + seccompPolicy}
	}

	respCC, err := m.cli.ContainerCreate(m.ctx, client.ContainerCreateOptions{
		Config:           &cConfig,
		HostConfig:       &hConfig,
		NetworkingConfig: &nConfig,
	})
	if err != nil {
		m.log.errorf("failed to create artifacts container: %s", err)
		return err
	}

	cid := respCC.ID
	crOpts := client.ContainerRemoveOptions{RemoveVolumes: true, Force: true}
	defer m.cli.ContainerRemove(m.ctx, cid, crOpts)

	m.log.infof("created container %s", cid)

	res, err := m.cli.CopyFromContainer(m.ctx, cid, client.CopyFromContainerOptions{SourcePath: "/challenge"})
	if err != nil {
		m.log.errorf("could not find '/challenge' in container: %s", err)
		return err
	}
	metaFile := res.Content
	defer metaFile.Close()

	cTar := tar.NewReader(metaFile)
	var hdr *tar.Header
	var lookups map[string]string
	var files []string
	var stagedArtifactsPath string
	var flag string
	for hdr, err = cTar.Next(); err == nil; hdr, err = cTar.Next() {
		m.log.debugf("found in tar: %s", hdr.Name)
		if hdr.Name == "challenge/metadata.json" {
			data, err := ioutil.ReadAll(cTar)
			if err != nil {
				m.log.errorf("could not read metadata.json: %s", err)
				return err
			}

			lookups = make(map[string]string)
			err = json.Unmarshal(data, &lookups)
			if err != nil {
				m.log.errorf("could not decode build metadata JSON file: %s", err)
				return err
			}

			var ok bool
			flag, ok = lookups["flag"]
			if !ok {
				err = errors.New("build metadata missing the flag")
				m.log.error(err)
				return err
			}

			delete(lookups, "flag")
		} else if hdr.Name == "challenge/artifacts.tar.gz" {
			// Stage the new archive beside the final build-ID path and only
			// promote it after validateBuild passes: a failed rebuild must leave
			// the previous archive in place, because cmgrd keeps serving it (the
			// build row is rolled back, not removed) and deleting it would turn
			// every player download into a 500. The dot-prefixed name can never
			// collide with a served "<id>.tar.gz" path.
			stagedArtifactsPath = filepath.Join(m.artifactsDir, "."+bMeta.getArtifactsFilename()+".staged")
			files, err = m.cacheArtifacts(cTar, stagedArtifactsPath)
			if err != nil {
				m.log.errorf("could not cache artifacts: %s", err)
				return err
			}
			for _, name := range files {
				m.log.debugf("artifact found: %s", name)
			}
		}
	}

	if err != io.EOF {
		m.log.errorf("could not read metadata file: %s", err)
		return err
	}

	if flag == "" {
		err = errors.New("'flag' missing in metadata.json")
		m.log.error(err)
		return err
	}

	bMeta.Flag = flag
	bMeta.LookupData = lookups
	bMeta.Images = images
	bMeta.HasArtifacts = len(files) > 0

	err = m.validateBuild(cMeta, bMeta, files)
	if err == nil && stagedArtifactsPath != "" {
		// Promote the validated archive into place; rename is atomic, so
		// concurrent downloads see either the old or the new archive, never a
		// partial one. The directory sync persists the swap across a crash.
		finalArtifactsPath := filepath.Join(m.artifactsDir, bMeta.getArtifactsFilename())
		err = os.Rename(stagedArtifactsPath, finalArtifactsPath)
		if err != nil {
			m.log.errorf("could not promote artifact archive: %s", err)
		} else if directory, dirErr := os.Open(m.artifactsDir); dirErr == nil {
			_ = directory.Sync()
			_ = directory.Close()
		}
	}
	if err != nil {
		if stagedArtifactsPath != "" {
			os.Remove(stagedArtifactsPath)
		}

		// Free the build image now rather than waiting for the deferred
		// container cleanup: while the extraction container exists it references
		// the image, so the non-forced ImageRemove below could not untag it.
		// (The deferred removal then no-ops on the already-gone container.)
		_, _ = m.cli.ContainerRemove(m.ctx, cid, crOpts)

		// Untag the failed generation's images unless they must survive: another
		// build row may share this exact (challenge, seed, format, checksum)
		// tuple (e.g. the same challenge and seed in two schemas), or THIS row —
		// on the rebuild path, not yet finalized — may retain this checksum as
		// its rollback target (PrevChecksum), as after an A->B->A source revert.
		// Both cases keep the images; only a genuinely unreferenced failed
		// generation is removed. imageMu keeps the check-and-remove atomic
		// against concurrent removers (destroyImages/pruneReplacedImages).
		m.imageMu.Lock()
		if bMeta.PrevChecksum != bMeta.Checksum && !m.contentReferenced(bMeta, bMeta.Id) {
			iro := client.ImageRemoveOptions{Force: false, PruneChildren: true}
			for _, image := range bMeta.Images {
				imageName := m.instanceImageName(bMeta.Challenge, bMeta, image)
				_, _ = m.cli.ImageRemove(m.ctx, imageName, iro)
			}
		}
		m.imageMu.Unlock()
	}

	m.log.debugf("%v", bMeta)

	return err
}

func (m *Manager) startNetwork(instance *InstanceMetadata, opts NetworkOptions) error {
	cli, err := m.instanceClient(instance)
	if err != nil {
		return err
	}
	netSpec := client.NetworkCreateOptions{
		Driver: "bridge",
	}
	netname := instance.getNetworkName()
	ctx, cancel := m.controlCtx()
	_, err = cli.NetworkCreate(ctx, netname, netSpec)
	cancel()
	if errdefs.IsConflict(err) {
		// Instance ids are reused once their rows are deleted, so the network
		// of an earlier instance with this id can still exist on the daemon
		// when its removal was cut short (one that timed out client-side and
		// finished on the daemon after its containers were gone). Nothing of
		// ours is on it: remove it and create again, each call under its own
		// control timeout. A network that still has endpoints, the leftovers
		// of a DB-only stop that reconcileWorker clears when the box rejoins
		// placement, refuses removal; that failure is reported alongside the
		// original conflict, and stays in the error chain, so that a removal
		// which timed out or found the daemon gone still marks the worker
		// down below.
		m.log.warnf("stale challenge network %s already exists; removing it and retrying", netname)
		rmCtx, rmCancel := m.controlCtx()
		_, rmErr := cli.NetworkRemove(rmCtx, netname, client.NetworkRemoveOptions{})
		rmCancel()
		if rmErr != nil {
			err = fmt.Errorf("%w (stale network not removed: %w)", err, rmErr)
		} else {
			retryCtx, retryCancel := m.controlCtx()
			_, err = cli.NetworkCreate(retryCtx, netname, netSpec)
			retryCancel()
		}
	}
	if err != nil {
		m.log.errorf("could not create challenge network (%s): %s", netname, err)
		m.noteWorkerTransportError(instance.Worker, err)
	}
	return err
}

func (m *Manager) stopNetwork(instance *InstanceMetadata) error {
	cli, err := m.instanceClient(instance)
	if err != nil {
		return err
	}
	networkName := instance.getNetworkName()
	ctx, cancel := m.controlCtx()
	defer cancel()
	_, err = cli.NetworkRemove(ctx, networkName, client.NetworkRemoveOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			m.log.warnf("skipped removing network (not found): %s", networkName)
			err = nil
		} else {
			m.log.errorf("failed to remove network: %s", err)
			m.noteWorkerTransportError(instance.Worker, err)
		}
	}
	return err
}

// portsAlreadyKnown reports whether every published port for an image already has
// a non-zero host port reserved in ports, so the post-start read-back in
// startContainers can be safely skipped. It is false when explicit ports are
// disabled (portLow == 0, Docker assigns ephemeral ports) or when any required
// port is unassigned — e.g. the rebuild flow, which clears instance.Ports before
// restarting and so must read the bound port back. A port whose name is missing
// from revPortMap is treated as unknown (false): without a resolvable name we
// cannot confirm its reservation, and must not skip the read-back.
func portsAlreadyKnown(portLow int, imagePorts []string, ports map[string]int, revPortMap map[string]string) bool {
	if portLow == 0 {
		return false
	}
	for _, portStr := range imagePorts {
		name, ok := revPortMap[portStr]
		if !ok || name == "" || ports[name] == 0 {
			return false
		}
	}
	return true
}

func (m *Manager) startContainers(build *BuildMetadata, instance *InstanceMetadata, opts map[string]ContainerOptions, envVars map[string]string, revPortMap map[string]string, limits launchLimits) error {
	// Everything below runs against the daemon hosting this instance: the
	// local one, or the recorded worker's.
	cli, err := m.instanceClient(instance)
	if err != nil {
		return err
	}

	// Call create in docker
	netname := instance.getNetworkName()
	for _, image := range build.Images {
		if image.Host == "builder" {
			continue
		}
		exposedPorts := network.PortSet{}
		publishedPorts := network.PortMap{}
		for _, portStr := range image.Ports {
			port, err := network.ParsePort(portStr)
			if err != nil {
				return fmt.Errorf("invalid port %q in image configuration: %w", portStr, err)
			}
			var hostPort string
			if m.portLow == 0 {
				hostPort = ""
			} else {
				hostPort = strconv.Itoa(instance.Ports[revPortMap[portStr]])
			}

			exposedPorts[port] = struct{}{}
			var addr netip.Addr
			if m.challengeInterface != "" {
				addr, err = netip.ParseAddr(m.challengeInterface)
				if err != nil {
					return fmt.Errorf("invalid challenge interface %q: %w", m.challengeInterface, err)
				}
			}
			publishedPorts[port] = []network.PortBinding{
				{HostIP: addr, HostPort: hostPort},
			}
		}

		isDynamicInstance := "false"
		if build.InstanceCount == DYNAMIC_INSTANCES {
			isDynamicInstance = "true"
		}

		cLabels := map[string]string{
			"cmgr.managed": "true",
			"cmgr.dynamic": isDynamicInstance,
		}

		cConfig := container.Config{
			Image:        m.instanceImageName(build.Challenge, build, image),
			Hostname:     image.Host,
			ExposedPorts: exposedPorts,
			Labels:       cLabels,
		}

		// Note: envVars (including user_id and any caller-supplied variables) are
		// injected identically into every container in the build. In multi-container
		// challenges all containers will receive the same set of environment variables.
		if len(envVars) > 0 {
			var envList []string
			for k, v := range envVars {
				envList = append(envList, fmt.Sprintf("%s=%s", k, v))
			}
			cConfig.Env = append(cConfig.Env, envList...)
		}

		hConfig := container.HostConfig{
			PortBindings:  publishedPorts,
			RestartPolicy: container.RestartPolicy{Name: "always"},
		}

		hasContainerOpts := false
		cOpts, hasContainerOpts := opts[""]
		if hostCOpts, ok := opts[strings.ToLower(image.Host)]; ok {
			cOpts = hostCOpts
			hasContainerOpts = true
		}
		if image.Host == "builder" {
			hasContainerOpts = false
		}
		if hasContainerOpts {
			hConfig.Init = &cOpts.Init
			if cOpts.Cpus != "" {
				nanoCpus, err := parseNanoCPUs(cOpts.Cpus)
				if err != nil {
					return err
				}
				hConfig.NanoCPUs = nanoCpus
			}
			if cOpts.Memory != "" {
				memoryBytes, err := units.RAMInBytes(cOpts.Memory)
				if err != nil {
					return err
				}
				hConfig.Memory = memoryBytes
			}
			if len(cOpts.Ulimits) > 0 {
				limits := make([]*units.Ulimit, len(cOpts.Ulimits))
				for i, limitStr := range cOpts.Ulimits {
					limit, err := units.ParseUlimit(limitStr)
					if err != nil {
						return err
					}
					limits[i] = limit
				}
				hConfig.Ulimits = limits
			}
			if cOpts.PidsLimit != 0 {
				hConfig.PidsLimit = &cOpts.PidsLimit
			}
			hConfig.ReadonlyRootfs = cOpts.ReadonlyRootfs
			hConfig.CapDrop = (strslice.StrSlice)(cOpts.DroppedCaps)
			if cOpts.NoNewPrivileges {
				hConfig.SecurityOpt = append(hConfig.SecurityOpt, "no-new-privileges:true")
			}
			if cOpts.DiskQuota != "" {
				_, quotas_enabled := os.LookupEnv(DISK_QUOTA_ENV)
				if quotas_enabled {
					var storageOpt = map[string]string{
						"size": cOpts.DiskQuota,
					}
					hConfig.StorageOpt = storageOpt
				} else {
					m.log.warnf("disk quota for %s container '%s' ignored (disk quotas are not enabled)", build.Challenge, image.Host)
				}
			}
			if cOpts.CgroupParent != "" {
				hConfig.CgroupParent = cOpts.CgroupParent
			}
		}

		if m.hostOSType == "linux" {
			if hasContainerOpts && cOpts.CapImmutable {
				hConfig.CapAdd = append(hConfig.CapAdd, "LINUX_IMMUTABLE")
			}
			profile := seccompPolicy
			if hasContainerOpts && cOpts.Seccomp != nil &&
				cOpts.Seccomp.effectiveProfile != "" {
				profile = cOpts.Seccomp.effectiveProfile
				m.log.debugf(
					"inserting challenge seccomp profile %s (%s)",
					cOpts.Seccomp.Profile,
					cOpts.Seccomp.ProfileHash,
				)
			} else {
				m.log.debug("inserting custom seccomp profile")
			}
			hConfig.SecurityOpt = append(hConfig.SecurityOpt, "seccomp:"+profile)
		}

		nConfig := network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netname: {
					NetworkID: netname,
					Aliases:   []string{image.Host},
				},
			},
		}

		ccCtx, ccCancel := m.controlCtx()
		respCC, err := cli.ContainerCreate(ccCtx, client.ContainerCreateOptions{
			Config:           &cConfig,
			HostConfig:       &hConfig,
			NetworkingConfig: &nConfig,
		})
		ccCancel()
		if errdefs.IsNotFound(err) && m.challengeRegistry != "" {
			// The image vanished between the pre-launch digest check and the
			// create (e.g. an image reaper on the worker). Pull and retry once.
			m.log.warnf("image '%s' disappeared before create; pulling and retrying", cConfig.Image)
			if err = m.pullImage(cli, cConfig.Image, limits.pullTimeout); err == nil {
				ccCtx, ccCancel = m.controlCtx()
				respCC, err = cli.ContainerCreate(ccCtx, client.ContainerCreateOptions{
					Config:           &cConfig,
					HostConfig:       &hConfig,
					NetworkingConfig: &nConfig,
				})
				ccCancel()
			}
		}
		if err != nil {
			m.log.errorf("failed to create instance container: %s", err)
			m.noteWorkerTransportError(instance.Worker, err)
			return err
		}

		cid := respCC.ID
		instance.Containers = append(instance.Containers, cid)
		m.log.infof("created new container: %s", cid)

		csCtx, csCancel := m.controlCtx()
		_, err = cli.ContainerStart(csCtx, cid, client.ContainerStartOptions{})
		csCancel()
		if err != nil {
			m.log.errorf("failed to start container: %s", err)
			m.noteWorkerTransportError(instance.Worker, err)
			return err
		}

		// When the port mapping is already known (explicit ports that were reserved
		// and bound before ContainerCreate), instance.Ports already holds the correct
		// values and a successful ContainerStart guarantees the port is bound. The
		// inspect/backoff loop below could only re-read the value we already set (and
		// would needlessly eat backoff sleeps while networking settles), so skip it.
		// It is still needed when the mapping is not yet known: Docker-assigned
		// ephemeral ports, or an explicit-port path entered with instance.Ports
		// cleared (e.g. rebuild), where the bound port must be read back.
		if !portsAlreadyKnown(m.portLow, image.Ports, instance.Ports, revPortMap) {
			backoff := time.Millisecond
			done := false
			for !done && backoff < time.Second {
				m.log.debug("Querying docker for port info...")

				ciCtx, ciCancel := m.controlCtx()
				cInfo, err := cli.ContainerInspect(ciCtx, cid, client.ContainerInspectOptions{})
				ciCancel()
				if err != nil {
					m.log.errorf("failed to get container info: %s", err)
					m.noteWorkerTransportError(instance.Worker, err)
					return err
				}
				if cInfo.Container.NetworkSettings == nil {
					done = false
					time.Sleep(backoff)
					backoff = 2 * backoff
					continue
				}

				done = true
				for cPort, hPortInfo := range cInfo.Container.NetworkSettings.Ports {
					if len(hPortInfo) == 0 {
						done = false
						time.Sleep(backoff)
						backoff = 2 * backoff
						break
					}

					hPort, err := strconv.Atoi(string(hPortInfo[0].HostPort))
					if err != nil {
						return err
					}
					name, ok := revPortMap[cPort.String()]
					if !ok {
						// No reverse-map entry: writing under "" would drop the real
						// mapping (and clobber other unmapped ports). Skip it instead.
						m.log.warnf("ignoring container port %s with no reverse-port-map entry", cPort)
						continue
					}
					instance.Ports[name] = hPort
					m.log.debugf("container port %s mapped to %s", cPort, hPortInfo[0].HostPort)
				}
			}
		}
	}

	return retryableDB(m.finalizeInstance(instance))
}

func (m *Manager) stopContainers(instance *InstanceMetadata) error {
	cli, err := m.instanceClient(instance)
	if err != nil {
		return err
	}
	for _, cid := range instance.Containers {
		crOpts := client.ContainerRemoveOptions{RemoveVolumes: true, Force: true}
		crCtx, crCancel := m.controlCtx()
		_, err = cli.ContainerRemove(crCtx, cid, crOpts)
		crCancel()
		if err != nil {
			if errdefs.IsNotFound(err) {
				m.log.warnf("skipped removing container (not found): %s", cid)
				err = nil
			} else {
				m.log.errorf("failed to remove container: %s", err)
				m.noteWorkerTransportError(instance.Worker, err)
				if isTransportError(err) {
					// The daemon is unreachable and the worker is down now;
					// the remaining removals would only repeat the timeout.
					break
				}
			}
		}
	}

	mdErr := m.removeContainersMetadata(instance)
	if mdErr != nil {
		err = retryableDB(mdErr)
	}

	return err
}

// replacedImages records the docker tags of a build generation superseded by
// a rebuild, along with the identity tuple those tags encode.
type replacedImages struct {
	tags []string
	meta BuildMetadata
}

// pruneReplacedImages untags image generations that fell out of retention
// after an update rebuild (the 'update --prune-old' flow): the generation
// displaced from PrevChecksum, never the newly retained rollback target. Each
// entry is skipped while any live build row still resolves to its tuple;
// removal failures are logged but never fatal — a leaked tag is recoverable,
// a failed update is not.
func (m *Manager) pruneReplacedImages(replaced []replacedImages) {
	// Serialize the reference-check-then-remove against concurrent removers
	// (destroyImages, executeBuild cleanup): content-addressed tags are shared
	// across build rows, so an unsynchronized check could untag images a build
	// finalized between the check and the removal.
	m.imageMu.Lock()
	defer m.imageMu.Unlock()

	iro := client.ImageRemoveOptions{Force: false, PruneChildren: true}
	for _, r := range replaced {
		if m.contentReferenced(&r.meta, 0) {
			m.log.debugf("keeping replaced images for %s: content still referenced by another build", r.meta.Challenge)
			continue
		}
		for _, tag := range r.tags {
			if _, err := m.cli.ImageRemove(m.ctx, tag, iro); err != nil {
				if errdefs.IsNotFound(err) {
					// Already removed — e.g. a build in another schema shared
					// the tuple and its entry was pruned first.
					m.log.debugf("replaced image already gone: %s", tag)
				} else {
					m.log.warnf("could not prune replaced image %s: %s", tag, err)
				}
			} else {
				m.log.infof("pruned replaced image %s", tag)
			}
			// Registry mode: also untag the generation in the registry, or it
			// accumulates one immutable tag per rebuild forever. Best-effort —
			// a leaked registry tag is recoverable, a failed update is not.
			if m.challengeRegistry != "" {
				if err := m.registryDeleteTag(tag); err != nil {
					m.log.warnf("could not prune replaced registry tag %s: %s", tag, err)
				}
			}
		}
	}
}

func (m *Manager) destroyImages(build BuildId) error {
	m.log.debugf("destroying build %d", build)
	bMeta, err := m.lookupBuildMetadata(build)
	if err != nil {
		return err
	}

	err = m.removeBuildMetadata(build)
	if err != nil {
		return err
	}

	if bMeta.HasArtifacts {
		artifactsFilename := bMeta.getArtifactsFilename()
		err := os.Remove(filepath.Join(m.artifactsDir, artifactsFilename))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				m.log.warnf("skipped removing artifacts file (not found): %s", artifactsFilename)
				err = nil
			} else {
				m.log.errorf("failed to remove artifacts file: %s", err)
				return err
			}
		}
	}

	// The build's metadata row is already gone, so any remaining row holding
	// this content as its current or rollback generation is another build that
	// shares these exact tags (e.g. the same challenge and seed in two
	// schemas); in that case the images must survive. The current and rollback
	// generations are checked independently — either can outlive the other.
	//
	// Force is false and removal failures are non-fatal by design: a content
	// tag can be referenced outside this cmgr database — another cmgr instance
	// on the same daemon that built identical content, possibly with a running
	// or stopped container this database cannot see. Refusing to delete an
	// in-use image and leaking the tag is recoverable; force-removing it out
	// from under another instance is not. This mirrors the opt-in rationale for
	// 'update --prune-old' (see UpdateOptions.PruneOldImages). imageMu serializes
	// the reference-check-then-remove against concurrent removers.
	m.imageMu.Lock()
	defer m.imageMu.Unlock()

	iro := client.ImageRemoveOptions{Force: false, PruneChildren: true}
	if m.contentReferenced(bMeta, bMeta.Id) {
		m.log.debugf("keeping images for destroyed build %d: content still referenced by another build", build)
	} else {
		for _, image := range bMeta.Images {
			imageName := m.instanceImageName(bMeta.Challenge, bMeta, image)
			if _, err := m.cli.ImageRemove(m.ctx, imageName, iro); err != nil {
				if errdefs.IsNotFound(err) {
					m.log.warnf("skipped removing image (not found): %s", imageName)
				} else {
					m.log.warnf("could not remove image %s (leaving it in place): %s", imageName, err)
				}
			}
			// Best-effort registry untag, mirroring pruneReplacedImages.
			if m.challengeRegistry != "" && image.Host != "builder" {
				if err := m.registryDeleteTag(imageName); err != nil {
					m.log.warnf("could not remove registry tag %s: %s", imageName, err)
				}
			}
		}
	}

	// Retire the build's retained rollback generation the same way; its tags
	// may already be gone (never rotated, or pruned via another row), so
	// removal here is best-effort.
	if bMeta.PrevChecksum != 0 && bMeta.PrevChecksum != bMeta.Checksum {
		prevMeta := BuildMetadata{
			Challenge: bMeta.Challenge,
			Seed:      bMeta.Seed,
			Format:    bMeta.Format,
			Checksum:  bMeta.PrevChecksum,
		}
		if !m.contentReferenced(&prevMeta, bMeta.Id) {
			for _, image := range bMeta.Images {
				imageName := m.instanceImageName(bMeta.Challenge, &prevMeta, image)
				if _, err := m.cli.ImageRemove(m.ctx, imageName, iro); err != nil && !errdefs.IsNotFound(err) {
					m.log.warnf("could not remove rollback-generation image %s: %s", imageName, err)
				}
				if m.challengeRegistry != "" && image.Host != "builder" {
					if err := m.registryDeleteTag(imageName); err != nil {
						m.log.warnf("could not remove rollback-generation registry tag %s: %s", imageName, err)
					}
				}
			}
		}
	}

	return nil
}
