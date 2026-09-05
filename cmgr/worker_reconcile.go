package cmgr

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	// managedLabel marks every container cmgr creates (see startContainers).
	managedLabel = "cmgr.managed"
	// networkPrefix is how instance networks are named: cmgr-<instance id>
	// (see getNetworkName).
	networkPrefix = "cmgr-"
	// reconcileParallelism bounds how many removals a reconcile keeps in
	// flight on one daemon.
	reconcileParallelism = 4
)

// reconcileResult reports how far a reconciliation pass got. The values are
// ordered by severity: a pass takes the worst of what its steps report.
type reconcileResult int32

const (
	// reconcileDone: the worker holds nothing belonging to instances it no
	// longer records, and can take placements.
	reconcileDone reconcileResult = iota
	// reconcileIncomplete: the pass reached the daemon but could not finish,
	// because a database read failed or the daemon refused a removal, so
	// leftovers may remain.
	reconcileIncomplete
	// reconcileUnreachable: the daemon could not be reached at all.
	reconcileUnreachable
)

func (r reconcileResult) String() string {
	switch r {
	case reconcileDone:
		return "done"
	case reconcileIncomplete:
		return "incomplete"
	default:
		return "its daemon unreachable"
	}
}

// reconcileWorker removes what cmgr created on the worker for instances it
// no longer records there: the containers and cmgr-<id> networks that a
// DB-only stop leaves behind while the worker is down or purged
// (stopInstance, RemoveWorker). They are not merely untidy. A box that
// rejoins placement, through worker-add or a cmgrd restart that starts every
// worker afresh, would otherwise refuse any new instance that draws one of
// their published host ports. It therefore runs before the worker's poller
// starts (runWorker), so nothing is placed on the worker until they are gone.
//
// It reports how far it got, and runWorker retries anything short of done:
// a transport failure ends the pass early as unreachable, and a database
// read that fails or a removal the daemon refuses leaves it incomplete,
// since leftovers may remain either way. Removals run a few at a time
// (reconcileParallelism): dockerd serializes the network side of each, so a
// few in flight keep it busy without a deep queue, and hundreds of leftovers
// take a fraction of the time they would one after another.
func (m *Manager) reconcileWorker(w *workerConn) reconcileResult {
	ctx, cancel := m.controlCtx()
	containers, err := w.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", managedLabel+"=true"),
	})
	cancel()
	if err != nil {
		return m.reconcileFailed(w, "list containers", err)
	}
	ctx, cancel = m.controlCtx()
	networks, err := w.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("name", networkPrefix),
	})
	cancel()
	if err != nil {
		return m.reconcileFailed(w, "list networks", err)
	}

	// The daemon is listed before the rows are read: a launch inserts its
	// row before it creates anything on the daemon, so whatever was listed
	// above and has no row now did not belong to an in-flight launch.
	var ids []InstanceId
	if err := m.db.Select(&ids, "SELECT id FROM instances WHERE worker = ?;", w.ip); err != nil {
		// Without the rows there is no telling which of the daemon's
		// containers are orphans, so the pass has not established anything.
		m.log.errorf("worker %s: could not read its instance ids to reconcile it: %s", w.ip, err)
		return reconcileIncomplete
	}
	live := make(map[InstanceId]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	orphanContainers, orphanNetworks := findOrphans(containers.Items, networks.Items, live)
	if len(orphanContainers) == 0 && len(orphanNetworks) == 0 {
		return reconcileDone
	}

	// Containers first: a network with endpoints refuses removal.
	removedContainers, result := m.removeOrphans(w, "container", orphanContainers, func(ctx context.Context, cid string) error {
		_, err := w.cli.ContainerRemove(ctx, cid, client.ContainerRemoveOptions{RemoveVolumes: true, Force: true})
		// A conflict is a removal already in progress on the daemon (cmgrd's
		// own, which timed out client-side while the daemon was hung): the
		// container is on its way out just the same.
		if errdefs.IsConflict(err) {
			return nil
		}
		return err
	})
	if result == reconcileUnreachable {
		return result
	}
	removedNetworks, netResult := m.removeOrphans(w, "network", orphanNetworks, func(ctx context.Context, name string) error {
		_, err := w.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
		return err
	})
	result = max(result, netResult)
	m.log.infof("worker %s: removed %d orphaned container(s) and %d orphaned network(s) of instances no longer recorded there",
		w.ip, removedContainers, removedNetworks)
	return result
}

