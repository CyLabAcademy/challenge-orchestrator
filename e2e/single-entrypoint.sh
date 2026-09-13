#!/bin/sh
# Start the box: dockerd first, then cmgrd beside it on the local socket.
#
# This is the shape a class deployment has and the multi-host fleet never
# does -- one daemon, reached at /var/run/docker.sock with no DOCKER_HOST and
# no certificates. cmgrd runs in the foreground so the container's lifetime is
# the orchestrator's.
set -eu

# The tree, seeded once from the read-only mount, so the scenario can edit a
# challenge and run update. Same contract as cork-entrypoint.sh.
if [ -d /challenges-seed ] && [ -d "${CMGR_DIR:-/challenges}" ] && [ -z "$(ls -A "${CMGR_DIR:-/challenges}")" ]; then
  echo "seeding ${CMGR_DIR:-/challenges} from /challenges-seed"
  cp -a /challenges-seed/. "${CMGR_DIR:-/challenges}/"
fi

# dind's own entrypoint, which sets up cgroups, iptables and storage before
# execing dockerd. Given no --host it appends both the socket and plain tcp
# on 2375; named explicitly, it serves the socket alone, which is all a class
# box has and all anything here uses.
dockerd-entrypoint.sh dockerd --host="${DOCKERD_HOST:-unix:///var/run/docker.sock}" &
DOCKERD_PID=$!

i=0
until docker info >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    echo "dockerd did not come up in 60s" >&2
    exit 1
  fi
  # A dockerd that died is not one worth waiting out.
  kill -0 "$DOCKERD_PID" 2>/dev/null || { echo "dockerd exited while starting" >&2; exit 1; }
  sleep 1
done
echo "dockerd is up on /var/run/docker.sock"

exec "$@"
