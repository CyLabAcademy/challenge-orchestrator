package cmgr

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// A launch runs against its daemon in two stages:
//
//   - image stage, unbounded: an inspect under the control timeout, then a
//     pull of what is missing under the pull timeout. Pulls never hold a
//     launch slot, so the pull of one instance overlaps the container start
//     of another; that overlap is what makes a pull per launch free. They
//     get no cap of their own: dockerd coalesces concurrent pulls of the
//     same image, and caps of 1, 2 and 16 measured identically.
//   - launch slot: network create, container create and start, and the port
//     read-back, each under the control timeout. These serialize inside
//     dockerd on either firewall backend, per daemon, so the slots are per
//     worker; that is why CMGR_CONCURRENT_LAUNCHES measured out at 2 on
//     iptables and shows no gain past 2 on nftables either (nftables makes
//     each launch faster, not more parallel). A slot that covers the network
//     stage keeps the daemon's queue in cmgrd, where it is bounded, instead
//     of inside dockerd, where a deep queue looks like a hung call.
//
// Waiting for a slot is bounded for a request-driven launch and refused
// outright once the worker is down: under adverse load a request fails
// fast, and the platform retries onto another worker instead of queueing
// behind a wedged or saturated daemon. The bounds are a launchLimits; a
// restart during a rebuild gets looser ones.

var (
	// ErrWorkerBusy: no slot on the instance's daemon within the wait.
	// Retryable (cmgrd answers 503 with Retry-After).
	ErrWorkerBusy = errors.New("worker is busy")
	// ErrWorkerDown: the instance's worker went down between placement and
	// the launch stage. Retryable: placement now skips it.
	ErrWorkerDown = errors.New("worker went down")
	// ErrPullTimeout: an image pull exceeded the pull timeout. Retryable: the
	// next attempt may land on a worker that already holds the image.
	ErrPullTimeout = errors.New("image pull timed out")
)

// launchLimits bounds one launch: how long it waits for a launch slot, and
// how long one image pull may take. Two settings exist:
//
//   - requestLimits, for a launch the platform asked for (POST /builds/<id>):
//     the configured launch wait and pull timeout, both short, so that under
//     adverse load the request fails as retryable and the platform's retry
//     is placed afresh;
//   - restartLimits, for the restart of a persistent instance during a
//     rebuild: nothing retries one, so it waits for its slot as long as it
//     takes, and its pull, always cold since the image was pushed moments
//     ago, gets a ceiling of minutes rather than seconds.
type launchLimits struct {
	slotWait      time.Duration // zero waits as long as it takes
	pullTimeout   time.Duration
	imagesEnsured bool // the caller ran ensureImages itself (a restart pulls before its teardown)
}

// restartPullTimeout is the pull ceiling of a restart during a rebuild; the
// configured pull timeout applies instead when it is longer.
const restartPullTimeout = 5 * time.Minute

func (m *Manager) requestLimits() launchLimits {
	t := m.timing()
	return launchLimits{slotWait: t.launchWait, pullTimeout: t.pullTimeout}
}

func (m *Manager) restartLimits() launchLimits {
	return launchLimits{slotWait: 0, pullTimeout: max(restartPullTimeout, m.timing().pullTimeout)}
}

// launch brings an instance up on its daemon within limits: images, network,
// containers. started reports whether the launch slot was taken, that is
// whether the daemon may hold the instance's network or containers when err
// is set: a launch that failed before its slot (worker down, image check or
// pull failed, no slot within the wait) left nothing on the daemon, so its
// caller can clear the instance's records without a docker round trip,
// which against a wedged daemon would cost another control timeout. A
// failure that leaves the worker down (its hung call is what marked it) is
// reported as ErrWorkerDown: the platform's retry is placed elsewhere.
func (m *Manager) launch(build *BuildMetadata, instance *InstanceMetadata, netOpts NetworkOptions,
	opts map[string]ContainerOptions, envVars map[string]string, revPortMap map[string]string, limits launchLimits) (started bool, err error) {
	started, err = m.launchStages(build, instance, netOpts, opts, envVars, revPortMap, limits)
	if err != nil && !errors.Is(err, ErrWorkerDown) && instance.Worker != "" && m.workerIsDown(instance.Worker) {
		err = fmt.Errorf("%w: worker %s, during the launch of instance %d: %v", ErrWorkerDown, instance.Worker, instance.Id, err)
	}
	return started, err
}

