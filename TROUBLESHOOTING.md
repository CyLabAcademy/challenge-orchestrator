# Troubleshooting

## Getting Docker working with `firewalld` (Fedora/CentOS/RHEL)
```
firewall-cmd --zone=public --add-masquerade --permanent
firewall-cmd --zone=trusted --change-interface=docker 0 --permanent
firewall-cmd --reload
```

## Receiving Docker network errors

**Symptom:**  Starting of challenge instances fails with the message
"ERROR:  could not create challenge network (cmgr-...): Error response from
daemon:  could not find an available, non-overlapping IPv4 address pool
among the defaults to assign to the network.

**Cause:** Docker has exhausted all available subnets that it has been
assigned and cannot create anymore.  By default, Docker only reserves 31
distinct subnets which constrains `cmgr` to no more than that number of running
challenge instances (each instance gets a network).

**Solution:** Choose a sufficiently large region of RFC 1918 address space
and update the Docker daemon's configuration (`/etc/docker/daemon.json`) to
allot more default networks.  It is important to ensure that these addresses
are not in use by another network segment and that the individual subnets are
large enough to handle any multi-host challenges (to include a solver host and
the default gateway).

An example configuration which carves the address space into \~2 million subnets
is:
```json
{
  "default-address-pools":
    [
      {"base":"10.0.0.0/8", "size":29}
    ]
}
```

**Note:** You will need to restart the daemon after changing its configuration.

## A worker is stuck `unresponsive`

**Symptom:** `cork worker-list` shows a worker as `unresponsive`, with a
duration and a reason, and it is not taking placements.

**What cork is doing about it:** nothing needs doing for a transient fault.
`unresponsive` is cork's own verdict, not an operator's, and the worker keeps
being probed. Once its docker daemon answers again — after a reboot, a
`systemctl restart docker`, a network blip — cork reconnects and reconciles
the box before letting it back into placement. A worker that keeps failing is
retried more slowly each time, so a box that has been ejected repeatedly can
wait a few minutes between attempts.

The reason says which probe failed, and the two mean different things:

- *dockerd did not answer* — the daemon is not reachable at all. If the box is
  up, check `dockerd` and the TLS material; if it is not, that is the fault.
- *dockerd answered /_ping but could not list containers* — the API is alive
  while containerd or the container store is wedged. A daemon restart is the
  usual fix; a reboot if that hangs.

A worker whose telemetry agent alone has died shows `load: unknown` and stays
in placement, which is deliberate — it is almost always still serving.

`worker-list` does not say why, because `reason` carries the reachable axis
only. The corkd log does, once, as the axis moves:

```
worker 10.0.0.2: telemetry stopped answering, load unknown: <cause>
```

The cause is worth reading before acting, because the three differ:

- *connection refused* — the agent is not running. Restart it.
- *context deadline exceeded* / a timeout — the agent is wedged, or the box is
  too loaded to answer a request that serves a cached value. Look at the box,
  not just the unit.
- *telemetry returned status 503* — the agent is running and declining to give
  a verdict. Almost always this means the box is crossing the CPU high mark
  without holding it: the agent will not call a box healthy while it cannot yet
  tell a sustained trip from a spike, so a worker that hovers near the mark logs
  this a few times an hour. It is a signal about the box, not the agent — look
  at what is running there. A genuinely fresh agent shows it for well under a
  second.

  It is *not* a sign of a hardened systemd unit. One that hides `/proc/stat`
  kills the agent at startup instead — it is read before the sampling loop,
  under a fatal — so that shows up as *connection refused* above, with
  `reading /proc/stat` in `journalctl -u telemetry` on every restart.

**When to intervene:** if the duration keeps growing and the box is healthy,
look at the reason first. `cork worker-add <ip>` forces an immediate
reconnect-and-reconcile rather than waiting out the backoff. Given no public
address it keeps the one already stored, so it is safe to run on a worker that
is already registered — which is the only way it is ever used here.

Do **not** reach for `cork worker-down` on the way to a reboot. It is asserted
rather than observed, so no probe lifts it, and the box would come back
healthy and sit out of the fleet until someone noticed. A worker that is
merely rebooting is already handled.

## A worker was reimaged rather than rebooted

**Symptom:** a worker rejoined the fleet looking healthy, but challenges it
was hosting serve nothing. Persistent instances stay missing; `cork
worker-list` still counts them against the worker.

**Cause:** cork's instance records survived, and the containers did not. A
reboot preserves both (challenge containers restart with the box), so cork
treats a box that comes back as a box that still holds what it held. A reimage,
a wiped docker data directory, or a manual `docker system prune` breaks that
assumption, and cork has no way to tell the two apart.

On-demand instances clear themselves when their TTL fires — the stop tolerates
the containers being gone. Persistent (schema-managed) instances do not: the
converge counts the surviving record as a live instance and launches nothing.

**Solution:** purge the records and relaunch.

```
cork worker-remove <ip>
cork worker-add <ip> [<public address>]
cork update-schema <schema.yaml>
```

`worker-remove` is the step that matters — it deletes the instance rows. A
`worker-add` alone leaves them in place, and the converge will still count
them.