// removeOrphans runs remove for each name, reconcileParallelism at a time and
// each under its own control timeout, and stops handing out work once the
// conn is gone or the daemon has become unreachable. It returns how many are
// gone (a not-found counts) and the worst result of the removals: a refusal
// leaves the pass incomplete, since that orphan still holds its host ports.
func (m *Manager) removeOrphans(w *workerConn, what string, names []string, remove func(context.Context, string) error) (removed int, result reconcileResult) {
	var (
		wg    sync.WaitGroup
		count atomic.Int32
		worst atomic.Int32 // the worst reconcileResult so far
		slots = make(chan struct{}, reconcileParallelism)
	)
	note := func(r reconcileResult) {
		for {
			prev := worst.Load()
			if reconcileResult(prev) >= r || worst.CompareAndSwap(prev, int32(r)) {
				return
			}
		}
	}
	for _, name := range names {
		if w.gone() || reconcileResult(worst.Load()) == reconcileUnreachable {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			defer func() { <-slots }()
			ctx, cancel := m.controlCtx()
			err := remove(ctx, name)
			cancel()
			if err != nil && !errdefs.IsNotFound(err) {
				note(m.reconcileFailed(w, "remove orphaned "+what+" "+name, err))
				return
			}
			count.Add(1)
		}(name)
	}
	wg.Wait()
	return int(count.Load()), reconcileResult(worst.Load())
}

// reconcileFailed logs a failed reconciliation call and reports what it means
// for the pass: unreachable for a connection-level failure, which runWorker
// retries (so it is logged quietly, being expected of a daemon that is still
// starting), incomplete for any other, which is an error of its own.
func (m *Manager) reconcileFailed(w *workerConn, what string, err error) reconcileResult {
	if isTransportError(err) {
		m.log.debugf("worker %s: could not %s while reconciling it: %s", w.ip, what, err)
		return reconcileUnreachable
	}
	m.log.errorf("worker %s: could not %s while reconciling it: %s", w.ip, what, err)
	return reconcileIncomplete
}

// gone reports whether the conn has been removed or replaced.
func (w *workerConn) gone() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// findOrphans picks the containers and networks whose instance has no row on
// this worker. A container is attributed to an instance through the
// cmgr-<id> network it is attached to; one on no such network is left alone,
// as is any network that does not follow cmgr's naming.
func findOrphans(containers []container.Summary, networks []network.Summary, live map[InstanceId]bool) (cids, netnames []string) {
	for _, c := range containers {
		if id, ok := containerInstance(c); ok && !live[id] {
			cids = append(cids, c.ID)
		}
	}
	for _, n := range networks {
		if id, ok := networkInstance(n.Name); ok && !live[id] {
			netnames = append(netnames, n.Name)
		}
	}
	return cids, netnames
}

// networkInstance parses the instance id out of a cmgr-<id> network name,
// the inverse of getNetworkName.
func networkInstance(name string) (InstanceId, bool) {
	digits, ok := strings.CutPrefix(name, networkPrefix)
	if !ok || digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return InstanceId(id), true
}

// containerInstance attributes a container to the instance whose cmgr-<id>
// network it is attached to.
func containerInstance(c container.Summary) (InstanceId, bool) {
	if c.NetworkSettings == nil {
		return 0, false
	}
	for name := range c.NetworkSettings.Networks {
		if id, ok := networkInstance(name); ok {
			return id, true
		}
	}
	return 0, false
}