func (m *Manager) launchStages(build *BuildMetadata, instance *InstanceMetadata, netOpts NetworkOptions,
	opts map[string]ContainerOptions, envVars map[string]string, revPortMap map[string]string, limits launchLimits) (bool, error) {
	cli, err := m.instanceClient(instance)
	if err != nil {
		return false, err
	}
	if instance.Worker != "" && m.workerIsDown(instance.Worker) {
		return false, fmt.Errorf("%w: worker %s, before the launch of instance %d", ErrWorkerDown, instance.Worker, instance.Id)
	}
	if err := m.ensureImages(cli, build, instance, limits); err != nil {
		return false, err
	}
	release, err := m.acquireSlot(m.launchSem(instance), instance, "launch", limits.slotWait)
	if err != nil {
		return false, err
	}
	defer release()
	if err := m.startNetwork(instance, netOpts); err != nil {
		return true, err
	}
	return true, m.startContainers(build, instance, opts, envVars, revPortMap, limits)
}

// ensureImages makes sure the daemon holds every image of the build, each
// pull under the limits' pull timeout, unless the caller already did
// (limits.imagesEnsured). Registry mode only: without a registry the daemon
// built the images itself.
func (m *Manager) ensureImages(cli *client.Client, build *BuildMetadata, instance *InstanceMetadata, limits launchLimits) error {
	if limits.imagesEnsured || m.challengeRegistry == "" {
		return nil
	}
	for _, image := range build.Images {
		if image.Host == "builder" {
			continue
		}
		name := m.instanceImageName(build.Challenge, build, image)
		present, err := m.imagePresent(cli, instance.Worker, name)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		if err := m.pullImage(cli, name, limits.pullTimeout); err != nil {
			// The daemon can go between the inspect and the pull, and a pull
			// that finds it gone is a control failure like any other. A pull
			// that merely ran out of time is not: the registry is the likelier
			// culprit, and the worker keeps its place (see pullImage).
			if !errors.Is(err, ErrPullTimeout) {
				m.noteWorkerTransportError(instance.Worker, err)
			}
			return err
		}
	}
	return nil
}

// imagePresent reports whether the daemon already holds the tag, in which
// case the pull is skipped: tags are content-addressed and cmgrd is their
// sole writer, so a present tag names the right content. The inspect runs
// under the control timeout, and anything but a clean "not found" is a
// failure: a hung or unreachable daemon is reported like any other control
// call (marking a worker down) rather than answered with a pull that would
// hang the same way.
func (m *Manager) imagePresent(cli *client.Client, worker string, imageName string) (bool, error) {
	ctx, cancel := m.controlCtx()
	defer cancel()
	_, err := cli.ImageInspect(ctx, imageName)
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	m.log.errorf("failed to inspect image '%s': %s", imageName, err)
	m.noteWorkerTransportError(worker, err)
	return false, err
}

// acquireSlot takes a slot on the instance's daemon, waiting at most wait
// (as long as it takes when wait is zero), and refuses as soon as the
// instance's worker is down, before, during or after the wait: the wait ends
// on the worker's down channel as well as on a slot, so a queue behind a
// daemon that has just been given up on drains at once instead of holding
// each launch for its full wait, and an unbounded teardown wait does not
// hang on a daemon nothing will release a slot on. The returned release must
// be called when the stage is over.
func (m *Manager) acquireSlot(sem chan struct{}, instance *InstanceMetadata, what string, wait time.Duration) (release func(), err error) {
	if instance.Worker != "" && m.workerIsDown(instance.Worker) {
		return nil, fmt.Errorf("%w: worker %s, before the %s stage of instance %d", ErrWorkerDown, instance.Worker, what, instance.Id)
	}
	var expired <-chan time.Time // nil, so never, when the wait is unbounded
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		expired = timer.C
	}
	down := m.workerDownCh(instance.Worker)
	select {
	case sem <- struct{}{}:
	case <-down:
		return nil, fmt.Errorf("%w: worker %s, while instance %d waited for a %s slot", ErrWorkerDown, instance.Worker, instance.Id, what)
	case <-expired:
		return nil, fmt.Errorf("%w: no %s slot on %s for instance %d within %s", ErrWorkerBusy, what, daemonLabel(instance), instance.Id, wait)
	}
	if instance.Worker != "" && m.workerIsDown(instance.Worker) {
		<-sem
		return nil, fmt.Errorf("%w: worker %s, while instance %d waited for a %s slot", ErrWorkerDown, instance.Worker, instance.Id, what)
	}
	return func() { <-sem }, nil
}

func daemonLabel(instance *InstanceMetadata) string {
	if instance.Worker == "" {
		return "the local daemon"
	}
	return "worker " + instance.Worker
}

// envSlots reads a per-daemon slot count from the environment: 1 to 16,
// else the default with a warning.
func (m *Manager) envSlots(name string, def int) int {
	s, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 16 {
		m.log.warnf("invalid %s value '%s' (want 1 to 16), defaulting to %d", name, s, def)
		return def
	}
	return n
}
