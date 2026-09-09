#!/usr/bin/env bash
#
# The cork launch sequence, end to end, against the compose fleet.
#
# Everything an operator or the platform would do goes through cmgrd's HTTP
# API: cmgrd-cli for the operator steps, curl for the platform ones. The
# workers' dockerds, the builder, and the registry are only ever read, with
# cork's own client certificates, to prove that what cmgrd reports really
# happened on the other boxes. The chaos steps additionally use the outer
# docker socket to stop or replace a telemetry sidecar, pause a worker's
# dockerd, and restart cork; without the socket (or with E2E_CHAOS=0) the
# cmgrd-restart, telemetry-silence, overloaded, all-overloaded, and
# hung-dockerd steps are skipped and say so.
#
# Runs inside the "e2e" compose service (see compose.yaml). Knobs:
#   CMGRD_SERVER         cmgrd base URL                            http://cork:4200
#   E2E_WORKERS          "<private ip>=<public name>" list         172.28.0.11=worker-a ...
#                        (the telemetry sidecar is the compose service <name>-telemetry)
#   E2E_SCHEMA           schema to converge                        /opt/e2e/schema.yaml
#   E2E_REGISTRY         registry address, as in CMGR_REGISTRY     zot.internal
#   E2E_BUILDER          the builder daemon's plain-TCP API        http://builder:2375
#   E2E_SCHEMA_FULL      schema converged by full mode only        /opt/e2e/schema-full.yaml
#   E2E_CHAOS            1 to run the outer-socket chaos steps     1
#   E2E_FULL             1 to add the full-mode steps (run.sh --full) 0
#   E2E_COMPOSE_PROJECT  compose project name, for those steps     cork-e2e
#   E2E_IMAGE            image to run the fake telemetry from      cork-e2e/cork
#   DOCKER_CERT_PATH     cork's worker client certificates         /root/.docker_certs
set -euo pipefail

CMGRD_SERVER="${CMGRD_SERVER:-http://cork:4200}"
E2E_WORKERS="${E2E_WORKERS:-172.28.0.11=worker-a 172.28.0.12=worker-b}"
E2E_SCHEMA="${E2E_SCHEMA:-/opt/e2e/schema.yaml}"
E2E_SCHEMA_FULL="${E2E_SCHEMA_FULL:-/opt/e2e/schema-full.yaml}"
E2E_REGISTRY="${E2E_REGISTRY:-zot.internal}"
E2E_BUILDER="${E2E_BUILDER:-http://builder:2375}"
E2E_CHAOS="${E2E_CHAOS:-1}"
E2E_FULL="${E2E_FULL:-0}"
E2E_COMPOSE_PROJECT="${E2E_COMPOSE_PROJECT:-cork-e2e}"
E2E_IMAGE="${E2E_IMAGE:-cork-e2e/cork}"
DOCKER_CERT_PATH="${DOCKER_CERT_PATH:-/root/.docker_certs}"
REGISTRY_CERT_DIR="${CMGR_REGISTRY_CERT_DIR:-/etc/docker/certs.d/$E2E_REGISTRY}"
WORKER_SERVERNAME=academy-docker-worker # WORKER_SERVERNAME in cmgr/workers.go
DOCKER_SOCK=/var/run/docker.sock
CHALLENGES=/challenges                  # CMGR_DIR, shared with cork
CHALLENGES_SEED=/challenges-seed        # pristine copy, to undo the edits
export CMGRD_SERVER # picked up by cmgrd-cli

# The challenges in schema.yaml, by the role each plays here.
SCHEMA_NAME=e2e
CH_PERSISTENT=cmgr/examples/custom-socat   # service, instance_count 1
CH_ONDEMAND=cmgr/examples/runtime-env-vars # service, instance_count -1, answers over HTTP
CH_MAKE=cmgr/examples/binex101             # service, instance_count -1, remote-make over TCP
CH_FLAGONLY=cmgr/examples/layer-cake       # flag_only, never launched
PERSISTENT_SRC=custom/Dockerfile           # files the update steps edit, under CMGR_DIR
ONDEMAND_SRC=runtime_env_vars/server.py
MAKE_META=remote-make/problem.md           # metadata only: the container-options step appends a "## Challenge Options" block here
PORT_LOW=20000                             # CMGR_PORTS on cork
PORT_HIGH=20029
# CMGR_WORKER_LAUNCH_WAIT on cork, in milliseconds. compose.yaml derives both
# this and cmgrd's own setting from one value, so the burst steps below measure
# themselves against the wait the daemon is really running with.
LAUNCH_WAIT_MS="${E2E_LAUNCH_WAIT_MS:-500}"
LAUNCH_WAIT_H="${LAUNCH_WAIT_MS}ms"        # how it reads in a message

# ---------------------------------------------------------------- helpers

T0=$(date +%s)
# Skipped steps are counted, not just printed: without the outer docker
# socket five of the twenty steps are the only coverage of worker health and
# the restart path, and a run that quietly drops them must not report the
# same verdict as one that ran them.
SKIPPED=0
SKIPPED_NAMES=()
# A step left out because the mode did not ask for it is not the same as one
# that could not run: the first is a choice, the second is a hole. Counting
# them together would make the verdict say nothing.
FULL=0
if [[ "$E2E_FULL" == 1 ]]; then FULL=1; fi
DESELECTED=0
DESELECTED_NAMES=()
step() { printf '\n=== [%4ds] %s\n' "$(( $(date +%s) - T0 ))" "$*"; }
ok() { printf '  ok   %s\n' "$*"; }
note() { printf '  --   %s\n' "$*"; }
skip() { SKIPPED=$((SKIPPED + 1)); SKIPPED_NAMES+=("$*"); printf '  skip %s\n' "$*"; }
# deselect <what>: a step this mode does not run. Regular runs print one line
# per full-mode step so the difference between the modes is visible in the
# transcript, not just in the documentation.
deselect() { DESELECTED=$((DESELECTED + 1)); DESELECTED_NAMES+=("$*"); printf '  --   full mode only: %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }
quiet() { "$@" >/dev/null 2>&1; }

# retry <seconds> <what> <cmd...>: poll once a second until cmd succeeds.
retry() {
  local timeout=$1 what=$2
  shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@"; do
    (( $(date +%s) < deadline )) || fail "timed out after ${timeout}s waiting for $what"
    sleep 1
  done
}

# cmgrd's API, the platform's view. api prints the body and fails on a
# non-2xx status; api_status prints only the status code; api_headers prints
# the response headers. Every curl here carries a timeout so retry's deadline
# can fire on a hung daemon; launches get a long one because a cold image
# pull on the worker happens inside the request.
API_TIMEOUT=600
api() { # api <method> <path> [json body]
  local method=$1 path=$2
  shift 2
  local data=()
  if (( $# )); then data=(-d "$1"); fi
  curl -sS --fail-with-body --connect-timeout 5 --max-time "$API_TIMEOUT" \
    -X "$method" -H 'Content-Type: application/json' "${data[@]}" "$CMGRD_SERVER$path"
}
api_status() {
  local method=$1 path=$2
  shift 2
  local data=()
  if (( $# )); then data=(-d "$1"); fi
  curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time "$API_TIMEOUT" \
    -X "$method" -H 'Content-Type: application/json' "${data[@]}" "$CMGRD_SERVER$path"
}
api_headers() {
  curl -sS -o /dev/null -D - --connect-timeout 5 --max-time "$API_TIMEOUT" \
    -X "$1" -H 'Content-Type: application/json' "$CMGRD_SERVER$2"
}

# A worker's dockerd, exactly as cmgrd dials it: cork's client certificate,
# and the dialed IP verified against the shared server name (--resolve does
# for curl what the pinned ServerName does in workers.go).
worker_curl() { # worker_curl <ip> <path> [curl args...]
  local ip=$1 path=$2
  shift 2
  curl -sS --connect-timeout 5 --max-time 15 \
    --cacert "$DOCKER_CERT_PATH/ca.pem" --cert "$DOCKER_CERT_PATH/cert.pem" --key "$DOCKER_CERT_PATH/key.pem" \
    --resolve "$WORKER_SERVERNAME:2376:$ip" "$@" "https://$WORKER_SERVERNAME:2376$path"
}
worker_api() { local ip=$1 path=$2; shift 2; worker_curl "$ip" "$path" --fail-with-body "$@"; }
worker_status() { local ip=$1 path=$2; shift 2; worker_curl "$ip" "$path" -o /dev/null -w '%{http_code}' "$@"; }

# The registry, with cork's read-write identity from certs.d.
registry_curl() {
  local path=$1
  shift
  curl -sS --connect-timeout 5 --max-time 15 \
    --cacert "$REGISTRY_CERT_DIR/ca.crt" --cert "$REGISTRY_CERT_DIR/client.cert" \
    --key "$REGISTRY_CERT_DIR/client.key" "$@" "https://$E2E_REGISTRY$path"
}
registry_api() { local path=$1; shift; registry_curl "$path" --fail-with-body "$@"; }
registry_status() { local path=$1; shift; registry_curl "$path" -o /dev/null -w '%{http_code}' "$@"; }
registry_tags() { # registry_tags <repo>: one tag per line, nothing for a missing repo
  local code
  code=$(registry_status "/v2/$1/tags/list")
  if [[ "$code" == 200 ]]; then registry_api "/v2/$1/tags/list" | jq -r '.tags // [] | .[]'; fi
}

# The builder daemon (cork's local docker), plain TCP inside the network.
builder_tags() { curl -sS --fail-with-body --connect-timeout 5 --max-time 15 "$E2E_BUILDER/images/json" | jq -r '.[].RepoTags // [] | .[]'; }
worker_tags() { worker_api "$1" /images/json | jq -r '.[].RepoTags // [] | .[]'; }
# builder_untag <image ref>: drop one tag from the builder daemon and print the
# HTTP status, the way --prune-old would have. Only the full-mode update step
# that deliberately orphans a generation needs it: cork never reclaims such a
# tag, so the harness must, or the teardown's "the builder holds no challenge
# images" check trips over it.
builder_untag() {
  curl -sS --connect-timeout 5 --max-time 30 -o /dev/null -w '%{http_code}' \
    -X DELETE "$E2E_BUILDER/images/$1"
}
# builder_pull <repo> <tag>: pull an image onto the builder daemon.
#
# A fixture that deletes a registry tag needs a local copy to restore it from,
# and it may not assume the builder still has one: with CMGR_PURGE_AFTER_PUSH on
# (production's default) cork drops its copy the moment the push succeeds. The
# create endpoint answers 200 and reports failures inside the stream, so the
# body is what says whether this worked.
builder_pull() {
  local body
  body=$(curl -sS --connect-timeout 5 --max-time 180 -X POST -H 'X-Registry-Auth: e30=' \
    "$E2E_BUILDER/images/create?fromImage=$1&tag=$2" 2>&1) || return 1
  ! grep -q '"error"' <<<"$body"
}

# has_line <line> <cmd...>: whether the command's output contains exactly that
# line. Captures first: `cmd | grep -q` under pipefail fails spuriously when
# grep exits before the producer has finished writing.
has_line() {
  local line=$1 out
  shift
  out=$("$@")
  grep -qx -- "$line" <<<"$out"
}

# The outer docker daemon, for the chaos steps.
outer() { local path=$1; shift; curl -sS --fail-with-body --connect-timeout 5 --max-time 60 --unix-socket "$DOCKER_SOCK" "$@" "http://docker$path"; }
have_outer() { [[ "$E2E_CHAOS" == 1 && -S "$DOCKER_SOCK" ]] && quiet outer /_ping; }
compose_container() { # compose_container <service>: the outer container id of a compose service
  local filters
  filters=$(jq -rn --arg p "$E2E_COMPOSE_PROJECT" --arg s "$1" \
    '{label: ["com.docker.compose.project=\($p)", "com.docker.compose.service=\($s)"]} | tostring | @uri')
  outer "/containers/json?all=true&filters=$filters" | jq -r '.[0].Id // empty'
}
outer_ctl() { outer "/containers/$1/$2" -X POST -o /dev/null; } # outer_ctl <id> stop|start|pause|unpause|restart

# Chaos state the EXIT trap undoes if the run dies mid-step, so the fleet is
# usable (and the next run does not stall) after a failure.
PAUSED_WORKER=""      # outer container id of a paused worker
STOPPED_SIDECAR=""    # outer container id of a stopped telemetry sidecar
DOWNED_WORKER=""      # a worker this run marked down and still owes a worker-add
DRAINED_WORKER=""     # a worker purged or PATCHed down for a scale-in drain
TRAFFIC_STOP=""       # stop file of the live-traffic launchers
PHANTOM_IP=""         # a worker registered at this container's address, whose dockerd is not listening
PHANTOM_RESPONDER=""  # pid of the loop answering telemetry for it
declare -A FAKES=()   # fake telemetry container id -> the sidecar it replaced

# The incomplete-reconcile step plants a container the pass cannot see (it
# carries no cmgr.managed label) holding a cmgr-<id> network open. Nothing
# label-scoped -- not reconcileWorker, not sweep_worker -- can clear it, so the
# names are fixed and known to both.
BLOCKER_NAME=e2e-reconcile-blocker
BLOCKER_NET=cmgr-997
BLOCKER_WORKER=""     # worker ip currently holding that blocker, if any

# Full-mode state. A step plants an orphan cmgrd is expected to reclaim later
# in the same step; until it does, the run owes the worker its removal, since a
# cmgr-<id> network left behind makes every later worker-add on that box burn
# its whole reconcile budget before it can take placements.
ORPHAN_WORKER=""      # worker ip holding the planted orphan
ORPHAN_NET=""         # its cmgr-<id> network
ORPHAN_CID=""         # the container holding that network open
FRESH_CMGRD=""        # pid of the throwaway cmgrd of the fresh-box step
EDITED_META=""        # a challenge metadata file a step edited under CMGR_DIR
ORPHAN_TAG=""         # a content tag an update deliberately orphaned, which cork never reclaims
STOPPED_REGISTRY=""   # outer container id of the registry this run stopped
STALL_REGISTRY=""     # outer container id of the stand-in that accepts and never answers
PROBE_BUILD=""        # a build made by hand that still owes a destroy
# The registry-identity step tags a challenge image on a worker under a
# throwaway repo and offers it to zot with the worker's own read-only
# certificate. Both names are fixed so cleanup() can untag it if the run dies
# between the tag and the delete.
PROBE_REPO=cmgr/examples/e2e-probe
PROBE_TAG=worker-write
PROBE_WORKER=""       # worker ip holding $E2E_REGISTRY/$PROBE_REPO:$PROBE_TAG
MAKE_TAG_DELETED=""   # a $CH_MAKE tag this run deleted from zot and still owes a re-push
MULTI_SCHEMA_ADDED="" # a second schema this run added and still owes a remove-schema
MULTI_INST=""         # its multi-container instance, if the run dies between launch and stop
DB_LOCK_PID=""        # a sqlite3 holding cmgrd's write lock open on purpose
DB_LOCK_FD=""         # the write end of the fifo feeding it, kept open by this shell
cleanup() {
  local status=$? fake
  # Before anything else: while the stand-in writer holds cmgrd's write
  # lock, every API call below would be refused as a busy database.
  if [[ -n "$DB_LOCK_FD" ]]; then exec {DB_LOCK_FD}>&- 2>/dev/null || true; DB_LOCK_FD=""; fi
  if [[ -n "$DB_LOCK_PID" ]]; then kill "$DB_LOCK_PID" >/dev/null 2>&1 || true; fi
  if [[ -n "$TRAFFIC_STOP" ]]; then touch "$TRAFFIC_STOP" 2>/dev/null || true; fi
  # Before anything that needs a daemon: it cannot fail, and it is what
  # unblocks the NEXT run, whose seed diff would otherwise read this step's
  # leftovers as a stale volume.
  if [[ -n "$EDITED_META" ]]; then cp "$CHALLENGES_SEED/$EDITED_META" "$CHALLENGES/$EDITED_META" 2>/dev/null || true; fi
  if [[ -n "$PROBE_WORKER" ]]; then
    worker_status "$PROBE_WORKER" "/images/$E2E_REGISTRY/$PROBE_REPO:$PROBE_TAG?noprune=1" -X DELETE >/dev/null 2>&1 || true
  fi
  if [[ -n "$MAKE_TAG_DELETED" ]]; then
    # Push it back from the builder, which holds the image and cork's
    # read-write certs.d identity: a run that died mid-step must not leave the
    # registry one tag short of what it was handed.
    curl -sS --connect-timeout 5 --max-time 180 -X POST -H 'X-Registry-Auth: e30=' \
      "$E2E_BUILDER/images/$E2E_REGISTRY/$CH_MAKE/push?tag=$MAKE_TAG_DELETED" >/dev/null 2>&1 || true
    MAKE_TAG_DELETED=""
  fi
  if [[ -n "$PAUSED_WORKER" ]]; then outer_ctl "$PAUSED_WORKER" unpause >/dev/null 2>&1 || true; fi
  for fake in "${!FAKES[@]}"; do
    outer_ctl "$fake" stop >/dev/null 2>&1 || true
    outer_ctl "${FAKES[$fake]}" start >/dev/null 2>&1 || true
  done
  if [[ -n "$STOPPED_SIDECAR" ]]; then outer_ctl "$STOPPED_SIDECAR" start >/dev/null 2>&1 || true; fi
  if [[ -n "$BLOCKER_WORKER" ]]; then
    # Container first: while it runs the network refuses removal, and a
    # leftover $BLOCKER_NET makes every later worker-add on that box burn the
    # whole reconcile budget before it can take placements.
    worker_status "$BLOCKER_WORKER" "/containers/$BLOCKER_NAME?force=true" -X DELETE >/dev/null 2>&1 || true
    worker_status "$BLOCKER_WORKER" "/networks/$BLOCKER_NET" -X DELETE >/dev/null 2>&1 || true
  fi
  if [[ -n "$ORPHAN_WORKER" ]]; then
    # Container first, as for the blocker above: while it runs, its network
    # refuses removal.
    worker_status "$ORPHAN_WORKER" "/containers/$ORPHAN_CID?force=true" -X DELETE >/dev/null 2>&1 || true
    worker_status "$ORPHAN_WORKER" "/networks/$ORPHAN_NET" -X DELETE >/dev/null 2>&1 || true
  fi
  if [[ -n "$FRESH_CMGRD" ]]; then kill "$FRESH_CMGRD" >/dev/null 2>&1 || true; fi
  # The stand-in holds the registry's own network alias, so it lets go first;
  # the probe build is destroyed last, so its registry untag has a registry to
  # talk to. t=1 because the stand-in's entrypoint is a pipeline and bash
  # swallows the SIGTERM.
  if [[ -n "$STALL_REGISTRY" ]]; then outer "/containers/$STALL_REGISTRY/stop?t=1" -X POST -o /dev/null >/dev/null 2>&1 || true; fi
  if [[ -n "$STOPPED_REGISTRY" ]]; then outer_ctl "$STOPPED_REGISTRY" start >/dev/null 2>&1 || true; fi
  if [[ -n "$ORPHAN_TAG" ]]; then
    # Neither a later --prune-old nor remove-schema's destroyImages reclaims
    # it, so a run that died here would leave the builder holding a challenge
    # image the next run's teardown reads as a leak.
    registry_status "/v2/$CH_ONDEMAND/manifests/${ORPHAN_TAG##*:}" -X DELETE >/dev/null 2>&1 || true
    builder_untag "$ORPHAN_TAG" >/dev/null 2>&1 || true
  fi
  if [[ -n "$MULTI_INST" ]]; then api_status DELETE "/instances/$MULTI_INST" >/dev/null 2>&1 || true; fi
  # remove-schema stops what it started and untags its images, so this also
  # clears the registry tags a run that died mid-step would otherwise leave
  # for the next run's teardown to read as a leak.
  if [[ -n "$MULTI_SCHEMA_ADDED" ]]; then timeout 120 cmgrd-cli remove-schema "$MULTI_SCHEMA_ADDED" >/dev/null 2>&1 || true; fi
  if [[ -n "$PROBE_BUILD" ]]; then timeout 60 cmgrd-cli destroy "$PROBE_BUILD" >/dev/null 2>&1 || true; fi
  if [[ -n "$PHANTOM_IP" ]]; then cmgrd-cli worker-remove "$PHANTOM_IP" >/dev/null 2>&1 || true; fi
  if [[ -n "$PHANTOM_RESPONDER" ]]; then
    kill "$PHANTOM_RESPONDER" >/dev/null 2>&1 || true
    curl -s --max-time 2 http://127.0.0.1:2136/ >/dev/null 2>&1 || true
  fi
  if [[ -n "$DOWNED_WORKER" ]]; then cmgrd-cli worker-add "$DOWNED_WORKER" "${PUBLIC[$DOWNED_WORKER]}" >/dev/null 2>&1 || true; fi
  if [[ -n "$DRAINED_WORKER" ]]; then cmgrd-cli worker-add "$DRAINED_WORKER" "${PUBLIC[$DRAINED_WORKER]}" >/dev/null 2>&1 || true; fi
  if (( status != 0 )); then printf '\nSCENARIO FAILED (exit %d) after %ds\n' "$status" "$(( $(date +%s) - T0 ))" >&2; fi
}
trap cleanup EXIT

# Fake an overloaded worker: stop its telemetry sidecar and run, in the
# worker's network namespace, a responder that always says overloaded.
# Leaves the fake's container id in LAST_FAKE (a subshell could not register
# it for cleanup).
LAST_FAKE=""
fake_overload() { # fake_overload <worker ip>
  local name=${PUBLIC[$1]} sidecar worker fake body
  sidecar=$(compose_container "$name-telemetry")
  worker=$(compose_container "$name")
  [[ -n "$sidecar" && -n "$worker" ]] || fail "compose containers for $name not found"
  outer_ctl "$sidecar" stop
  body=$(jq -cn --arg img "$E2E_IMAGE" --arg net "container:$worker" '{
    Image: $img,
    Entrypoint: ["bash", "-c",
      "resp=$(printf \"HTTP/1.1 200 OK\\r\\nContent-Type: application/json\\r\\nContent-Length: 19\\r\\nConnection: close\\r\\n\\r\\n{\\\"overloaded\\\":true}\"); while :; do printf \"%s\" \"$resp\" | nc -l -q 0 2136 >/dev/null 2>&1; done"],
    HostConfig: {NetworkMode: $net, AutoRemove: true}}')
  fake=$(outer /containers/create -X POST -H 'Content-Type: application/json' -d "$body" | jq -r .Id)
  FAKES[$fake]=$sidecar
  outer_ctl "$fake" start
  LAST_FAKE=$fake
}
unfake_overload() { # unfake_overload <fake id>: back to the real sidecar
  outer_ctl "$1" stop
  outer_ctl "${FAKES[$1]}" start
  unset 'FAKES[$1]'
}

# fake_hung_telemetry <worker ip>: an agent that accepts every poll and answers
# none. Same shape and order as fake_overload -- stop the real sidecar, run a
# responder in the worker's own network namespace, leave the id in LAST_FAKE
# and the FAKES entry for unfake_overload and the EXIT trap -- but the
# responder writes nothing at all, so only cmgrd's per-poll timeout ends the
# request. -k keeps the listener open so a poll arriving while another is held
# still completes its handshake; -q -1 stops nc quitting on the EOF its stdin
# starts at, so the connection is held rather than closed; the plain listener
# behind the || is the fallback for an nc without -k.
fake_hung_telemetry() { # fake_hung_telemetry <worker ip>
  local name=${PUBLIC[$1]} sidecar worker fake body
  sidecar=$(compose_container "$name-telemetry")
  worker=$(compose_container "$name")
  [[ -n "$sidecar" && -n "$worker" ]] || fail "compose containers for $name not found"
  outer_ctl "$sidecar" stop
  body=$(jq -cn --arg img "$E2E_IMAGE" --arg net "container:$worker" '{
    Image: $img,
    Entrypoint: ["bash", "-c",
      "while :; do nc -l -k -q -1 2136 >/dev/null 2>&1 || nc -l -q -1 2136 >/dev/null 2>&1 || sleep 0.2; done"],
    HostConfig: {NetworkMode: $net, AutoRemove: true}}')
  fake=$(outer /containers/create -X POST -H 'Content-Type: application/json' -d "$body" | jq -r .Id)
  [[ -n "$fake" && "$fake" != null ]] || fail "could not create the wedged telemetry responder for $name"
  FAKES[$fake]=$sidecar
  outer_ctl "$fake" start
  LAST_FAKE=$fake
}

# telemetry_hangs <worker ip>: a poll of that worker's agent hangs rather than
# being refused. The exit code is the whole point: curl 28 is "the connection
# was accepted and nothing came back", curl 7 is "nothing is listening" -- the
# state the telemetry-silence step produces, and the one the wedged-telemetry
# step must never be passing on.
telemetry_hangs() {
  local rc=0
  curl -sS -o /dev/null --max-time 2 "http://$1:2136/health" >/dev/null 2>&1 || rc=$?
  (( rc == 28 ))
}

# down_worker <worker ip>: cmgrd-cli worker-down, verified at once. The PATCH
# is handled synchronously and markWorkerDown swaps the health unconditionally,
# so the very next read must say down: a poll already in flight cannot put the
# worker back, because pollerSetHealth stores its verdict with a
# compare-and-swap that refuses to leave down. This used to need a retry —
# cork really did have that race — so a single call is the assertion.
down_worker() {
  cmgrd-cli worker-down "$1"
  health_is "$1" down ||
    fail "worker $1 is not down on the read right after worker-down: an in-flight telemetry poll overwrote it (the race fixed in ab72f5d)"
}

# readd <worker ip>: the recovery path for a down or purged worker.
readd() {
  cmgrd-cli worker-add "$1" "${PUBLIC[$1]}"
  retry 30 "worker $1 to come back ok" health_is "$1" ok
}

worker_health() { api GET /workers | jq -r --arg ip "$1" '.[] | select(.ip==$ip) | .health'; }
health_is() { [[ "$(worker_health "$1")" == "$2" ]]; }
# worker_instances <worker ip>: the per-worker instance count from GET
# /workers, which is the signal an autoscaling drain polls. Empty for a worker
# that is not registered.
worker_instances() { api GET /workers | jq -r --arg ip "$1" '.[] | select(.ip==$ip) | .instances'; }

# launch <build id> <user id> <custom var>: POST /builds/<id> as the platform does.
launch() {
  api POST "/builds/$1" "$(jq -cn --arg u "$2" --arg v "$3" '{user_id: $u, env: {CUSTOM_VAR: $v}}')"
}
# try_launch <build id> <user id> <custom var>: like launch, but prints the
# HTTP status on the first line and the body after it, whatever the outcome.
try_launch() {
  curl -sS --connect-timeout 5 --max-time "$API_TIMEOUT" -w '\n%{http_code}' \
    -X POST -H 'Content-Type: application/json' \
    -d "$(jq -cn --arg u "$2" --arg v "$3" '{user_id: $u, env: {CUSTOM_VAR: $v}}')" "$CMGRD_SERVER/builds/$1" |
    { body=$(cat); printf '%s\n%s\n' "${body##*$'\n'}" "${body%$'\n'*}"; }
}

# timed_launch <dir> <tag> <build id> <json body>: one launch with the whole
# response on disk -- <tag>.code holds "<status> <seconds>", <tag>.body the
# body, <tag>.head the response headers. It takes a prebuilt body rather than
# building one, so a burst of these spawns as fast as bash can fork; it
# asserts nothing and records curl's own failure as status 000 rather than
# raising it, so they can run in the background under set -e without one
# refusal or one lost connection killing the run.
timed_launch() {
  local dir=$1 tag=$2 build=$3 body=$4
  curl -sS --connect-timeout 5 --max-time "$API_TIMEOUT" \
    -o "$dir/$tag.body" -D "$dir/$tag.head" -w '%{http_code} %{time_total}\n' \
    -X POST -H 'Content-Type: application/json' -d "$body" \
    "$CMGRD_SERVER/builds/$build" >"$dir/$tag.code" 2>>"$dir/curl.err" ||
    printf '000 0\n' >"$dir/$tag.code"
}

# On-demand instance bookkeeping: track/forget keep OD_IDS and the per-instance
# maps in step, so later steps and the teardown know what is live and where.
declare -A INST_WORKER=() INST_USER=() INST_META=()
OD_IDS=()
track() { # track <instance json> <user index>
  local id
  id=$(jq -r .id <<<"$1")
  OD_IDS+=("$id")
  INST_WORKER[$id]=$(jq -r .worker <<<"$1")
  INST_USER[$id]=$2
  INST_META[$id]=$1
}
forget() { # forget <instance id>
  local keep=() id
  for id in "${OD_IDS[@]}"; do
    if [[ "$id" != "$1" ]]; then keep+=("$id"); fi
  done
  OD_IDS=("${keep[@]}")
}
instance_on() { # instance_on <worker ip>: a tracked instance living there, or nothing
  local id
  for id in "${OD_IDS[@]}"; do
    if [[ "${INST_WORKER[$id]}" == "$1" ]]; then echo "$id"; return; fi
  done
}

# ensure_instance_on <worker ip> <user index>: make sure a tracked on-demand
# instance lives on the worker. Round robin over two ok workers needs at most
# two launches; needing more means placement is broken.
ensure_instance_on() {
  local attempt
  for attempt in 1 2; do
    if [[ -n "$(instance_on "$1")" ]]; then return; fi
    track "$(launch "$OD_BUILD" "e2e-user-$2" "e2e-value-$2")" "$2"
  done
  [[ -n "$(instance_on "$1")" ]] || fail "two launches did not place an instance on $1"
}

containers_on() { worker_api "$1" '/containers/json?all=true' | jq -r '.[].Id'; }

# assert_on_worker <ip> <instance json>: every container cmgrd recorded for
# the instance exists on that worker's dockerd.
assert_on_worker() {
  local present cid
  present=$(containers_on "$1")
  for cid in $(jq -r '.containers[]' <<<"$2"); do
    if ! grep -qx "$cid" <<<"$present"; then
      fail "container $cid of instance $(jq -r .id <<<"$2") is not on worker $1"
    fi
  done
}

# assert_gone_from_worker <ip> <instance id> <instance json>: the instance's
# containers and network are gone from that worker's dockerd.
assert_gone_from_worker() {
  local present cid
  present=$(containers_on "$1")
  for cid in $(jq -r '.containers[]' <<<"$3"); do
    if grep -qx "$cid" <<<"$present"; then
      fail "container $cid of instance $2 is still on worker $1"
    fi
  done
  if [[ "$(worker_status "$1" "/networks/cmgr-$2")" != 404 ]]; then
    fail "network cmgr-$2 is still on worker $1"
  fi
}

# reap <ip> <instance id> <instance json>: what docker-reaper does in
# production for containers cmgrd could not or did not tear down.
container_gone() { [[ "$(worker_status "$1" "/containers/$2/json")" == 404 ]]; }

# orphan_gone <worker ip> <instance id> <instance json>: none of the
# instance's containers and not its network are left on the worker, which is
# what cmgrd's reconciliation of a re-added (or, at startup, every) worker
# must achieve for instances it no longer records there.
orphan_gone() {
  local cid
  for cid in $(jq -r '.containers[]' <<<"$3"); do
    container_gone "$1" "$cid" || return 1
  done
  [[ "$(worker_status "$1" "/networks/cmgr-$2")" == 404 ]]
}

# plant_orphan <worker ip> <instance id>: a cmgr-managed container on a
# cmgr-<id> network that no record refers to, as a DB-only stop leaves
# behind. Prints the container id.
plant_orphan() {
  local ip=$1 id=$2 code cid image
  image="$E2E_REGISTRY/$CH_ONDEMAND:${IMAGE_TAG[$CH_ONDEMAND]}"
  code=$(worker_status "$ip" /networks/create -X POST -H 'Content-Type: application/json' -d "{\"Name\":\"cmgr-$id\",\"Driver\":\"bridge\"}")
  [[ "$code" == 201 ]] || fail "could not plant network cmgr-$id on $ip: HTTP $code"
  cid=$(worker_api "$ip" /containers/create -X POST -H 'Content-Type: application/json' \
    -d "{\"Image\":\"$image\",\"Labels\":{\"cmgr.managed\":\"true\"},\"HostConfig\":{\"NetworkMode\":\"cmgr-$id\"}}" | jq -r .Id)
  [[ -n "$cid" && "$cid" != null ]] || fail "could not plant a container from $image on $ip"
  code=$(worker_status "$ip" "/containers/$cid/start" -X POST)
  [[ "$code" == 204 ]] || fail "could not start the planted container on $ip: HTTP $code"
  [[ "$(worker_api "$ip" "/containers/$cid/json" | jq -r .State.Running)" == true ]] || fail "the planted container on $ip is not running"
  echo "$cid"
}
reap() {
  local cid code
  for cid in $(jq -r '.containers[]' <<<"$3"); do
    # 409: a removal is already in progress, e.g. cmgrd's own force-remove
    # that timed out client-side but is still running on a thawed daemon.
    code=$(worker_status "$1" "/containers/$cid?force=true" -X DELETE)
    case "$code" in
      204|404|409) ;;
      *) fail "could not remove container $cid on worker $1: HTTP $code" ;;
    esac
    retry 30 "container $cid to leave worker $1" container_gone "$1" "$cid"
  done
  # A network goes only once its last endpoint does, and dockerd can hold one
  # for a moment after the container is gone -- the same race $BLOCKER_NET is
  # retried for in the incomplete-reconcile step. A bare --fail-with-body
  # DELETE would abort the run on it with no message.
  if [[ "$(worker_status "$1" "/networks/cmgr-$2")" == 200 ]]; then
    retry 15 "the cmgr-$2 network to leave worker $1" reap_net_gone "$1" "$2"
  fi
}
# reap_net_gone <worker ip> <instance id>: one attempt at removing a reaped
# instance's network; 404 counts, it is already gone.
reap_net_gone() {
  local code
  code=$(worker_status "$1" "/networks/cmgr-$2" -X DELETE)
  [[ "$code" == 204 || "$code" == 404 ]]
}

# sweep_worker <ip>: docker-reaper's job at the start of a run. Containers
# and networks a failed earlier run left on a worker would otherwise collide
# with reused instance ids (a fresh database hands out the same numbers).
sweep_worker() {
  local ip=$1 n=0 cid net filters
  filters=$(jq -rn '{label: ["cmgr.managed=true"]} | tostring | @uri')
  for cid in $(worker_api "$ip" "/containers/json?all=true&filters=$filters" | jq -r '.[].Id'); do
    worker_status "$ip" "/containers/$cid?force=true" -X DELETE >/dev/null
    n=$(( n + 1 ))
  done
  for net in $(worker_api "$ip" /networks | jq -r '.[] | select(.Name | startswith("cmgr-")) | .Name'); do
    worker_status "$ip" "/networks/$net" -X DELETE >/dev/null
    n=$(( n + 1 ))
  done
  if (( n > 0 )); then note "swept $n leftover cmgr container(s)/network(s) from worker $ip"; fi
}

# assert_gone <instance id> <ip> <instance json>: gone from cmgrd and the worker.
assert_gone() {
  [[ "$(api_status GET "/instances/$1")" == 404 ]] || fail "instance $1 is still known to cmgrd"
  assert_gone_from_worker "$2" "$1" "$3"
}

# assert_torn_down <instance id>: what a rebuild does to an on-demand
# instance: it cannot be restarted without its original launch payload, so
# cmgrd removes its containers and network on the worker and its record,
# exactly like a stop.
assert_torn_down() {
  assert_gone "$1" "${INST_WORKER[$1]}" "${INST_META[$1]}"
}

http_body() { curl -sf --max-time 3 "http://$1:$2/"; }
http_says() { # http_says <host> <port> <expected line>
  local out
  out=$(http_body "$1" "$2" 2>/dev/null || true)
  grep -qx -- "$3" <<<"$out"
}
tcp_says() { # tcp_says <host> <port> <stdin> <expected substring>
  local out
  out=$(printf '%b' "$3" | nc -w 5 "$1" "$2" 2>/dev/null || true)
  grep -q -- "$4" <<<"$out"
}

# check_ondemand <instance json> <user index> [extra line]: the instance
# answers on its public address with its own environment and the build's
# flag, and its container is on its worker.
check_ondemand() {
  local meta=$1 i=$2 w pub port body
  w=$(jq -r .worker <<<"$meta")
  pub=$(jq -r .worker_public <<<"$meta")
  port=$(jq -r .ports.server <<<"$meta")
  [[ "$pub" == "${PUBLIC[$w]:-}" ]] || fail "instance $(jq -r .id <<<"$meta"): worker_public is '$pub', expected '${PUBLIC[$w]:-?}'"
  (( port >= PORT_LOW && port <= PORT_HIGH )) || fail "port $port is outside CMGR_PORTS $PORT_LOW-$PORT_HIGH"
  assert_on_worker "$w" "$meta"
  retry 30 "http at $pub:$port" quiet http_body "$pub" "$port"
  body=$(http_body "$pub" "$port")
  grep -qx "CMGR_USER_ID=e2e-user-$i" <<<"$body" || fail "instance $(jq -r .id <<<"$meta"): user_id was not injected: $body"
  grep -qx "CMGR_CUSTOM_VAR=e2e-value-$i" <<<"$body" || fail "instance $(jq -r .id <<<"$meta"): env was not injected: $body"
  grep -qxF "FLAG=$OD_FLAG" <<<"$body" || fail "instance $(jq -r .id <<<"$meta"): served flag does not match the build: $body"
  if (( $# > 2 )); then
    grep -qx -- "$3" <<<"$body" || fail "instance $(jq -r .id <<<"$meta"): expected '$3' in: $body"
  fi
}

# image_tag <build json> <host>: BuildMetadata.dockerId, s<seed>-<checksum hex>-<host>
image_tag() { printf 's%d-%x-%s' "$(jq -r .seed <<<"$1")" "$(jq -r .checksum <<<"$1")" "$2"; }
# The artifact endpoints, which are the platform's and a competitor's only view
# of what a build publishes. artifact_get writes the body to disk (an artifact
# is gzip, which no shell variable survives) and prints the status instead of
# failing, exactly as api_status does, so the caller judges it.
artifact_get() { # artifact_get <build id> <member|artifacts.tar.gz> <output file>
  curl -sS -o "$3" -w '%{http_code}' --connect-timeout 5 --max-time 60 \
    "$CMGRD_SERVER/builds/$1/$2"
}
# art_members <file>: the archive's member names, sorted, on one line, so a
# bundle's contents can be compared against a literal. Non-zero (pipefail) when
# the file is not a readable gzip tar, which is itself an assertion.
art_members() { tar -tzf "$1" | sort | tr '\n' ' ' | sed 's/ $//'; }
# same_bytes <a> <b>: byte-identical files. diff rather than cmp because diff is
# the comparison tool this image is known to carry (the seed check uses it) and
# it reports binary files as differing instead of printing them.
same_bytes() { diff -q "$1" "$2" >/dev/null 2>&1; }
# exec_in <worker ip> <container id> <user> <cmd...>: run a command inside a
# container over that worker's own dockerd, with cork's client certificate.
# Tty:true so the response body is the command's raw output: without it docker
# frames the stream in 8-byte headers that nothing in this image can
# demultiplex. Prints stdout and stderr together, with the pty's CRs stripped.
exec_in() {
  local ip=$1 cid=$2 user=$3 eid req
  shift 3
  # One argument per line into jq -R, rather than jq's positional-argument
  # option, whose variable reads as an unset shell variable to anything
  # scanning this file. No command run through here has a newline in an
  # argument.
  req=$(printf '%s\n' "$@" | jq -cRn --arg u "$user" '{AttachStdout: true, AttachStderr: true, Tty: true, User: $u, Cmd: [inputs]}')
  eid=$(worker_api "$ip" "/containers/$cid/exec" -X POST -H 'Content-Type: application/json' -d "$req" | jq -r '.Id // empty')
  [[ -n "$eid" ]] || return 1
  worker_api "$ip" "/exec/$eid/start" -X POST -H 'Content-Type: application/json' \
    -d '{"Detach": false, "Tty": true}' | tr -d '\r'
}

# manual_builds <challenge> <seed>: the ids of hand-made builds (cmgrd-cli
# build, not a schema's) of that challenge and seed. The full-mode registry
# steps make one and destroy it again; a run killed before its EXIT trap leaves
# it behind, and a second build with the same challenge, seed and checksum
# makes contentReferenced true, so the next run's destroy would keep the images
# it is asserted to untag.
manual_builds() { # <challenge id> <seed>
  cmgrd-cli system-dump |
    jq -r --arg c "$1" --argjson s "$2" '.[] | select(.id == $c) | .builds[]? | select(.seed == $s) | .id'
}

# Edit the sources the update steps rebuild. The generation marker shows up
# in what each service answers, so a rebuilt image is distinguishable from
# the old one at the wire.
restore_sources() {
  cp "$CHALLENGES_SEED/$ONDEMAND_SRC" "$CHALLENGES/$ONDEMAND_SRC"
  cp "$CHALLENGES_SEED/$PERSISTENT_SRC" "$CHALLENGES/$PERSISTENT_SRC"
  # Metadata, not source: the container-options step appends a Challenge
  # Options block here. problem.md is outside the source checksum but inside
  # the tree check, so a run killed without its trap is repaired here.
  cp "$CHALLENGES_SEED/$MAKE_META" "$CHALLENGES/$MAKE_META"
  # The pin step writes CMGR_BASE_PINS here (the corpus root, as in
  # production). It is cork's file rather than an edit, but it lands inside the
  # tree the seed check diffs, and run.sh does not take the fleet down with -v,
  # so a run that left one behind would fail the NEXT run at step 2.
  rm -f "$CHALLENGES/base-pins.json"
}
# The generation each source currently carries on disk, maintained by
# set_generation. A step that edits one source and must put it back cannot name
# a literal: which generation the other steps have left behind depends on the
# mode, and a step deselected in regular mode moves it. This is the second time
# that coupling has cost a run, so the number is read rather than written.
OD_GEN=0
PERSIST_GEN=0
set_generation() { # set_generation <n> both|ondemand|persistent: mark the source(s) as generation n
  case "$2" in
    persistent) PERSIST_GEN=$1 ;;
    ondemand) OD_GEN=$1 ;;
    both) PERSIST_GEN=$1; OD_GEN=$1 ;;
    *) fail "set_generation: unknown mode '$2'" ;;
  esac
  if [[ "$2" == persistent ]]; then
    # Only the persistent challenge. A step that wants to rebuild it must not
    # rewrite the on-demand source at whatever generation another step left it
    # at: that would rebuild the on-demand challenge too and tear down every
    # instance the scenario still tracks.
    cp "$CHALLENGES_SEED/$PERSISTENT_SRC" "$CHALLENGES/$PERSISTENT_SRC"
    sed -i "s/-in secret.enc'/-in secret.enc e2e-generation-$1'/" "$CHALLENGES/$PERSISTENT_SRC"
    grep -q "e2e-generation-$1" "$CHALLENGES/$PERSISTENT_SRC" || fail "could not mark $PERSISTENT_SRC"
    sed -i "s|RUN date > /time.txt|RUN echo e2e-generation-$1 > /time.txt|" "$CHALLENGES/$PERSISTENT_SRC"
    grep -qx "RUN echo e2e-generation-$1 > /time.txt" "$CHALLENGES/$PERSISTENT_SRC" ||
      fail "could not stamp the generation into the artifact source of $PERSISTENT_SRC"
    return
  fi
  cp "$CHALLENGES_SEED/$ONDEMAND_SRC" "$CHALLENGES/$ONDEMAND_SRC"
  sed -i "s/response = f\"CMGR_USER_ID/response = f\"E2E_GENERATION=$1\\\\nCMGR_USER_ID/" "$CHALLENGES/$ONDEMAND_SRC"
  grep -q "E2E_GENERATION=$1" "$CHALLENGES/$ONDEMAND_SRC" || fail "could not mark $ONDEMAND_SRC"
  if [[ "$2" == both ]]; then
    cp "$CHALLENGES_SEED/$PERSISTENT_SRC" "$CHALLENGES/$PERSISTENT_SRC"
    sed -i "s/-in secret.enc'/-in secret.enc e2e-generation-$1'/" "$CHALLENGES/$PERSISTENT_SRC"
    grep -q "e2e-generation-$1" "$CHALLENGES/$PERSISTENT_SRC" || fail "could not mark $PERSISTENT_SRC"
    # And inside the artifact, not only in what the service says: time.txt is
    # tarred into the published bundle, and it is the only thing that makes a
    # promoted archive distinguishable from the one it replaced. cmgr builds
    # with the cache on and the edit above touches only the CMD, below the tar,
    # so without this the bundle is byte-stable across generations and the
    # full-mode "the promoted bundle really changed" assertion could not exist.
    sed -i "s|RUN date > /time.txt|RUN echo e2e-generation-$1 > /time.txt|" "$CHALLENGES/$PERSISTENT_SRC"
    grep -qx "RUN echo e2e-generation-$1 > /time.txt" "$CHALLENGES/$PERSISTENT_SRC" ||
      fail "could not stamp the generation into the artifact source of $PERSISTENT_SRC"
  fi
}

# ------------------------------------------------------------------ setup

declare -A PUBLIC=()
WORKER_IPS=()
for spec in $E2E_WORKERS; do
  WORKER_IPS+=("${spec%%=*}")
  PUBLIC["${spec%%=*}"]="${spec#*=}"
done
NWORKERS=${#WORKER_IPS[@]}
(( NWORKERS == 2 )) || fail "E2E_WORKERS must name exactly two workers"
WA=${WORKER_IPS[0]}
WB=${WORKER_IPS[1]}

printf 'cork end-to-end scenario\n  cmgrd     %s\n  workers   %s\n  registry  %s\n' \
  "$CMGRD_SERVER" "$E2E_WORKERS" "$E2E_REGISTRY"
# Latched once: every chaos step tests OUTER rather than re-probing, so a
# socket that goes away mid-run fails the step that needed it instead of
# turning into a skip nobody reads.
OUTER=0
if have_outer; then
  OUTER=1
  note "outer docker socket available: chaos steps enabled"
else
  note "outer docker socket unavailable or E2E_CHAOS=$E2E_CHAOS: chaos steps will be skipped"
fi

step "waiting for the fleet"
retry 90 "cmgrd" quiet api GET /version
retry 90 "zot" quiet registry_api /v2/
retry 60 "the builder daemon" quiet curl -sSf "$E2E_BUILDER/_ping"
for ip in "${WORKER_IPS[@]}"; do
  retry 120 "dockerd on $ip" quiet worker_api "$ip" /_ping
  retry 60 "telemetry on $ip" quiet curl -sSf --max-time 3 "http://$ip:2136/health"
  # A worker that fell back to iptables still passes every other step, while
  # behaving nothing like production: network setup there grows with the
  # number of challenge networks on the box.
  info=$(worker_api "$ip" /info)
  backend=$(jq -r '.FirewallBackend.Driver // "unknown"' <<<"$info")
  [[ "$backend" == "nftables" ]] || fail "worker $ip programs its firewall with $backend, not nftables"
  # oci-interceptor is the default runtime on a production worker, so every
  # container cmgrd creates goes through it. A worker running plain runc passes
  # every other step here while exercising a different runtime chain -- and the
  # chain is where the shim leak lived.
  runtime=$(jq -r '.DefaultRuntime // "unknown"' <<<"$info")
  [[ "$runtime" == "oci-interceptor" ]] ||
    fail "worker $ip has default runtime '$runtime', not oci-interceptor: the fleet is not exercising the runtime chain production runs (e2e/worker/daemon.json, e2e/worker.Dockerfile)"
  # And it must be OUR wrapper that dockerd registered, with no runtimeArgs.
  #
  # This is the only thing in the fleet that can see the regression the whole
  # wrapper design exists for. Give a runtime a non-empty runtimeArgs and
  # docker writes its own wrapper -- "#!/bin/sh\n<path> <args> $@", no exec --
  # and registers THAT instead, leaving a shell between the containerd shim
  # and runc for the lifetime of every runtime call. A cancelled `runc delete`
  # then SIGKILLs the shell and orphans runc, and its shim never shuts down:
  # 66 of them, ~5 MiB each, measured on a 2 GiB worker.
  #
  # Nothing observable inside a container can catch that, because the
  # generated wrapper still invokes the interceptor and the spec is still
  # rewritten -- the read-only mounts asserted later are identical either way.
  # The registered path and runtimeArgs are where the two differ.
  rt_path=$(jq -r '.Runtimes["oci-interceptor"].path // "absent"' <<<"$info")
  rt_args=$(jq -r '.Runtimes["oci-interceptor"].runtimeArgs // [] | length' <<<"$info")
  [[ "$rt_path" == /usr/local/bin/oci-interceptor-runtime.sh ]] ||
    fail "worker $ip registered oci-interceptor as '$rt_path', not the exec'ing wrapper e2e/worker/daemon.json names. A path under /var/lib/docker/runtimes/ means docker generated its own wrapper because runtimeArgs is non-empty, and that wrapper does not exec: it stays between the containerd shim and runc and leaks a shim per cancelled runtime call"
  [[ "$rt_args" == 0 ]] ||
    fail "worker $ip has $rt_args runtimeArgs on the oci-interceptor runtime; it must have none. Docker generates a non-exec'ing wrapper for any runtime whose runtimeArgs is non-empty, which re-inserts the process layer oci-interceptor v0.3.0 removed. The interceptor's flags belong in e2e/worker/oci-interceptor-runtime.sh, which execs"
  sweep_worker "$ip"
done
ok "cmgrd $(api GET /version | jq -r .version), zot, builder, and $NWORKERS workers (dockerd on nftables + oci-interceptor + telemetry) answer"

########################################################################
# BLOCK 1 of 4 — after the "waiting for the fleet" step (see insertAfter)
########################################################################

# ------------------------------- 1b. a fresh box makes its own directories

if (( FULL )); then
  step "fresh-box startup: cmgrd creates its artifact and database directories itself, and still refuses a missing challenge directory"
  # The promise is about a box that has nothing on it yet: ansible drops the
  # unit file and the challenge tree, and cmgrd makes CMGR_ARTIFACT_DIR and
  # the directory holding CMGR_DB itself (MkdirAll in cmgr/filesystem.go
  # setDirectories:60-67 and cmgr/database.go initDatabase:225-230, which
  # replaced a bare Stat and sqlite's "the file, never its directory"). The
  # fleet's own cork can never answer it: cork-data has been mounted and
  # written since the first run of this checkout, so both directories always
  # already exist. So this runs a throwaway cmgrd out of this very image --
  # the e2e service is built from cork.Dockerfile, so /usr/local/bin/cmgrd is
  # the same binary cork runs -- against a tmpdir that is new every run and
  # the builder daemon the fleet already answers on. It duplicates
  # cmgr/startup_dirs_test.go on purpose: what the unit tests cannot say is
  # that the shipped binary does it in the shipped layout.
  #
  # Nothing of the fleet is touched. The e2e service carries no CMGR_* in its
  # environment (compose.yaml), so this daemon has no CMGR_REGISTRY (no
  # registry work), no CMGR_PORTS (no port reservation), its own tmpdir
  # database, no workers to load, and reads /challenges without ever scanning
  # it (an update is an explicit call). Its only docker traffic is
  # initDocker's Ping and Info against the builder (cmgr/docker.go:36-110),
  # both read-only. DOCKER_CERT_PATH is inherited and deliberately harmless:
  # initDocker uses client.WithHostFromEnv() only, never client.FromEnv, so
  # the plain-TCP builder is not switched into HTTPS mode.
  fresh=$(mktemp -d)
  fresh_docker="tcp://${E2E_BUILDER#*://}" # the DOCKER_HOST form of $E2E_BUILDER
  # Two levels below a directory that does not exist either: a Mkdir would
  # fail where MkdirAll must not.
  CMGR_DIR=/challenges \
  CMGR_ARTIFACT_DIR="$fresh/var/lib/cork/artifacts" \
  CMGR_DB="$fresh/var/lib/cork/db/cmgr.db" \
  DOCKER_HOST="$fresh_docker" \
    cmgrd --port 4299 >"$fresh/cmgrd.log" 2>&1 &
  FRESH_CMGRD=$! # from here the EXIT trap kills it if anything below fails
  fresh_answers() {
    # Fail on a daemon that died rather than spend the whole retry budget on
    # a port nothing will ever answer on, and say why it died. NewManager
    # returns nil on any startup failure and cmgrd log.Fatals on that
    # (cmd/cmgrd/main.go:52-54), so a dead process here is the assertion.
    kill -0 "$FRESH_CMGRD" 2>/dev/null ||
      fail "the throwaway cmgrd exited instead of starting on a fresh box: $(tr '\n' ' ' <"$fresh/cmgrd.log" | tail -c 400)"
    quiet curl -sSf --max-time 3 http://127.0.0.1:4299/version
  }
  retry 30 "the throwaway cmgrd to answer on :4299" fresh_answers
  # Answering at all already means both directories were made. These two say
  # which one, and that the database really opened in a directory sqlite
  # would not have created.
  [[ -d "$fresh/var/lib/cork/artifacts" ]] ||
    fail "the throwaway cmgrd answered without creating CMGR_ARTIFACT_DIR ($fresh/var/lib/cork/artifacts): on a fresh box every artifact bundle would fail until someone made it by hand (cmgr/filesystem.go setDirectories)"
  [[ -f "$fresh/var/lib/cork/db/cmgr.db" ]] ||
    fail "the throwaway cmgrd left no database at $fresh/var/lib/cork/db/cmgr.db: sqlite creates the file but never its directory, so initDatabase must MkdirAll filepath.Dir(CMGR_DB) (cmgr/database.go)"
  kill "$FRESH_CMGRD" >/dev/null 2>&1 || true
  wait "$FRESH_CMGRD" 2>/dev/null || true
  FRESH_CMGRD=""
  # The other half, which the commit message is explicit about: creating what
  # cmgr owns must not turn into creating what the operator owns. A missing
  # challenge directory stays a hard failure -- and it is checked before
  # anything is created (setDirectories stats CMGR_DIR at :37, the artifacts
  # MkdirAll is at :64 and initDatabase runs later still, cmgr/api.go:43-53),
  # so a refused start leaves no tree behind either.
  t=$(date +%s)
  rc=0
  CMGR_DIR="$fresh/nope" \
  CMGR_ARTIFACT_DIR="$fresh/a2" \
  CMGR_DB="$fresh/d2/cmgr.db" \
  DOCKER_HOST="$fresh_docker" \
    timeout 30 cmgrd --port 4298 >"$fresh/refused.log" 2>&1 || rc=$?
  refuse_took=$(( $(date +%s) - t ))
  (( rc != 0 )) ||
    fail "cmgrd started with CMGR_DIR pointing at a directory that does not exist: a mistyped challenge path must fail the unit, not bring a box up serving an empty catalogue"
  # Independent of the exit status, because the status busybox timeout reports
  # for a kill is not something to depend on: a cmgrd that got as far as its
  # artifact directory started, whatever it exited with.
  [[ ! -e "$fresh/a2" && ! -e "$fresh/d2" ]] ||
    fail "cmgrd created its artifact/database tree although CMGR_DIR was missing: the challenge directory is stat'ed first (setDirectories) precisely so a misconfigured box leaves nothing behind"
  (( refuse_took < 25 )) ||
    fail "cmgrd did not exit on a missing challenge directory; it had to be killed after ${refuse_took}s"
  # The exact error, not just the phrase: setDirectories logs "challenge
  # directory: <path>" at INFO on every successful start too, so grepping for
  # "challenge directory" would pass whatever it exited on.
  grep -q "could not stat the challenge directory" "$fresh/refused.log" ||
    fail "cmgrd exited $rc for some reason other than the missing challenge directory: $(tr '\n' ' ' <"$fresh/refused.log" | tail -c 400)"
  rm -rf "$fresh"
  ok "a cmgrd started on a fresh path created CMGR_ARTIFACT_DIR and its database directory itself and served /version; one pointed at a missing CMGR_DIR exited $rc in ${refuse_took}s and created neither"
else
  deselect "fresh-box startup directories"
fi

# ------------------------------------------------- 1. register the workers

step "registering the workers: cmgrd-cli worker-add <private ip> <public name>"
for ip in "${WORKER_IPS[@]}"; do
  cmgrd-cli worker-add "$ip" "${PUBLIC[$ip]}"
done
all_workers_ok() {
  [[ "$(api GET /workers | jq -r 'map(select(.health == "ok")) | length')" == "$NWORKERS" ]]
}
retry 30 "every worker to report ok" all_workers_ok
cmgrd-cli worker-list | sed 's/^/       /'
for ip in "${WORKER_IPS[@]}"; do
  public=$(api GET /workers | jq -r --arg ip "$ip" '.[] | select(.ip == $ip) | .public')
  [[ "$public" == "${PUBLIC[$ip]}" ]] || fail "worker $ip has public address '$public', expected '${PUBLIC[$ip]}'"
done
ok "$NWORKERS workers registered, telemetry-polled, and eligible for placement"

# --------------------------------------------- 2. scan the challenge tree

step "scanning the challenge directory: cmgrd-cli update"
[[ -d "$CHALLENGES_SEED" && -f "$CHALLENGES/$ONDEMAND_SRC" ]] || fail "challenge tree not mounted at $CHALLENGES (seed at $CHALLENGES_SEED)"
restore_sources # undo edits left by an interrupted run
# The CMGR_DIR volume is seeded from the read-only mount only when it is
# empty, and run.sh never takes the fleet down with -v: a volume left over
# from an older checkout would silently test challenges nobody has now.
if ! seed_diff=$(diff -rq "$CHALLENGES_SEED" "$CHALLENGES" 2>&1); then
  fail "the challenges volume does not match this checkout ($(head -3 <<<"$seed_diff" | tr '\n' ';')); run 'docker compose down -v' and bring the fleet back up"
fi
cmgrd-cli update --verbose | sed 's/^/       /'
for id in "$CH_PERSISTENT" "$CH_ONDEMAND" "$CH_MAKE" "$CH_FLAGONLY"; do
  if ! has_line "$id" cmgrd-cli list; then fail "$id is not in the challenge list"; fi
done
ok "the schema's challenges are known to cmgrd"

# ---------------------------------------------------- 3. converge schema

step "converging the schema: cmgrd-cli add-schema (builds on the builder daemon, pushes to zot, starts persistent instances)"
if has_line "$SCHEMA_NAME" cmgrd-cli list-schemas; then
  note "schema $SCHEMA_NAME is left over from an earlier run; removing it first"
  cmgrd-cli remove-schema "$SCHEMA_NAME"
fi
t=$(date +%s)
cmgrd-cli add-schema "$E2E_SCHEMA"
note "converged in $(( $(date +%s) - t ))s"

STATE=$(api GET "/schemas/$SCHEMA_NAME")
challenge() { jq -c --arg id "$1" '.[] | select(.id == $id)' <<<"$STATE"; }
build_id() { challenge "$1" | jq -r '.builds[0].id'; }

for id in "$CH_PERSISTENT" "$CH_ONDEMAND" "$CH_MAKE" "$CH_FLAGONLY"; do
  [[ -n "$(challenge "$id")" ]] || fail "$id is missing from the schema state"
  [[ "$(build_id "$id")" =~ ^[0-9]+$ ]] || fail "$id has no build"
done
[[ "$(challenge "$CH_FLAGONLY" | jq -r .delivery_type)" == flag_only ]] || fail "$CH_FLAGONLY should be flag_only"
[[ "$(challenge "$CH_FLAGONLY" | jq -r '.builds[0].instances | length')" == 0 ]] || fail "the flag-only challenge got an instance"
[[ -n "$(challenge "$CH_FLAGONLY" | jq -r '.builds[0].flag')" ]] || fail "the flag-only build has no flag"
[[ -n "$(challenge "$CH_FLAGONLY" | jq -r '.builds[0].lookup_data.options // empty')" ]] || fail "the flag-only build has no lookup data"
for id in "$CH_ONDEMAND" "$CH_MAKE"; do
  [[ "$(challenge "$id" | jq -r '.builds[0].instances | length')" == 0 ]] || fail "on-demand challenge $id has an instance already"
done
PERSIST_INST=$(challenge "$CH_PERSISTENT" | jq -r '.builds[0].instances[0].id // empty')
[[ -n "$PERSIST_INST" ]] || fail "no persistent instance was started for $CH_PERSISTENT"
PERSIST_BUILD=$(build_id "$CH_PERSISTENT")
OD_BUILD=$(build_id "$CH_ONDEMAND")
MK_BUILD=$(build_id "$CH_MAKE")
OD_FLAG=$(api GET "/builds/$OD_BUILD" | jq -r .flag)
ok "4 builds; flag-only build has a flag and options but no instance; persistent instance $PERSIST_INST exists"

step "persistent instance: placed on a worker during the converge, serving on its public address"
PERSIST_META=$(api GET "/instances/$PERSIST_INST")
PERSIST_WORKER=$(jq -r .worker <<<"$PERSIST_META")
pub=$(jq -r .worker_public <<<"$PERSIST_META")
port=$(jq -r .ports.socat <<<"$PERSIST_META")
[[ -n "${PUBLIC[$PERSIST_WORKER]:-}" ]] || fail "instance $PERSIST_INST is on unknown worker '$PERSIST_WORKER'"
[[ "$pub" == "${PUBLIC[$PERSIST_WORKER]}" ]] || fail "worker_public is '$pub', expected '${PUBLIC[$PERSIST_WORKER]}'"
(( port >= PORT_LOW && port <= PORT_HIGH )) || fail "port $port is outside CMGR_PORTS $PORT_LOW-$PORT_HIGH"
assert_on_worker "$PERSIST_WORKER" "$PERSIST_META"
retry 30 "the socat service at $pub:$port" tcp_says "$pub" "$port" '' 'openssl aes-256-cbc'
ok "instance $PERSIST_INST on $PERSIST_WORKER: container on the worker, $pub:$port hands out the decryption command"
# The chaos steps take one worker down, purge it, and pause it. That must not
# be the worker hosting the persistent instance (worker-remove would purge
# its record too), and its placement follows cork's round-robin cursor, not
# anything the scenario controls: pick the other one.
if [[ "$PERSIST_WORKER" == "${WORKER_IPS[0]}" ]]; then
  WA=${WORKER_IPS[0]}; WB=${WORKER_IPS[1]}
else
  WA=${WORKER_IPS[1]}; WB=${WORKER_IPS[0]}
fi
note "chaos target is $WB (${PUBLIC[$WB]}); the persistent instance stays on $WA"

# BLOCK 1 -- insert after e2e/scenario.sh:633 (the `note "chaos target is ..."`
# that closes the "persistent instance" step). Needs PERSIST_BUILD/MK_BUILD/
# OD_BUILD (:607-609), build_id/$STATE (:590-592) and PERSIST_META (:614), and
# must run BEFORE the first set_generation edit.
# ============================================================================

# --------------------------------------------- 3b. artifact delivery

if (( FULL )); then
  step "artifacts: every published bundle downloads, holds exactly the challenge's files, and decrypts to the build's flag"
  # The half of the player contract nothing else in this scenario touches: the
  # platform hands a competitor a download, not a flag, and cork is the only
  # thing that serves it. cork's artifact directory lives in the cork-data
  # volume, which this container deliberately does not mount (compose.yaml), so
  # every assertion below travels the same HTTP a player's download does.
  # Placed here on purpose: it must run before the first set_generation edit,
  # which appends an argument to the advertised openssl command, because the
  # round trip at the end runs the command the service hands out.
  # The decrypt round trip needs openssl, which this image does not carry.
  # Everything else this step asserts — that the bundle downloads, holds
  # exactly the challenge's files, and belongs to this build — does not, so the
  # round trip degrades to a note rather than taking the step down with it.
  ART_OPENSSL=1
  command -v openssl >/dev/null 2>&1 || ART_OPENSSL=0
  (( ART_OPENSSL )) ||
    note "no openssl in this image: the decrypt round trips are not run (add openssl to the apk line in e2e/cork.Dockerfile to restore them)"
  ART_TMP=$(mktemp -d) # also holds the bundles the two artifact steps after the rebuilds compare against
  ART_G1="$ART_TMP/persist-g1.tar.gz"
  PERSIST_FLAG=$(api GET "/builds/$PERSIST_BUILD" | jq -r .flag)
  [[ -n "$PERSIST_FLAG" && "$PERSIST_FLAG" != null ]] ||
    fail "build $PERSIST_BUILD has no flag to check its artifact against"

  # has_artifacts is what the platform reads to decide whether to offer a
  # download at all, and it is stored per build (HasArtifacts = len(files) > 0,
  # cmgr/docker.go:810). Two builds that publish and two that do not, so the
  # field is proved to discriminate rather than to be constantly true: a
  # regression that derived it from the challenge type, or defaulted it, fails
  # on one of the two halves.
  for b in "$PERSIST_BUILD" "$MK_BUILD"; do
    [[ "$(api GET "/builds/$b" | jq -r .has_artifacts)" == true ]] ||
      fail "build $b publishes artifacts but reports has_artifacts=false: the platform would never offer its download (HasArtifacts, cmgr/docker.go:810)"
  done
  FLAG_BUILD=$(build_id "$CH_FLAGONLY")
  for b in "$OD_BUILD" "$FLAG_BUILD"; do
    [[ "$(api GET "/builds/$b" | jq -r .has_artifacts)" == false ]] ||
      fail "build $b publishes no artifacts.tar.gz but reports has_artifacts=true: the platform would offer a download cork cannot serve"
  done

  # The operator's path (cmgrd-cli artifacts) and the player's path
  # (GET /builds/<id>/artifacts.tar.gz) are the same handler; the CLI is used
  # here so its exit status is covered too (cmd/cmgrd-cli/commands.go:421-445).
  out=$(cmgrd-cli artifacts "$PERSIST_BUILD" "$ART_G1" 2>&1) ||
    fail "cmgrd-cli artifacts $PERSIST_BUILD did not succeed: $out"
  members=$(art_members "$ART_G1") ||
    fail "the bundle served for build $PERSIST_BUILD is not a readable gzip tar"
  # Exactly the files examples/custom/Dockerfile publishes, and nothing else: a
  # member the challenge never published is a leak (the image's own /challenge
  # tree would be one), and a missing member is a broken player download that
  # nothing else in this run would notice.
  [[ "$members" == "secret.enc time.txt" ]] ||
    fail "the bundle for build $PERSIST_BUILD holds '$members', expected 'secret.enc time.txt' (examples/custom/Dockerfile publishes exactly those two)"

  ART_MK="$ART_TMP/make.tar.gz"
  out=$(cmgrd-cli artifacts "$MK_BUILD" "$ART_MK" 2>&1) ||
    fail "cmgrd-cli artifacts $MK_BUILD did not succeed: $out"
  members=$(art_members "$ART_MK") ||
    fail "the bundle served for build $MK_BUILD is not a readable gzip tar"
  # The remote-make challenge tars its own archive in its Makefile and the
  # built-in Dockerfile moves it into /challenge, so this is the second,
  # independent way a bundle reaches cork's artifact directory.
  [[ "$members" == "BinEx101 BinEx101.c" ]] ||
    fail "the bundle for build $MK_BUILD holds '$members', expected 'BinEx101 BinEx101.c' (the artifacts.tar.gz target of examples/remote-make/Makefile)"

  # The per-file path is a different branch of the same handler: it walks the
  # stored tar and copies one member out (cmd/cmgrd/main.go:493-514). Comparing
  # it against the same member of the bundle just downloaded is what proves it
  # serves the raw file rather than the gzip, a prefix of it, or the wrong
  # member -- none of which a status check alone would catch.
  code=$(artifact_get "$PERSIST_BUILD" secret.enc "$ART_TMP/secret.enc")
  [[ "$code" == 200 ]] ||
    fail "GET /builds/$PERSIST_BUILD/secret.enc answered HTTP $code: the per-file path is how every {{url_for}} link in a challenge's text is served"
  tar -xzOf "$ART_G1" secret.enc >"$ART_TMP/secret.from-bundle" ||
    fail "could not read secret.enc out of the bundle for build $PERSIST_BUILD"
  same_bytes "$ART_TMP/secret.enc" "$ART_TMP/secret.from-bundle" ||
    fail "the per-file download of secret.enc differs from the same member of the same build's bundle: the tar walk at cmd/cmgrd/main.go:493-514 served something other than the stored member"
  # A name the archive does not hold is a 404, not a 500 and not an empty 200:
  # the platform renders {{url_for}} links out of the challenge text, so a typo
  # there must read as missing rather than as a broken cmgrd.
  code=$(api_status GET "/builds/$PERSIST_BUILD/e2e-not-a-member")
  [[ "$code" == 404 ]] ||
    fail "GET /builds/$PERSIST_BUILD/e2e-not-a-member answered HTTP $code, expected 404 for a member the bundle does not hold (the io.EOF arm of the tar walk, cmd/cmgrd/main.go:511-514)"

  # The round trip, which is the whole reason anything is published: the file
  # the player downloads, decrypted with the command the running service hands
  # out, is this build's flag. Nothing else in the scenario links an artifact to
  # a flag, and nothing else would notice a bundle promoted from another build
  # or another seed -- the member names and the sizes would all be right.
  # Retried rather than asked once: this is a guard on the fixture, not the
  # assertion, and the socat service is one TCP connection per answer.
  ppub=$(jq -r .worker_public <<<"$PERSIST_META")
  pport=$(jq -r .ports.socat <<<"$PERSIST_META")
  retry 15 "the persistent instance at $ppub:$pport to advertise the generation-1 decryption command this step is about to run" \
    tcp_says "$ppub" "$pport" '' 'openssl aes-256-cbc -d -k unguessable -pbkdf2 -in secret.enc'
  # Guarded: a failed decrypt exits non-zero, and a bare one would abort the run
  # without saying what went wrong.
  if (( ART_OPENSSL )); then
    dec=$(openssl aes-256-cbc -d -k unguessable -pbkdf2 -in "$ART_TMP/secret.enc" 2>/dev/null || true)
    [[ "$dec" == "$PERSIST_FLAG" ]] ||
      fail "secret.enc from build $PERSIST_BUILD's bundle decrypts to '$dec', not to the build's flag '$PERSIST_FLAG': the archive cork serves is not this build's"
  fi

  # Negative space. A build that publishes nothing must never hand out bytes:
  # the served path is <artifact dir>/<build id>.tar.gz, so a build id that
  # collided with another build's archive would answer 200 here and give one
  # challenge's files to another's players.
  code=$(api_status GET "/builds/$OD_BUILD/artifacts.tar.gz")
  [[ "$code" != 200 ]] ||
    fail "build $OD_BUILD reports has_artifacts=false yet GET /builds/$OD_BUILD/artifacts.tar.gz answered 200: cork served an archive for a build that publishes none"
  # Deliberately not asserted as 404, which is what the unknown-build arm below
  # really does answer: for a KNOWN build with no artifacts the guard at
  # cmd/cmgrd/main.go:478 reads "ok || (err != nil && !meta.HasArtifacts)", and
  # err is nil on that path, so the handler falls through to an os.Open that
  # cannot succeed and answers 500. Asserting 404 here would fail today against
  # behaviour nobody has changed; "not 200" fails only for the case that costs a
  # player someone else's files, and keeps passing once the guard is corrected.
  [[ "$code" == 404 ]] ||
    note "a build without artifacts answers HTTP $code rather than 404 (cmd/cmgrd/main.go:478 tests err != nil where err == nil was meant)"
  [[ "$(api_status GET "/builds/999999/artifacts.tar.gz")" == 404 ]] ||
    fail "GET /builds/999999/artifacts.tar.gz was not a 404: an unknown build must come back as an UnknownIdentifierError, not a 500 (cmd/cmgrd/main.go:474-479)"

  ok "has_artifacts is true for exactly the two builds that publish; both bundles hold exactly their challenge's files; the per-file path serves the stored member byte for byte and 404s an unknown one; secret.enc decrypts to build $PERSIST_BUILD's flag with the command the service advertises; a build without artifacts serves nothing and an unknown build is a 404"
else
  deselect "artifacts: bundles download, hold the challenge's files, and decrypt to the flag"
fi

# ============================================================================

# ------------------------------------------------------------ 4. registry

step "registry: every build's instance images are in zot under their content-addressed tags"
declare -A IMAGE_TAG=()
for id in "$CH_PERSISTENT" "$CH_ONDEMAND" "$CH_MAKE" "$CH_FLAGONLY"; do
  build=$(api GET "/builds/$(build_id "$id")")
  tags=$(registry_tags "$id")
  for host in $(jq -r '.images[].host' <<<"$build"); do
    if [[ "$host" == builder ]]; then continue; fi
    tag=$(image_tag "$build" "$host")
    if ! grep -qx "$tag" <<<"$tags"; then fail "$E2E_REGISTRY/$id:$tag is not in the registry"; fi
    IMAGE_TAG[$id]=$tag
    note "$E2E_REGISTRY/$id:$tag"
  done
done
# Workers keep images until docker-reaper sweeps them, so a previous run may
# have left this tag behind; remember who has it to tell a pull from a cache hit.
declare -A HAD_TAG=()
for ip in "${WORKER_IPS[@]}"; do
  if has_line "$E2E_REGISTRY/$CH_ONDEMAND:${IMAGE_TAG[$CH_ONDEMAND]}" worker_tags "$ip"; then
    HAD_TAG[$ip]=1
  fi
done
ok "all instance images were pushed"

# ------------------------------- 5b. purge after push

step "purge after push: the builder keeps no copy of what it pushed, and keeps the layer cache that makes the next build cheap"
# The two halves are one claim. Dropping the images is what stops the builder's
# image store growing with the whole fleet -- challenges times seeds times
# retained generations, on a volume it shares with the challenge tree, the
# database and the artifacts. Keeping the cache is what makes that free rather
# than a trade: under the legacy builder the cache WAS untagged images, so the
# same reclaim re-ran every apt install in the fleet on the next build.
#
# This fleet always runs with CMGR_PURGE_AFTER_PUSH on, which is cmgrd's default
# whenever a registry is configured. Turning it off is a supported setting but
# not one any deployment uses -- and the case that genuinely needs the builder's
# copies, single-host cmgr with no registry at all, cannot be run here because
# every step of this fleet leans on zot.
btags=$(builder_tags)
held=""
for id in "$CH_PERSISTENT" "$CH_ONDEMAND" "$CH_MAKE" "$CH_FLAGONLY"; do
  if grep -q "^$E2E_REGISTRY/$id:" <<<"$btags"; then held="$held $id"; fi
done
[[ -z "$held" ]] ||
  fail "the builder still holds images for$held after pushing them: purgeBuiltImages runs after finalizeBuild and should have dropped every one (cmgr/purge.go)"
# The registry has to hold what the builder just gave up, or the purge threw
# away the only copy rather than a redundant one. This is also what keeps the
# check above from passing on a build that produced nothing.
for id in "$CH_PERSISTENT" "$CH_ONDEMAND" "$CH_MAKE" "$CH_FLAGONLY"; do
  has_line "${IMAGE_TAG[$id]}" registry_tags "$id" ||
    fail "$id is neither on the builder nor in the registry: the purge dropped an image that was never pushed"
done
# The cache the purge must not have cost. Reported by the daemon rather than
# read off disk, because the bytes live in overlay2 rather than under
# /var/lib/docker/buildkit and du on the wrong directory would say zero.
cache_bytes=$(curl -sS --connect-timeout 5 --max-time 60 "$E2E_BUILDER/system/df" |
  jq -r '[.BuildCache[]?.Size] | add // 0')
[[ "$cache_bytes" =~ ^[0-9]+$ ]] ||
  fail "could not read the builder's build cache size from /system/df (got '$cache_bytes')"
(( cache_bytes > 0 )) ||
  fail "the builder's BuildKit cache is empty after building four challenges: image removal must not reach it, or every rebuild re-runs the shared apt and pip layers"
ok "the builder holds no challenge images, the registry holds all four, and $(( cache_bytes / 1024 / 1024 ))MB of layer cache survived the purge"

############################################################################
# BLOCK 1 -- full-mode step, goes after the registry step's ok (line 658)
############################################################################

if (( FULL )); then
  step "cold workers: with every $CH_ONDEMAND image deleted from both boxes, the next launch on each pulls the content tag from zot inside the request"
  # Workers keep images until docker-reaper sweeps them, and run.sh never takes
  # the fleet down with -v, so on a box that has run this scenario before the
  # "each worker holds the image it launched from" check in the step below can
  # be satisfied by last week's leftover. Rather than sharpen a step that also
  # runs in regular mode, this one makes the claim itself and hands the fleet
  # back warm: delete the repo, prove it is gone, launch, prove the tag is back
  # and that the instance serves this build's flag.
  (( ${#OD_IDS[@]} == 0 )) ||
    fail "${#OD_IDS[@]} on-demand instance(s) are tracked at this point: this step deletes every $CH_ONDEMAND image from both workers, which a daemon refuses while a container of that image runs. The step belongs before the on-demand launches, not after them"
  COLD_REF="$E2E_REGISTRY/$CH_ONDEMAND:${IMAGE_TAG[$CH_ONDEMAND]}"
  for ip in "${WORKER_IPS[@]}"; do
    # Every generation of the repo, not only the current tag: an earlier full
    # run leaves two or three behind, and the negative control below reads the
    # repo prefix rather than one tag.
    tags=$(worker_tags "$ip")
    for ref in $(grep "^$E2E_REGISTRY/$CH_ONDEMAND:" <<<"$tags" || true); do
      # Deliberately not force=true: a 409 means a container still uses the
      # image, i.e. something the fleet-wait sweep could not see is running,
      # and forcing it out from under that container would hide the problem
      # instead of reporting it.
      code=$(worker_status "$ip" "/images/$ref" -X DELETE)
      case "$code" in
        200|404) ;;
        409) fail "worker $ip refuses to delete $ref because a container still uses it: sweep_worker only removes containers labelled cmgr.managed=true (scenario.sh), so a run killed before its EXIT trap ran may have left one behind. This run cannot be made cold" ;;
        *) fail "deleting $ref from worker $ip answered HTTP $code" ;;
      esac
    done
  done
  # The negative control. Without it every assertion below passes just as well
  # on a warm box and the step proves only that a cache hit works.
  for ip in "${WORKER_IPS[@]}"; do
    tags=$(worker_tags "$ip")
    if grep -q "^$E2E_REGISTRY/$CH_ONDEMAND:" <<<"$tags"; then
      fail "worker $ip still holds an image of $CH_ONDEMAND after the delete: imagePresent would skip the pull (cmgr/launch.go:135-141) and the launches below would prove nothing about a pull"
    fi
  done
  note "deleted every $E2E_REGISTRY/$CH_ONDEMAND image from both workers; both are cold for $COLD_REF"

  # One launch at a time, not two at once. Two cold pulls of the same
  # ubuntu:24.04-based image into two dinds on one host contend for the same
  # disk and are the likeliest way to push a pull past the 30s default
  # pullTimeout (compose sets no CMGR_WORKER_PULL_TIMEOUT) -- which would be a
  # legitimate 503 the fleet is entitled to, not a regression, and so a bad
  # thing to hang a step on. Sequential also gets its placement from the
  # invariant the harness already relies on: two consecutive launches over two
  # ok workers land one each, because selectWorker advances its cursor per
  # placement (cmgr/workers.go, as ensure_instance_on and the autoscaling step
  # both assume).
  COLD_INST=""; COLD_CODE=""; COLD_TOOK=0
  cold_launch() { # cold_launch <user index>: one on-demand launch, past a transient refusal
    local i=$1 attempt out s
    for attempt in 1 2 3; do
      s=$(date +%s)
      out=$(try_launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
      COLD_TOOK=$(( $(date +%s) - s ))
      COLD_CODE=${out%%$'\n'*}
      COLD_INST=${out#*$'\n'}
      case "$COLD_CODE" in
        200|201) return ;;
        503)
          # Every 503 here is retryable by contract (cmd/cmgrd/main.go:447-452)
          # and none of them is this step's subject: a momentarily overloaded
          # box, a busy launch queue, or a pull that ran out of time on a
          # loaded host. Say which it was, wait for the fleet, ask again. The
          # ErrPullTimeout case is still covered, by the health check below:
          # a timed-out pull must not have cost the worker its place.
          note "the launch for e2e-user-$i was refused as retryable after ${COLD_TOOK}s ($(head -c 140 <<<"$COLD_INST")); waiting for both workers and trying again"
          retry 30 "both workers to report ok before retrying the cold launch" all_workers_ok
          ;;
        *)
          fail "the cold launch for e2e-user-$i answered HTTP $COLD_CODE after ${COLD_TOOK}s: a launch onto a worker that does not hold the build's image must pull it from $E2E_REGISTRY under the worker's own certs.d identity and start ($(head -c 200 <<<"$COLD_INST"))"
          ;;
      esac
    done
    fail "three launches for e2e-user-$i in a row were refused as retryable, the last after ${COLD_TOOK}s: the fleet never got far enough to attempt the cold pull this step is about ($(head -c 200 <<<"$COLD_INST"))"
  }

  retry 30 "both workers to report ok before the cold launches" all_workers_ok
  for i in 41 42; do
    cold_launch "$i"
    track "$COLD_INST" "$i"
    note "instance $(jq -r .id <<<"$COLD_INST") on $(jq -r .worker <<<"$COLD_INST") after ${COLD_TOOK}s, its image pulled inside the request"
  done
  # Placement is not the subject here: if a worker flipped overloaded in the
  # window between the health read and the second placement, both landed on one
  # box. That costs one more launch, not the step -- ensure_instance_on is the
  # harness's own way of getting an instance onto a named worker.
  for ip in "${WORKER_IPS[@]}"; do
    if [[ -z "$(instance_on "$ip")" ]]; then
      note "both cold launches landed on the same worker; one more so $ip pulls too"
      ensure_instance_on "$ip" 43
    fi
  done

  # The load-bearing pair: the repo was provably empty a moment ago and holds
  # the exact content tag now, on a daemon nothing but ensureImages writes
  # images to. No registry credentials are configured in this fleet
  # (CMGR_REGISTRY_USER/TOKEN are unset, so authString carries an empty
  # username and password, cmgr/docker.go:95-103), which leaves the worker's own
  # read-only CN=worker certs.d identity as the only thing that can have
  # authorized the pull.
  for ip in "${WORKER_IPS[@]}"; do
    has_line "$COLD_REF" worker_tags "$ip" ||
      fail "worker $ip does not hold $COLD_REF after launching an instance of that build from an empty image store: ensureImages did not pull it (cmgr/launch.go:126-153)"
  done
  # And the pull fetched this build's content, not merely something with the
  # right name: the instance serves the build's flag and its own environment.
  for id in "${OD_IDS[@]}"; do
    check_ondemand "${INST_META[$id]}" "${INST_USER[$id]}"
  done
  # A pull that failed must never cost the worker its place, and a pull that
  # succeeded certainly must not: only ErrPullTimeout is exempt from
  # noteWorkerTransportError (cmgr/launch.go:142-150), so a regression there
  # shows up as a box that pulled and was downed for it. down is sticky and
  # only a worker-add clears it, so this read is the whole assertion; the retry
  # afterwards is for overloaded, which a big pull can cause and which clears
  # by itself.
  for ip in "${WORKER_IPS[@]}"; do
    ! health_is "$ip" down ||
      fail "worker $ip was marked down by the pull it had to perform: a pull failure that is not ErrPullTimeout is offered to noteWorkerTransportError (cmgr/launch.go:142-150), and one slow registry would walk the whole fleet down until an operator re-added every box"
  done
  retry 20 "both workers to report ok after the cold pulls" all_workers_ok

  for id in "${OD_IDS[@]}"; do
    cmgrd-cli stop "$id"
    assert_gone "$id" "${INST_WORKER[$id]}" "${INST_META[$id]}"
  done
  OD_IDS=() # the next step counts its own four launches over an empty list
  # Both workers hold the tag again, exactly as a warm fleet would, so the step
  # after this one sees what it always sees. HAD_TAG was read before this step
  # deleted anything; correct it, or its note there claims a pull that will not
  # happen. Nothing else reads HAD_TAG, and nothing asserts on it.
  for ip in "${WORKER_IPS[@]}"; do HAD_TAG[$ip]=1; done
  ok "both workers started this step holding no $CH_ONDEMAND image at all and ended it holding $COLD_REF, pulled from zot inside the launch that needed it, serving the build's flag, with neither box downed for the pull"
else
  deselect "cold workers: the first launch on each box really pulls the image from zot"
fi

# ----------------------------------------------- 5. on-demand launches

step "on-demand launches: POST /builds/<id> with user_id and env, placed round robin, pulled from zot by the worker"
for i in 1 2 3 4; do
  inst=$(launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
  track "$inst" "$i"
  note "instance $(jq -r .id <<<"$inst") -> worker $(jq -r .worker <<<"$inst") (${PUBLIC[$(jq -r .worker <<<"$inst")]:-?}:$(jq -r .ports.server <<<"$inst"))"
done
declare -A PER_WORKER=()
for id in "${OD_IDS[@]}"; do
  w=${INST_WORKER[$id]}
  PER_WORKER[$w]=$(( ${PER_WORKER[$w]:-0} + 1 ))
done
for ip in "${WORKER_IPS[@]}"; do
  (( ${PER_WORKER[$ip]:-0} == 2 )) || fail "worker $ip received ${PER_WORKER[$ip]:-0} of 4 instances, expected 2"
  if ! has_line "$E2E_REGISTRY/$CH_ONDEMAND:${IMAGE_TAG[$CH_ONDEMAND]}" worker_tags "$ip"; then
    fail "worker $ip does not hold $E2E_REGISTRY/$CH_ONDEMAND:${IMAGE_TAG[$CH_ONDEMAND]} after launching from it"
  fi
  if [[ -n "${HAD_TAG[$ip]:-}" ]]; then note "worker $ip already had the image from an earlier run (no pull needed)"; else note "worker $ip pulled the image from the registry"; fi
done
ok "4 instances, 2 per worker; each worker holds the image it launched from"

step "on-demand instances: reachable at worker_public:port with the injected environment, containers on their workers"
for id in "${OD_IDS[@]}"; do
  check_ondemand "$(api GET "/instances/$id")" "${INST_USER[$id]}"
  note "instance $id: ${PUBLIC[${INST_WORKER[$id]}]} serves e2e-user-${INST_USER[$id]} with flag $OD_FLAG"
done
ok "every instance answers with its own user_id and env"

# ------------------------------- 5c. the runtime shim is really in the path

step "oci-interceptor: the runtime shim production runs is in the create path, and the networking mounts it makes read-only really are"
# /info reporting the default runtime says only that dockerd was configured
# with it. This says the interceptor actually ran for a container cmgrd
# created, because the only thing that makes these three mounts read-only is
# the interceptor rewriting the OCI spec on the way past.
#
# The property is not cosmetic. Docker bind-mounts /etc/hosts, /etc/hostname
# and /etc/resolv.conf read/write, and where a container's writable layer is
# capped by an XFS project quota those three files are outside it -- an escape
# hatch for filling the host volume from inside a challenge. The mounts are
# left in place rather than removed: docker points a container on a
# user-defined network at its embedded resolver by writing
# "nameserver 127.0.0.11" into that bind-mounted resolv.conf, so removing it
# would break resolution of sibling containers by name.
oci_id=${OD_IDS[0]}
oci_meta=${INST_META[$oci_id]}
oci_worker=${INST_WORKER[$oci_id]}
oci_cid=$(jq -r '.containers[0]' <<<"$oci_meta")
mounts=$(exec_in "$oci_worker" "$oci_cid" root \
  sh -c "grep -E ' /etc/(hosts|hostname|resolv[.]conf) ' /proc/mounts") ||
  fail "could not read /proc/mounts inside container $oci_cid of instance $oci_id on $oci_worker"
for path in /etc/hosts /etc/hostname /etc/resolv.conf; do
  opts=$(awk -v p="$path" '$2 == p {print $4}' <<<"$mounts")
  [[ -n "$opts" ]] ||
    fail "$path is not a mount inside container $oci_cid of instance $oci_id: expected docker's bind mount (seen: $(tr '\n' ';' <<<"$mounts"))"
  [[ "$opts" == ro,* || "$opts" == ro ]] ||
    fail "$path is mounted '$opts' inside container $oci_cid of instance $oci_id, not read-only: oci-interceptor did not rewrite this container's spec, so it is not in the create path at all. Note this cannot tell you whether docker generated a wrapper for it -- the generated wrapper still invokes the interceptor and the spec is still rewritten. The fleet-wait step is what checks that, on the registered runtime"
done
# And the effect a competitor would meet. The container reports the verdict
# either way, so an exec that never ran (a wedged daemon, a container that has
# exited, an image without /bin/sh) is an empty answer and fails here rather
# than passing for the silence.
wrote=$(exec_in "$oci_worker" "$oci_cid" root \
  sh -c 'if echo probe >> /etc/hosts 2>/dev/null; then echo WROTE; else echo REFUSED; fi' || true)
case "$wrote" in
  *WROTE*) fail "a write to /etc/hosts inside container $oci_cid of instance $oci_id succeeded although the mount reports read-only" ;;
  *REFUSED*) : ;;
  *) fail "the write probe inside container $oci_cid of instance $oci_id answered neither WROTE nor REFUSED ('$wrote'): the exec did not run, so this step proved nothing about the mount being enforced" ;;
esac
ok "instance $oci_id went through oci-interceptor on $oci_worker: /etc/hosts, /etc/hostname and /etc/resolv.conf are all mounted read-only in its container, the mounts are still present (so docker's embedded resolver still works), and a write to /etc/hosts was refused"

############################################################################
# BLOCK 2 -- full-mode step, goes after the on-demand-instances ok (line 687)
############################################################################

if (( FULL )); then
  step "registry identities: the worker's certificate pulls from zot but may not push to it, and a client with no certificate is refused outright"
  # Every probe here is driven through $WA's own dockerd, which carries the
  # worker's certs.d identity (CN=worker, read-only in e2e/zot/config.json) --
  # the same files, over the same identity, a pull travels with. Doing it this
  # way needs no compose change and, more to the point, tests the identity a
  # worker actually has rather than one the scenario mints for the occasion.
  # cork's read-write certificate is used only to read the registry back.
  probe_src="$E2E_REGISTRY/$CH_ONDEMAND:${IMAGE_TAG[$CH_ONDEMAND]}"
  probe_ref="$E2E_REGISTRY/$PROBE_REPO:$PROBE_TAG"
  has_line "$probe_src" worker_tags "$WA" ||
    fail "worker $WA does not hold $probe_src although the launches above pulled it there: this step needs an image to offer the registry"

  # worker_probe: try_launch's shape for a worker's docker API -- status on the
  # first line, body after it, a transport failure reported as 000 rather than
  # raised, because a push that dies mid-stream is a legitimate outcome here
  # and must not abort the run under errexit. worker_curl hardcodes
  # --max-time 15; curl takes the last occurrence of a repeated option, so the
  # callers below override it for the two calls that talk to the registry.
  worker_probe() { # worker_probe <ip> <path> [curl args...]
    local ip=$1 path=$2 out
    shift 2
    out=$(worker_curl "$ip" "$path" -w '\n%{http_code}' "$@") || out=$'\n000'
    printf '%s\n%s\n' "${out##*$'\n'}" "${out%$'\n'*}"
  }

  # The positive control for the identity, and it has to come first: without it
  # the refusal below would prove nothing more than a broken connection or a
  # misplaced certificate. dockerd re-resolves the manifest with the registry
  # even for a tag it already holds, so this is a real authenticated read.
  out=$(worker_probe "$WA" "/images/create?fromImage=$E2E_REGISTRY/$CH_ONDEMAND&tag=${IMAGE_TAG[$CH_ONDEMAND]}" -X POST --max-time 120)
  code=${out%%$'\n'*}
  body=${out#*$'\n'}
  { [[ "$code" == 200 ]] && ! grep -qiE 'errorDetail|denied|unauthorized' <<<"$body"; } ||
    fail "worker $WA cannot read $probe_src from $E2E_REGISTRY with its own certificate (HTTP $code): the \"worker\" identity is granted read on every repository (e2e/zot/config.json), and without it no launch on a cold worker could pull at all ($(head -c 200 <<<"$body"))"

  # The write attempt, under a repository name in the challenges' own namespace
  # so zot judges it by exactly the "**" policy that guards the real ones.
  code=$(worker_status "$WA" "/images/$probe_src/tag?repo=$E2E_REGISTRY/$PROBE_REPO&tag=$PROBE_TAG" -X POST)
  [[ "$code" == 201 ]] || fail "could not tag $probe_src as $probe_ref on worker $WA: HTTP $code"
  PROBE_WORKER=$WA # from here the EXIT trap untags it if the run dies
  # X-Registry-Auth: e30= is base64 "{}", the empty auth config a
  # credential-less docker push sends; zot authenticates by certificate only
  # (http.auth.mtls), so the certs.d client cert is the whole identity.
  out=$(worker_probe "$WA" "/images/$E2E_REGISTRY/$PROBE_REPO/push?tag=$PROBE_TAG" -X POST -H 'X-Registry-Auth: e30=' --max-time 180)
  push_code=${out%%$'\n'*}
  push_body=${out#*$'\n'}

  # The authoritative assertion, read back with cork's identity and independent
  # of how dockerd words anything: a push that got through leaves the tag, or --
  # if it died between the blobs and the manifest -- the repository. Cleaned up
  # before failing, so a fleet with this regression is still usable afterwards.
  if has_line "$PROBE_TAG" registry_tags "$PROBE_REPO"; then
    registry_status "/v2/$PROBE_REPO/manifests/$PROBE_TAG" -X DELETE >/dev/null 2>&1 || true
    fail "the worker's read-only certificate pushed $probe_ref into $E2E_REGISTRY: zot grants the \"worker\" identity read only (e2e/zot/config.json accessControl), and a worker that can write the registry can replace any challenge image the whole fleet then pulls"
  fi
  [[ "$(registry_status /v2/_catalog)" == 200 ]] ||
    fail "cork's own certificate cannot read $E2E_REGISTRY/v2/_catalog, so this step cannot tell whether the worker's push created a repository"
  # A repository may appear in the catalog without the policy having leaked:
  # zot initialises a repository directory as a side effect of reads the worker
  # identity is entitled to make, and an empty one publishes nothing. Verified
  # directly against this fleet: with the worker's certificate,
  # POST /v2/<repo>/blobs/uploads/ answers 403 and creates no catalog entry,
  # while GET /v2/_catalog answers 200. So what matters is that the repository
  # is EMPTY -- a tag is the only thing another box would ever pull.
  if registry_api /v2/_catalog | jq -e --arg r "$PROBE_REPO" '.repositories // [] | index($r)' >/dev/null; then
    probe_tags=$(registry_tags "$PROBE_REPO" | tr '\n' ' ' | sed 's/ $//')
    [[ -z "$probe_tags" ]] ||
      fail "the worker's push left tag(s) '$probe_tags' under $PROBE_REPO in $E2E_REGISTRY: a worker that can write the registry can replace any challenge image the whole fleet then pulls"
    note "zot left an empty $PROBE_REPO repository behind the refused push; it holds no tag, so nothing is publishable through it"
  fi
  # Second, weaker, and deliberately so: that the write was REFUSED rather than
  # merely lost. The wording is dockerd's and zot's, not cork's, so pinning an
  # exact string here would pin another project's strings; the two assertions
  # above are the ones that cannot be right for the wrong reason.
  grep -qiE 'denied|unauthorized|forbidden|errorDetail' <<<"$push_body" ||
    fail "the push of $probe_ref with the worker's certificate answered HTTP $push_code and named no refusal: nothing reached the registry, but neither did anything refuse it, so the probe did not exercise the policy ($(head -c 200 <<<"$push_body"))"
  note "the worker's certificate was refused the push: $(tr -d '\r' <<<"$push_body" | grep -iEm1 'denied|unauthorized|forbidden|errorDetail' | head -c 160)"

  code=$(worker_status "$WA" "/images/$probe_ref?noprune=1" -X DELETE)
  case "$code" in
    200|404) ;;
    *) fail "could not untag $probe_ref from worker $WA: HTTP $code" ;;
  esac
  PROBE_WORKER=""
  # noprune, plus the original tag still being there, means only the name went.
  # If the image itself went, the instances running from it are on borrowed
  # time and every later plant_orphan on this box would fail.
  has_line "$probe_src" worker_tags "$WA" ||
    fail "untagging the probe removed $probe_src from worker $WA: the image this run's instances launched from is gone"

  # No client certificate at all. zot's identity model is mTLS only
  # (http.auth.mtls with a configured cacert, defaultPolicy [] and
  # anonymousPolicy []), so the refusal may land at the TLS handshake -- curl
  # fails and %{http_code} is 000 -- or come back as a 401/403. Which one it is
  # is zot's business; a 200 is the only outcome that matters, and it would mean
  # every challenge image and flag-bearing layer is readable by anything that
  # can reach the registry.
  anon_status() { # anon_status <path>: status with the CA only, 000 for a TLS-level refusal
    local out
    out=$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 15 \
      --cacert "$REGISTRY_CERT_DIR/ca.crt" "https://$E2E_REGISTRY$1" 2>/dev/null) || true
    if [[ -z "$out" ]]; then out=000; fi
    printf '%s' "$out"
  }
  for path in "/v2/" "/v2/$CH_ONDEMAND/tags/list" "/v2/_catalog"; do
    # The positive control for the same path first: with cork's certificate it
    # answers 200, so the refusal below is about the identity and not about the
    # path being wrong or the registry being down.
    ccode=$(registry_status "$path")
    [[ "$ccode" == 200 ]] ||
      fail "cork's read-write certificate got HTTP $ccode from $path, so an anonymous refusal there would say nothing about identity"
    acode=$(anon_status "$path")
    case "$acode" in
      000) note "anonymous GET $path: refused at the TLS handshake" ;;
      401|403) note "anonymous GET $path: HTTP $acode" ;;
      *) fail "an anonymous client got HTTP $acode from $E2E_REGISTRY$path: zot's defaultPolicy and anonymousPolicy are both empty (e2e/zot/config.json), so nothing without a certificate may read the registry" ;;
    esac
  done
  ok "the worker's certificate read $probe_src from zot and was refused the push of $probe_ref, which left neither tag nor repository behind; an anonymous client was refused /v2/, the tag list and the catalog, all three of which cork's own certificate reads"
else
  deselect "registry identities: the worker's certificate is read-only and anonymous access is refused"
fi

# ------------------------------------------------- 6b. launch slot burst

# --------------------------------------------------- 5b. launch burst

step "launch burst: with one worker down, a burst on the other fills its launch slots and the overflow is refused at once as a retryable 503"
# Both workers already hold the on-demand image (the four launches above
# pulled it on each), so a launch slot here covers the network create, the
# container create and start, and the port read-back only: pulls run outside
# the slot (launch.go), so no pull time is smeared into the hold and the 2s
# launch wait bounds a real queue instead of racing a cold pull. That is why
# this step sits immediately after the launches that warmed both workers.
BURST_N=18 # at most 18 of $WB's 30 ports are held at once, on top of the 2 already there
DOWNED_WORKER=$WA # set before the call, so the trap owes a worker-add even if down_worker's own check fails
down_worker "$WA" # $WB is the only worker up, so every launch below is placed there
burst_before=$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip==$ip) | .instances')
[[ "$burst_before" =~ ^[0-9]+$ ]] || fail "could not read the instance count of worker $WB before the burst"
burst_dir=$(mktemp -d)
# A release barrier, because the depth of the queue at the moment the requests
# land is the whole subject of this step. Spawning N subshells in a loop smears
# their arrivals over however long the shell takes to fork N curls, and every
# millisecond of that smear is a millisecond the two slots get to drain before
# the next request turns up -- so the harness's own fork rate, which has nothing
# to do with cork, silently sets how deep the queue gets. Each subshell is
# spawned parked on a flag file and they are released together.
#
# A flag file rather than a FIFO: a reader that reaches its open() after the
# writer has opened and closed blocks forever, and a subshell that loses its
# slice for long enough would hang the run. A late reader here still sees the
# flag. The poll costs at most one sleep of skew, two orders of magnitude under
# the per-launch hold this is trying to outrun.
burst_gate="$burst_dir/gate"
for (( i = 30; i < 30 + BURST_N; i++ )); do
  # One subshell per request, each timed and captured whole. It ends on a
  # printf, so a curl that never answered cannot escape into `wait` and abort
  # the run under set -e: it is reported as the missing status it is.
  (
    while [[ ! -e "$burst_gate" ]]; do sleep 0.01; done
    s=$(date +%s)
    out=$(try_launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i") || out=$'000\ncurl did not answer'
    printf '%s\n%s\n' "$(( $(date +%s) - s ))" "$out"
  ) >"$burst_dir/$i" &
done
sleep 1 # every subshell reaches its poll; forking N of them is the slow part
t=$(date +%s)
touch "$burst_gate"
wait
# Read the health first, before anything else can spend time: refusing a
# launch touches no daemon at all, so a saturated queue must never look like
# a wedged one. down is sticky, so this single read is the whole assertion.
burst_health=$(worker_health "$WB")
note "$BURST_N launches fired at $WB, all answered in $(( $(date +%s) - t ))s"
[[ "$burst_health" != down ]] ||
  fail "worker $WB was marked down by a burst it merely refused: the launch queue is cmgrd's own (daemonQueue), and only two of its launches ever reach docker at once, so nothing here can reach a control timeout"

burst_ids=()
burst_503=0 burst_slot=0 burst_admit=0 burst_odd=0 burst_worst=0
for (( i = 30; i < 30 + BURST_N; i++ )); do
  took=$(sed -n 1p "$burst_dir/$i")
  code=$(sed -n 2p "$burst_dir/$i")
  body=$(sed -n '3,$p' "$burst_dir/$i")
  case "$code" in
    200|201)
      [[ "$(jq -r .worker <<<"$body")" == "$WB" ]] ||
        fail "burst launch for e2e-user-$i landed on $(jq -r .worker <<<"$body"), not on the only worker that was up ($WB)"
      track "$body" "$i"
      burst_ids+=("$(jq -r .id <<<"$body")")
      ;;
    503)
      burst_503=$(( burst_503 + 1 ))
      if (( took > burst_worst )); then burst_worst=$took; fi
      if grep -q "worker went down" <<<"$body"; then
        fail "the burst answered e2e-user-$i with a worker-down refusal after ${took}s: a busy daemon must be reported busy (ErrWorkerBusy), not given up on ($body)"
      fi
      if grep -qE "no launch slot on worker $WB for instance [0-9]+ within" <<<"$body"; then
        burst_slot=$(( burst_slot + 1 ))       # acquireSlot's expired arm, launch.go:243
      elif grep -q "launches already waiting on worker $WB" <<<"$body"; then
        burst_admit=$(( burst_admit + 1 ))     # admit refused before any record, launch.go:189
      else
        # Still retryable (a lost race for the database write lock is one),
        # but not the busy refusal this step is about.
        burst_odd=$(( burst_odd + 1 ))
        note "e2e-user-$i: 503 after ${took}s that is not a busy refusal: $(head -c 120 <<<"$body")"
      fi
      ;;
    *)
      fail "burst launch for e2e-user-$i answered HTTP ${code:-<none>} after ${took:-?}s: every launch of a burst is either accepted or refused as a retryable 503, never a 500 and never left unanswered ($(head -c 200 <<<"$body"))"
      ;;
  esac
done
note "${#burst_ids[@]} accepted, $burst_slot refused for want of a slot, $burst_admit refused on admission, $burst_odd other 503(s)"
(( ${#burst_ids[@]} > 0 )) ||
  fail "not one of the $BURST_N launches on $WB was accepted: $WB was wedged, not busy"
(( burst_503 > 0 )) ||
  fail "all $BURST_N launches on $WB were accepted, so nothing here exercised the refusal: either its two launch slots emptied faster than the burst filled them (raise BURST_N; CMGR_PORTS caps it at $(( PORT_HIGH - PORT_LOW + 1 ))), or cork is not running with CMGR_WORKER_LAUNCH_WAIT=$LAUNCH_WAIT_H"
# The sharp one, and the only form-independent one that can be: the overflow
# must come back as ErrWorkerBusy, by either of its two wordings. Which one
# appears is a race between admit's view of the queue and how fast the
# requests arrive, so demanding a particular wording would fail on a fleet
# that is behaving perfectly; demanding neither would pass on a cmgrd that
# answered 503 for some unrelated reason.
(( burst_slot + burst_admit > 0 )) ||
  fail "$burst_503 launch(es) on $WB were refused without naming the busy refusal: an overflowing launch queue must fail with ErrWorkerBusy, which is what cmgrd answers 503 + Retry-After to (cmd/cmgrd/main.go:447)"
# The bound, which is what actually bites: a refusal costs its wait and no
# more. An unbounded wait accepts the overflow late instead of refusing it,
# and the 10s default would show here as a ~10s refusal; 6s leaves room for
# the pre-slot work (three sqlite writes and one image inspect, all under
# $BURST_N-way contention) on top of the wait itself.
(( burst_worst <= 6 )) ||
  fail "a burst launch was refused only after ${burst_worst}s: a refusal must arrive within the $LAUNCH_WAIT_H launch wait plus overhead, never after queueing behind the daemon (cmgr/launch.go acquireSlot)"
# ok, not merely not-down: a burst that spikes the host's CPU can leave the
# telemetry agent reporting overloaded for a sample or two, which clears by
# itself, so this one is given a moment.
retry 20 "worker $WB to read ok again after the burst" health_is "$WB" ok

# Every accepted launch really ran: its containers are on $WB, and the first
# of them serves its own environment. One full check_ondemand is enough --
# what is new here is the burst, not the instance.
check_ondemand "${INST_META[${burst_ids[0]}]}" "${INST_USER[${burst_ids[0]}]}"
for id in "${burst_ids[@]}"; do
  assert_on_worker "$WB" "${INST_META[$id]}"
done
burst_after=$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip==$ip) | .instances')
(( burst_after == burst_before + ${#burst_ids[@]} )) ||
  fail "worker $WB records $burst_after instances after the burst, expected $(( burst_before + ${#burst_ids[@]} )) ($burst_before before it, ${#burst_ids[@]} accepted): a launch refused after its row was opened left a hollow record and its reserved ports behind (clearInstanceRecords in cmgr/api.go)"

burst_stops=$(mktemp -d)
for id in "${burst_ids[@]}"; do
  api_status DELETE "/instances/$id" >"$burst_stops/$id" &
done
wait
for id in "${burst_ids[@]}"; do
  code=$(cat "$burst_stops/$id")
  [[ "$code" == 204 ]] || fail "stopping burst instance $id on $WB answered HTTP $code"
  assert_gone "$id" "$WB" "${INST_META[$id]}"
  forget "$id"
done
rm -rf "$burst_dir" "$burst_stops"
burst_after=$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip==$ip) | .instances')
(( burst_after == burst_before )) ||
  fail "worker $WB records $burst_after instances with the burst's instances stopped, expected the $burst_before it started with"
readd "$WA"
DOWNED_WORKER=""
ok "$BURST_N launches at $WB: ${#burst_ids[@]} accepted and on the worker, $(( burst_slot + burst_admit )) refused as a busy 503 within ${burst_worst}s, $WB never down, no record left behind, and $WA re-added"

# --------------------------------------------------- 6c. launch backlog

# ----------------------------------------- 5b. launch admission control

step "launch backlog: once a worker's launch queue is deeper than the launch wait allows, further launches are refused at once, before a row exists"
# admit (cmgr/launch.go, a53e8d6) sits between placement and openInstance in
# newInstance: a launch whose worker's queue would evidently outwait
# CMGR_WORKER_LAUNCH_WAIT is refused there, so it costs no instance row, no
# port claim and no docker round trip. The refusal it must never decay back
# into is the one acquireSlot issues after the full wait: same error, same
# 503, but 2s later and with an id already burned. Telling those two apart is
# the whole step, and the id arithmetic at the end is the half that still
# bites if the wording of either message changes.
BURST_N=32         # wave one: round robin puts 16 on each of the two workers
PROBE_N=8          # wave two, fired while those queues are at their deepest
# Admission decides on an estimate, before any I/O, so it must come back well
# inside the wait a slot refusal would have cost. Three quarters of it: loose
# enough for an HTTP round trip on a loaded box, tight enough that a refusal
# which actually queued for a slot cannot pass as one.
ADMIT_MAX_MS=$(( LAUNCH_WAIT_MS * 3 / 4 ))
burst=$(mktemp -d)
all_workers_ok ||
  fail "both workers must be ok before the burst: the queue depth below assumes placement round robins over two of them"
BURST_BODY=$(jq -cn '{user_id: "e2e-user-burst", env: {CUSTOM_VAR: "e2e-value-burst"}}')

# Everything the burst starts is stopped again inside this step, so the two
# workers must look exactly as they do now when it is over. Containers and
# networks are the docker side; the recorded instance count is cmgrd's.
docker_objects() { containers_on "$1" | sort; worker_api "$1" /networks | jq -r '.[].Name' | sort; }
state_a=$(docker_objects "$WA")
state_b=$(docker_objects "$WB")
rows_before=$(api GET /workers | jq -r 'map(.instances) | add')
# The instances cmgrd records for this build right now. Anything of this build
# it records at the end and did not record here is the burst's and gets
# stopped -- including an instance whose 201 never reached us, which a list of
# ids scraped from the responses would strand on a worker.
od_instances() {
  api GET "/schemas/$SCHEMA_NAME" |
    jq -r --arg c "$CH_ONDEMAND" '.[] | select(.id == $c) | .builds[].instances[]?.id'
}
before_ids=$(od_instances)

# The probes bracket the burst. Instance ids are AUTOINCREMENT (1ef7502), so
# the gap between the two probe ids counts exactly the ids the burst consumed,
# and a launch refused by admission must consume none.
timed_launch "$burst" probe1 "$OD_BUILD" "$BURST_BODY"
code=000; t=0
read -r code t <"$burst/probe1.code" || true
[[ "$code" == 200 || "$code" == 201 ]] ||
  fail "the probe launch before the burst answered HTTP $code: $(head -c 200 "$burst/probe1.body")"
probe1=$(jq -r .id <"$burst/probe1.body")
[[ "$probe1" =~ ^[0-9]+$ ]] || fail "the probe launch before the burst returned no instance id: $(head -c 200 "$burst/probe1.body")"

for ((i = 0; i < BURST_N; i++)); do
  timed_launch "$burst" "b$i" "$OD_BUILD" "$BURST_BODY" &
done
# Wave two has to arrive while wave one is still queued, and the queue itself
# is not observable from outside. The recorded instance count is: openInstance
# creates a launch's row immediately before ensureImages and the slot wait, and
# only a refusal removes one again, so the count stops climbing exactly when
# the last of wave one has either joined the queue or been refused -- which is
# the moment the queue is at its deepest. That plateau, not a guessed sleep, is
# what releases wave two. If it never settles the wave goes out at the poll
# ceiling anyway and the assertions below judge whatever came back.
rows=$rows_before; prev=-1; polls=0
while (( polls < 60 )); do
  polls=$(( polls + 1 ))
  n=$(api GET /workers | jq -r 'map(.instances) | add' || echo "")
  [[ "$n" =~ ^[0-9]+$ ]] || continue
  rows=$n
  if (( n == prev && n >= rows_before + BURST_N / 2 )); then break; fi
  prev=$n
done
note "$(( rows - rows_before )) of $BURST_N wave-one launches were recorded after $polls polls; firing $PROBE_N more"
for ((i = BURST_N; i < BURST_N + PROBE_N; i++)); do
  timed_launch "$burst" "b$i" "$OD_BUILD" "$BURST_BODY" &
done
wait

n2xx=0; nadmit=0; nslot=0; nother=0
admit_times=()
for ((i = 0; i < BURST_N + PROBE_N; i++)); do
  code=000; t=0
  read -r code t <"$burst/b$i.code" || true
  [[ -n "$code" ]] || code=000
  body=$(cat "$burst/b$i.body" 2>/dev/null || true)
  case "$code" in
    200|201) n2xx=$(( n2xx + 1 )) ;;
    503)
      # Two 503s come out of a saturated worker and only one of them is this
      # step's subject. admit names the worker whose queue it read and how
      # many launches are waiting on it; acquireSlot names the instance it had
      # already created ("no launch slot on ... for instance <id> within ...").
      if grep -Eq "launches already waiting on worker ($WA|$WB)" <<<"$body"; then
        nadmit=$(( nadmit + 1 ))
        admit_times+=("$t")
        # Retryable like every other launch refusal, or the platform drops the
        # request instead of placing it afresh (cmd/cmgrd/main.go).
        grep -qi '^Retry-After: 1' "$burst/b$i.head" ||
          fail "the admission refusal of burst launch b$i carried no Retry-After: $(tr -d '\r' <"$burst/b$i.head" | head -1)"
      elif grep -q "no launch slot on " <<<"$body"; then
        nslot=$(( nslot + 1 ))
      else
        nother=$(( nother + 1 ))
        if (( nother == 1 )); then note "burst launch b$i: a 503 from neither admission nor the slot wait: $(head -c 160 <<<"$body")"; fi
      fi
      ;;
    *)
      nother=$(( nother + 1 ))
      if (( nother == 1 )); then note "burst launch b$i: HTTP $code $(head -c 160 <<<"$body")"; fi
      ;;
  esac
done
note "$(( BURST_N + PROBE_N )) burst launches: $n2xx started, $nadmit refused by admission, $nslot refused after the full $LAUNCH_WAIT_H wait, $nother other"

# The substitution is the regression, and a slot refusal is what proves the
# queue really was deeper than the wait: if one launch sat there for the whole
# wait, the launches that arrived behind it should have been turned away on
# sight. A burst that produced neither refusal never saturated anything and
# has tested nothing, which is a broken test rather than a broken cork -- so
# it says so separately, and names the knob.
if (( nadmit == 0 && nslot > 0 )); then
  fail "$nslot burst launches waited the full $LAUNCH_WAIT_H for a slot and not one was refused ahead of it: admit (cmgr/launch.go, a53e8d6) no longer runs before openInstance, so every refusal fell back to acquireSlot's 'no launch slot on ... within' form"
fi
if (( nadmit == 0 )); then
  fail "the burst of $(( BURST_N + PROBE_N )) launches never queued deeply enough to refuse anything ($n2xx started, $nother other): the fleet worked them off faster than the estimate admit reads, so nothing was proved here. Raise BURST_N (CMGR_PORTS caps it near 25 per worker) or lower CMGR_WORKER_LAUNCH_WAIT"
fi
# Half the wait would do; three quarters because these are 40 concurrent curls
# in one container on a box that is also starting containers, and the message
# already said which refusal each of these was. A refusal that really waited for
# a slot cannot come back inside the wait at all.
slowest=$(printf '%s\n' "${admit_times[@]}" | jq -s -r '(. + [0] | max) * 1000 | floor' || echo "")
[[ "$slowest" =~ ^[0-9]+$ ]] ||
  fail "could not read the response times of the $nadmit admission refusals from curl: '$(printf '%s ' "${admit_times[@]}")'"
(( slowest < ADMIT_MAX_MS )) ||
  fail "the slowest of $nadmit admission refusals took ${slowest}ms against a $LAUNCH_WAIT_H launch wait: it waited for a slot instead of being refused on the estimate"

# Let the fleet read ok again first, exactly as the launch-burst step does
# after its own burst. Forty concurrent launches spike the host's CPU, and on
# a small box the telemetry agents report overloaded for a sample or two --
# placement then skips every worker and the probe comes back "all workers are
# overloaded", which says nothing about the ids this step is counting. It
# costs the assertion nothing to wait: no id is consumed while the fleet
# settles, because nothing else is launching.
retry 30 "the fleet to read ok again after the burst" all_workers_ok
timed_launch "$burst" probe2 "$OD_BUILD" "$BURST_BODY"
code=000; t=0
read -r code t <"$burst/probe2.code" || true
[[ "$code" == 200 || "$code" == 201 ]] ||
  fail "the probe launch after the burst answered HTTP $code: $(head -c 200 "$burst/probe2.body")"
probe2=$(jq -r .id <"$burst/probe2.body")
[[ "$probe2" =~ ^[0-9]+$ ]] || fail "the probe launch after the burst returned no instance id: $(head -c 200 "$burst/probe2.body")"
consumed=$(( probe2 - probe1 - 1 ))
(( consumed >= n2xx )) ||
  fail "the burst started $n2xx instances but only $consumed instance ids passed between the probes ($probe1 -> $probe2): ids are being reused (1ef7502)"
# The load-bearing assertion. Every launch that reached openInstance burned an
# id, whether it went on to start or was refused at the slot and rolled back;
# a launch refused by admission burned none. So the ids the burst consumed can
# never exceed the launches admission let through -- and were admit moved
# behind openInstance, they would be exactly the launches fired, which neither
# the message nor the timing check would necessarily notice. The equality is
# asserted only when nothing else went wrong, since a launch that died before
# its INSERT (a busy database) also burns no id.
if (( nother == 0 )); then
  (( consumed == BURST_N + PROBE_N - nadmit )) ||
    fail "the burst consumed $consumed instance ids for the $(( BURST_N + PROBE_N - nadmit )) launches admission let through ($nadmit refused outright, $n2xx started, $nslot refused at the slot): a launch refused by admission still created a row, so admit ran after openInstance"
else
  (( consumed <= BURST_N + PROBE_N - nadmit )) ||
    fail "the burst consumed $consumed instance ids although admission refused $nadmit of $(( BURST_N + PROBE_N )) launches outright: a launch refused by admission still created a row, so admit ran after openInstance"
fi

# Everything the burst left running goes away again, all at once because one
# at a time would cost more than the burst itself. The list comes from cmgrd
# rather than from the responses, so an instance whose answer was lost in
# transit is stopped too instead of being left on a worker for later steps.
extra=()
while read -r id; do
  if [[ -n "$id" ]] && ! grep -qx "$id" <<<"$before_ids"; then extra+=("$id"); fi
done <<<"$(od_instances)"
for id in "${extra[@]}"; do
  api_status DELETE "/instances/$id" >"$burst/stop-$id" &
done
wait
for id in "${extra[@]}"; do
  scode=$(cat "$burst/stop-$id")
  [[ "$scode" == 204 ]] || fail "stopping instance $id after the burst answered HTTP $scode"
done
note "${#extra[@]} instances left by the burst (the two probes among them) were stopped"
[[ "$(docker_objects "$WA")" == "$state_a" ]] || fail "the burst left containers or networks behind on $WA"
[[ "$(docker_objects "$WB")" == "$state_b" ]] || fail "the burst left containers or networks behind on $WB"
rows_after=$(api GET /workers | jq -r 'map(.instances) | add')
(( rows_after == rows_before )) ||
  fail "$(( rows_after - rows_before )) instance record(s) outlived the burst: a refused launch did not roll its row back (clearInstanceRecords, cmgr/api.go)"
rm -rf "$burst"
# down is sticky and only worker-add clears it, so a worker the burst knocked
# down would strand every step after this one. Say so here rather than let it
# surface as a confusing timeout later; overloaded clears itself, which the
# retry absorbs.
for ip in "$WA" "$WB"; do
  if health_is "$ip" down; then
    fail "worker $ip was marked down by a burst of $(( BURST_N + PROBE_N )) launches: a control call against it ran into CMGR_WORKER_CONTROL_TIMEOUT, or its telemetry poll was starved for CMGR_WORKER_MAX_MISSES"
  fi
done
retry 30 "both workers to be ok again after the burst" all_workers_ok
ok "$nadmit of $(( BURST_N + PROBE_N )) burst launches were refused by admission in under ${ADMIT_MAX_MS}ms against a $LAUNCH_WAIT_H wait, each naming the worker whose queue was full and carrying Retry-After; the burst consumed $consumed ids for the $(( BURST_N + PROBE_N - nadmit )) launches it let through, so a refusal cost no row, no port and no docker call"

# -------------------------------------------------------- 6. remote-make

step "remote-make challenge: an on-demand instance answers over TCP, then stops cleanly"
inst=$(api POST "/builds/$MK_BUILD")
id=$(jq -r .id <<<"$inst")
w=$(jq -r .worker <<<"$inst")
pub=$(jq -r .worker_public <<<"$inst")
port=$(jq -r .ports.socat <<<"$inst")
assert_on_worker "$w" "$inst"
retry 30 "the BinEx101 prompt at $pub:$port" tcp_says "$pub" "$port" '1\n1\n' 'Give me a number'

############################################################################
# BLOCK 3 -- one line inside the existing remote-make step (both modes),
#            after its tcp_says (line 1020)
############################################################################

# After this step exactly one worker has had to hold $CH_MAKE's image, and
# nothing rebuilds or relaunches this challenge afterwards (set_generation
# touches only $ONDEMAND_SRC and $PERSISTENT_SRC): the full-mode missing-tag
# step needs exactly that asymmetry, and it is free to record here.
MK_WORKER=$w
note "instance $id on $w: $pub:$port prompts for input"
cmgrd-cli stop "$id"
assert_gone "$id" "$w" "$inst"
ok "remote-make instance launched, answered, and was torn down on the worker"

########################################################################
# BLOCK 1 -- insert after e2e/scenario.sh:1024
#   ok "remote-make instance launched, answered, and was torn down on the worker"
########################################################################

# ------------------------------ 6b. container options (full mode only)

if (( FULL )); then
  step "container options: a challenge that asks for cpu, memory, pid and file limits gets them in its container's HostConfig on the worker"
  # Production runs the whole picoCTF library, and that library hardens its
  # challenges through the "## Challenge Options" block
  # (examples/specification.md:93-190, examples/multi/problem.md:41-52): the
  # cpu, memory and pid ceilings are what keep one instance from taking a
  # worker down with it. Nothing in e2e/schema.yaml declares any -- its four
  # challenges are chosen by delivery type, not by hardening -- and this step
  # may not add one, so it declares the block on the copy of binex101's
  # problem.md inside the CMGR_DIR volume (the technique set_generation
  # already uses on the sources) and takes it off again at the end.
  #
  # binex101 is the challenge to edit, for three reasons: it is on-demand, no
  # step holds a tracked instance of it, and it is not one of the two the
  # update steps rebuild -- so the rebuild this edit costs (see below) tears
  # nothing down and displaces no image generation.
  live=$(api GET "/schemas/$SCHEMA_NAME" | jq -r --arg c "$CH_MAKE" '[.[] | select(.id == $c) | .builds[].instances[]?] | length')
  [[ "$live" == 0 ]] ||
    fail "$CH_MAKE has $live live instance(s) at this point: the metadata edit below rebuilds the challenge, and a rebuild removes every on-demand instance of it (updateChallenges' DYNAMIC_INSTANCES arm, cmgr/database_challenges.go:723-729) behind whichever step launched them -- move this step, do not weaken it"

  # The negative control, and it is nearly free: an instance of a challenge
  # that declares no options at all. Every challenge carries a containerOptions
  # row for host "" -- the loader stores the zero value even when there is no
  # block (cmgr/loader.go:43-46) -- so hasContainerOpts is true here too and
  # the branch at cmgr/docker.go:1047-1105 does run; the limits are the only
  # evidence that distinguishes a declared option from a daemon default.
  # Deliberately no assertion on Init: that "" row makes cork send an explicit
  # false, which is an implementation detail of the empty row rather than a
  # promise about a challenge that asks for nothing.
  bare=${OD_IDS[0]:-}
  [[ -n "$bare" ]] ||
    fail "no tracked on-demand instance to read as the no-options control: this step must run while the on-demand launches are still standing"
  bare_cid=$(jq -r '.containers[0]' <<<"${INST_META[$bare]}")
  bare_hc=$(worker_api "${INST_WORKER[$bare]}" "/containers/$bare_cid/json" |
    jq -c '.HostConfig | {NanoCpus: (.NanoCpus // 0), Memory: (.Memory // 0), PidsLimit: (.PidsLimit // 0)}')
  for f in NanoCpus Memory PidsLimit; do
    [[ "$(jq -r --arg f "$f" '.[$f]' <<<"$bare_hc")" == 0 ]] ||
      fail "instance $bare of $CH_ONDEMAND declares no container options but already runs under $f on ${INST_WORKER[$bare]} ($bare_hc): the numbers asserted below would then prove nothing about the options block"
  done

  mk_before=$(api GET "/builds/$MK_BUILD")
  mk_tags_before=$(registry_tags "$CH_MAKE" | sort | tr '\n' ' ')
  opts_before=$(api GET "/challenges/$CH_MAKE" | jq -cS '.challenge_options // {}')

  # From here the EXIT trap puts the seed copy of problem.md back: a run that
  # died mid-step would otherwise leave the CMGR_DIR volume out of step with
  # the checkout, which the next run's tree check (scenario.sh:571-577) reports
  # as a stale volume rather than as this step's mess.
  EDITED_META=$MAKE_META
  # The fence markers are compared literally (lines[i] == "```yaml", == "```",
  # cmgr/loader_markdown.go:252-262), so they carry no indentation and no
  # trailing space; the ulimit is quoted so nothing about it depends on how
  # yaml resolves a plain scalar containing a colon.
  cat >>"$CHALLENGES/$MAKE_META" <<'OPTS'

## Challenge Options

```yaml
init: true
cpus: 0.5
memory: 256m
pidslimit: 50
ulimits:
  - "nofile=1024:1024"
nonewprivileges: true
```
OPTS
  grep -qx 'pidslimit: 50' "$CHALLENGES/$MAKE_META" ||
    fail "could not append the Challenge Options block to $MAKE_META"

  t=$(date +%s)
  rc=0
  out=$(cmgrd-cli update) || rc=$?
  sed 's/^/       /' <<<"$out"
  (( rc == 0 )) ||
    fail "cmgrd-cli update exited $rc after a Challenge Options block was added to $MAKE_META: a syntactically valid block must load (a section error fails the load loudly, cmgr/loader_markdown.go:161-186)"
  took=$(( $(date +%s) - t ))
  grep -q "  $CH_MAKE$" <<<"$out" ||
    fail "the update did not report $CH_MAKE after its problem.md changed: the options never reached the challenge row (DetectChanges compares MetadataChecksum, the crc32 of problem.md itself, cmgr/loader_markdown.go:191-198)"
  # And it touched nothing else. Said here rather than left to surface later:
  # the sources of the other two challenges are pristine at this point, and a
  # rebuild of the on-demand one would remove all four tracked instances
  # (cmgr/database_challenges.go:723-729) and strand every step after this.
  for other in "$CH_ONDEMAND" "$CH_PERSISTENT"; do
    if grep -q "  $other$" <<<"$out"; then
      fail "the update that only changed $MAKE_META also acted on $other, whose sources are untouched here: this step's edit is not confined to $CH_MAKE, and the instances the scenario is holding have been torn down"
    fi
  done
  # Which section it lands in is worth saying but not worth failing on: an
  # options change is not refreshable today -- safeToRefresh requires the
  # parsed and persisted ChallengeOptions to be DeepEqual (cmgr/database.go:
  # 710-722) -- so it takes the Updated arm and rebuilds. The durable promise,
  # asserted right below, is that the rebuild reproduces the same content and
  # so costs no image generation; whether cork later learns to refresh an
  # options edit is a decision this step must not pin.
  if grep -q "^Updated:" <<<"$out"; then
    note "the options edit rebuilt $CH_MAKE in ${took}s (safeToRefresh refuses to call an options change a refresh, cmgr/database.go:717); the build context is unchanged, so the rebuild is a cache hit that reproduces the same content checksum"
  else
    note "the options edit was applied to $CH_MAKE in ${took}s without a rebuild"
  fi

  # problem.md is excluded from the source checksum (checksumIgnore,
  # cmgr/filesystem.go:248-255), so the content identity -- and therefore every
  # tag, the flag and the rollback target -- must come through an options edit
  # untouched. A regression that folded problem.md into the source checksum
  # would push a fresh generation for a comment change and leak the displaced
  # tag past the teardown step's registry check.
  mk_after=$(api GET "/builds/$MK_BUILD")
  [[ "$(jq -r .checksum <<<"$mk_after")" == "$(jq -r .checksum <<<"$mk_before")" ]] ||
    fail "build $MK_BUILD's content checksum changed when only problem.md did ($(jq -r .checksum <<<"$mk_before") -> $(jq -r .checksum <<<"$mk_after")): problem.md is not part of the source checksum (cmgr/filesystem.go:248-255)"
  [[ "$(jq -r .flag <<<"$mk_after")" == "$(jq -r .flag <<<"$mk_before")" ]] ||
    fail "the flag of build $MK_BUILD changed over a metadata-only edit"
  [[ "$(jq -r .prev_checksum <<<"$mk_after")" == "$(jq -r .prev_checksum <<<"$mk_before")" ]] ||
    fail "build $MK_BUILD's rollback target rotated over a metadata-only edit: a rebuild that reproduces the same checksum must leave retention alone (rotatedPrevChecksum, cmgr/database_challenges.go:346-351)"
  mk_tags_now=$(registry_tags "$CH_MAKE" | sort | tr '\n' ' ')
  [[ "$mk_tags_now" == "$mk_tags_before" ]] ||
    fail "the registry tags of $CH_MAKE changed over a metadata-only edit (was '$mk_tags_before', now '$mk_tags_now'): the teardown step's leak check would inherit the difference"

  # The options really are on the challenge row now. Asserting this separately
  # is what makes a HostConfig failure below squarely the launch path
  # (cmgr/docker.go:1047-1105) rather than the loader or the database round
  # trip (cmgr/database_challenges.go:569-604, :120-136).
  opts_now=$(api GET "/challenges/$CH_MAKE" | jq -cS '.challenge_options // {}')
  [[ "$(jq -r '.cpus // ""' <<<"$opts_now")" == "0.5" && "$(jq -r '.pidslimit // 0' <<<"$opts_now")" == 50 ]] ||
    fail "$CH_MAKE's challenge row does not carry the declared options after the update: $opts_now"

  # try_launch, not launch: a container create the daemon refuses because of
  # the options themselves (no docker-init for 'init: true', a kernel without
  # the pids cgroup) must say so here instead of aborting the run on api's
  # --fail-with-body with the body swallowed.
  out=$(try_launch "$MK_BUILD" e2e-user-28 e2e-value-28)
  code=${out%%$'\n'*}
  body=${out#*$'\n'}
  case "$code" in
    200|201) ;;
    503) fail "a launch of $CH_MAKE with container options declared was refused as retryable (503): nothing is loading the fleet at this point, so this is a busy or unhealthy worker rather than anything about the options -- $(head -c 300 <<<"$body")" ;;
    *) fail "a launch of $CH_MAKE with cpu/memory/pid/ulimit options declared answered HTTP $code: $(head -c 300 <<<"$body") -- if the daemon refused the container create, it is one of the options its dockerd cannot honour (docker-init for 'init: true', or the pids cgroup), not the launch path" ;;
  esac
  track "$body" 28
  id=$(jq -r .id <<<"$body")
  w=${INST_WORKER[$id]}
  [[ "$(jq -r '.containers | length' <<<"$body")" == 1 ]] ||
    fail "instance $id of $CH_MAKE has $(jq -r '.containers | length' <<<"$body") container(s); this step inspects the one container a remote-make challenge launches"
  cid=$(jq -r '.containers[0]' <<<"$body")

  # One inspect, projected down to the fields the options branch writes: the
  # full HostConfig carries the whole seccomp policy inline in SecurityOpt and
  # is not something to print in a failure message. no-new-privileges is
  # matched on its prefix, not on the exact string: cork sends
  # "no-new-privileges:true" (docker.go:1088-1090) and the promise is the flag,
  # not dockerd's spelling of it in the stored HostConfig.
  hc=$(worker_api "$w" "/containers/$cid/json" |
    jq -c '.HostConfig | {NanoCpus, Memory, PidsLimit: (.PidsLimit // 0), Init,
                          nofile: [.Ulimits[]? | select(.Name == "nofile") | .Soft, .Hard],
                          nonewprivs: ([.SecurityOpt[]? | select(test("^no-new-privileges"))] | length)}')
  [[ "$(jq -r .NanoCpus <<<"$hc")" == 500000000 ]] ||
    fail "container $cid of instance $id on $w runs with NanoCpus $(jq -r .NanoCpus <<<"$hc"), expected 500000000 for 'cpus: 0.5' (parseNanoCPUs, cmgr/cpu.go:13-28, applied at cmgr/docker.go:1058-1064): the challenge's cpu ceiling never reached the worker ($hc)"
  [[ "$(jq -r .Memory <<<"$hc")" == 268435456 ]] ||
    fail "container $cid of instance $id on $w runs with Memory $(jq -r .Memory <<<"$hc"), expected 268435456 for 'memory: 256m' (units.RAMInBytes at cmgr/docker.go:1065-1071) ($hc)"
  [[ "$(jq -r .PidsLimit <<<"$hc")" == 50 ]] ||
    fail "container $cid of instance $id on $w runs with PidsLimit $(jq -r .PidsLimit <<<"$hc"), expected 50 (cmgr/docker.go:1083-1085): a forkbomb in one instance would take the whole worker with it ($hc)"
  [[ "$(jq -r .Init <<<"$hc")" == true ]] ||
    fail "container $cid of instance $id on $w has Init $(jq -r .Init <<<"$hc"), expected true (cmgr/docker.go:1057) ($hc)"
  [[ "$(jq -c .nofile <<<"$hc")" == '[1024,1024]' ]] ||
    fail "container $cid of instance $id on $w carries nofile ulimits $(jq -c .nofile <<<"$hc"), expected [1024,1024] for 'nofile=1024:1024' (units.ParseUlimit at cmgr/docker.go:1072-1082) ($hc)"
  [[ "$(jq -r .nonewprivs <<<"$hc")" == 1 ]] ||
    fail "container $cid of instance $id on $w does not carry no-new-privileges although the challenge asks for it (cmgr/docker.go:1088-1090) ($hc)"
  note "instance $id on $w: NanoCpus 500000000, Memory 268435456, PidsLimit 50, Init true, nofile 1024:1024, no-new-privileges"

  # And it still works under them: a ceiling that strangles the service is a
  # different bug from one that never arrives, and only the wire tells them
  # apart. Same prompt the remote-make step reads.
  pub=$(jq -r .worker_public <<<"$body")
  port=$(jq -r .ports.socat <<<"$body")
  retry 30 "the BinEx101 prompt at $pub:$port under its declared limits" tcp_says "$pub" "$port" '1\n1\n' 'Give me a number'
  cmgrd-cli stop "$id"
  assert_gone "$id" "$w" "$body"
  forget "$id"

  # Undo it for real: the challenge tree is restored AND the row is put back,
  # because the cork database lives in the cork-data volume and outlives this
  # run (run.sh never takes the fleet down with -v). A row left carrying these
  # limits would harden this challenge for every later step and every later
  # run, silently.
  cp "$CHALLENGES_SEED/$MAKE_META" "$CHALLENGES/$MAKE_META"
  diff -q "$CHALLENGES_SEED/$MAKE_META" "$CHALLENGES/$MAKE_META" >/dev/null ||
    fail "$MAKE_META in $CHALLENGES still differs from the seed after the restore; the next run's tree check would report the volume as stale"
  EDITED_META=""
  rc=0
  out=$(cmgrd-cli update) || rc=$?
  sed 's/^/       /' <<<"$out"
  (( rc == 0 )) || fail "the update that restores $MAKE_META exited $rc; $CH_MAKE is left carrying container options nothing declares"
  opts_restored=$(api GET "/challenges/$CH_MAKE" | jq -cS '.challenge_options // {}')
  [[ "$opts_restored" == "$opts_before" ]] ||
    fail "$CH_MAKE still carries container options after $MAKE_META was restored (was '$opts_before', now '$opts_restored'): every later run would launch it under limits its problem.md does not declare"
  [[ "$(api GET "/builds/$MK_BUILD" | jq -r .checksum)" == "$(jq -r .checksum <<<"$mk_before")" ]] ||
    fail "build $MK_BUILD's checksum did not come back to what it was before this step"
  [[ "$(registry_tags "$CH_MAKE" | sort | tr '\n' ' ')" == "$mk_tags_before" ]] ||
    fail "the registry tags of $CH_MAKE did not come back to what they were before this step"
  ok "a declared cpu/memory/pid/ulimit block reached instance $id's HostConfig on $w and the challenge still served under it; an instance of a challenge that declares nothing carries no limits at all; the edit cost no image generation and was taken back off the challenge row"
else
  deselect "container options: cpu, memory, pid and ulimit limits reach the worker's HostConfig"
fi

# ------------------------------------------------------------ 7. stop path

step "stop path: DELETE /instances/<id> tears the instance down on its worker and is idempotent"
id=${OD_IDS[0]}
w=${INST_WORKER[$id]}
cmgrd-cli stop "$id"
assert_gone "$id" "$w" "${INST_META[$id]}"
[[ "$(api_status DELETE "/instances/$id")" == 204 ]] || fail "a second DELETE of instance $id was not a 204"
forget "$id"
ok "instance $id: containers and network gone from $w, second delete is a no-op"

########################################################################
# BLOCK 2 -- insert after e2e/scenario.sh:1035
#   ok "instance $id: containers and network gone from $w, second delete is a no-op"
########################################################################

# --------------------------- 7b. reaped from under us (full mode only)

if (( FULL )); then
  step "reaped from under us: an instance whose containers and network docker-reaper has already removed still stops cleanly"
  # This is the ordinary production case, not an edge one. Every worker runs
  #   docker-reaper containers --filter label=cmgr.dynamic=true --min-age 60m
  #                            --reap-networks
  # every 60s (roles/multihost_docker/defaults/main.yml:41-46), so any
  # on-demand instance a player leaves standing for an hour has its containers
  # AND its cmgr-<id> network taken out from under cork, which never notices.
  # The platform's stop then arrives against nothing. reap() is that sweep,
  # driven through the worker's own dockerd with cork's client certificate --
  # no outer socket, exactly as the reaper needs no cork.
  inst=$(launch "$OD_BUILD" "e2e-user-26" "e2e-value-26")
  track "$inst" 26
  id=$(jq -r .id <<<"$inst")
  w=${INST_WORKER[$id]}
  # A real instance first: containers on the worker, a port out of CMGR_PORTS,
  # answering. Reaping a launch that never came up would prove nothing.
  check_ondemand "$inst" 26
  rows_with=$(worker_instances "$w")
  [[ "$rows_with" =~ ^[0-9]+$ ]] || fail "could not read the instance count of worker $w"

  reap "$w" "$id" "$inst"
  # The negative control, and the reason the 204 below means anything: the
  # sweep really did complete, so the stop has nothing left to remove. Without
  # it a 204 is just what an ordinary stop returns.
  orphan_gone "$w" "$id" "$inst" ||
    fail "the simulated docker-reaper sweep left containers or the cmgr-$id network on $w: the stop below would not be the post-reap case this step is about"
  # And cork has not noticed: nothing sweeps a finalized row, so the record
  # (and its port reservation) survives until something stops it. If this ever
  # stops being true, the assertions below would pass for the wrong reason.
  [[ "$(api_status GET "/instances/$id")" == 200 ]] ||
    fail "cork dropped instance $id's record when the reaper removed its containers: only a stop, a rebuild or a worker purge may clear a record, and the platform is still holding this one"

  t=$(date +%s)
  code=$(api_status DELETE "/instances/$id")
  took=$(( $(date +%s) - t ))
  [[ "$code" == 204 ]] ||
    fail "stopping instance $id after a docker-reaper sweep answered HTTP $code, not 204: a container that is already gone must be skipped (errdefs.IsNotFound in stopContainers, cmgr/docker.go:1259-1262) and a missing cmgr-$id network counted as removed (stopNetwork, cmgr/docker.go:915-919). Without that tolerance stopInstance never reaches removeInstanceMetadata (cmgr/api.go:427-440), so every stop of a reaped instance is a 500 that strands the row, its ports and its worker's instance count"
  # Bounded, because a NotFound-intolerant path that also retried would show
  # up as time rather than as a status. Both removals are answered by the
  # daemon at once, so the whole call is a couple of database writes; 15s is
  # well past that and still inside one CMGR_WORKER_CONTROL_TIMEOUT (10s in
  # this fleet), so only a real hang can fire it.
  (( took < 15 )) ||
    fail "the stop of the already-reaped instance $id took ${took}s: nothing it does can block (every removal is answered 404 at once), so it spent a control timeout waiting on the daemon"
  [[ "$(api_status GET "/instances/$id")" == 404 ]] ||
    fail "instance $id is still known to cmgrd after a 204 stop: the record outlived the teardown that reported success"
  # Nothing was re-created on the way out, and the network was not recreated
  # by a stop that took "not found" for "make it so".
  assert_gone_from_worker "$w" "$id" "$inst"
  # down is sticky and costs an operator a worker-add, so this is the
  # assertion that would cost a production box: a NotFound offered to
  # noteWorkerTransportError (cmgr/docker.go:1264, isTransportError at
  # cmgr/workers.go:521-539) would walk a fleet down every time the reaper ran
  # ahead of a stop. Read into a variable rather than negating health_is, so a
  # /workers read that fails is a loud failure instead of a silent pass; and
  # written as "not down" rather than "ok", because overloaded is a legitimate
  # transient reading here while down is never one.
  h=$(worker_health "$w")
  [[ "$h" != down ]] ||
    fail "worker $w was marked down by the stop of an instance the reaper had already cleared: a container that is not there is not a transport failure (cmgr/docker.go:1259-1262, cmgr/workers.go:521-539)"
  rows_after=$(worker_instances "$w")
  [[ "$rows_after" == "$(( rows_with - 1 ))" ]] ||
    fail "worker $w records $rows_after instance(s) after the reaped instance was stopped, expected $(( rows_with - 1 )): the row survived its own stop, and its port reservation with it (the port rows cascade with the instance, removeInstanceMetadata at cmgr/api.go:440)"
  forget "$id"

  # The fleet still hands out ports afterwards. A smoke test rather than a
  # control -- placement is round robin and 30 ports per worker leave room for
  # one stranded reservation to hide -- but it is the cheapest thing that
  # would catch a stop that cleared the record while leaving cork's port index
  # (179b875) holding the reaped instance's port. Deliberately no
  # check_ondemand: the stale-network step that follows launches and serves an
  # instance in both modes, so the "the fleet still works" half is already
  # paid for, and this launch only has to be admitted and given a port.
  fresh=$(launch "$OD_BUILD" "e2e-user-27" "e2e-value-27")
  track "$fresh" 27
  fid=$(jq -r .id <<<"$fresh")
  fport=$(jq -r .ports.server <<<"$fresh")
  (( fport >= PORT_LOW && fport <= PORT_HIGH )) ||
    fail "the launch after the reaped stop was given port $fport, outside CMGR_PORTS $PORT_LOW-$PORT_HIGH"
  cmgrd-cli stop "$fid"
  assert_gone "$fid" "${INST_WORKER[$fid]}" "$fresh"
  forget "$fid"
  ok "instance $id was reaped off $w (containers and cmgr-$id network) behind cork's back, its stop still answered 204 in ${took}s, its record and its worker's instance count went with it, $w was never marked down, and the next launch still drew a port from CMGR_PORTS"
else
  deselect "reaped from under us: a stop after docker-reaper removed the containers and the network"
fi

# ------------------------------------------- 7b. stale network self-heal

step "stale network: a launch whose cmgr-<id> network already exists on the worker still starts (ids are never reused)"
inst=$(launch "$OD_BUILD" "e2e-user-21" "e2e-value-21")
id=$(jq -r .id <<<"$inst")
[[ "$(api_status DELETE "/instances/$id")" == 204 ]] || fail "could not stop instance $id"
# Ids are never reused (AUTOINCREMENT), so the next launch takes $id + 1,
# and a stop must never bring an id back. Either worker may receive the
# launch, so leave a stale network of that name on both.
next=$((id + 1))
declare -A PLANTED_NET=()
for ip in "${WORKER_IPS[@]}"; do
  code=$(worker_status "$ip" /networks/create -X POST -H 'Content-Type: application/json' -d "{\"Name\":\"cmgr-$next\",\"Driver\":\"bridge\"}")
  [[ "$code" == 201 ]] || fail "could not plant network cmgr-$next on $ip: HTTP $code"
  # The planted network is byte for byte what cmgrd would create, so only its
  # id distinguishes "removed and recreated" from "adopted the leftover".
  PLANTED_NET[$ip]=$(worker_api "$ip" "/networks/cmgr-$next" | jq -r '.Id // empty')
  [[ -n "${PLANTED_NET[$ip]}" ]] || fail "could not read the id of the planted network cmgr-$next on $ip"
done
inst=$(launch "$OD_BUILD" "e2e-user-22" "e2e-value-22")
[[ "$(jq -r .id <<<"$inst")" == "$next" ]] || fail "expected the launch to take instance id $next (never $id again), got $(jq -r .id <<<"$inst")"
id=$next
track "$inst" 22
check_ondemand "$inst" 22
w=${INST_WORKER[$id]}
live_net=$(worker_api "$w" "/networks/cmgr-$id" | jq -r '.Id // empty')
[[ -n "$live_net" ]] || fail "no cmgr-$id network on $w after the launch"
[[ "$live_net" != "${PLANTED_NET[$w]}" ]] ||
  fail "the launch adopted the stale network cmgr-$id on $w instead of removing it and creating its own"
for ip in "${WORKER_IPS[@]}"; do
  if [[ "$ip" != "$w" ]]; then
    code=$(worker_status "$ip" "/networks/cmgr-$id" -X DELETE)
    [[ "$code" == 204 ]] || fail "could not remove the unused planted network on $ip: HTTP $code"
  fi
done
cmgrd-cli stop "$id"
assert_gone "$id" "$w" "$inst"
forget "$id"
ok "instance $id started over a stale cmgr-$id network on $w: cmgrd replaced the network and the instance served; the stopped id was not reused"

# BLOCK 1 — goes after the existing stale-network step (scenario.sh:1075),
# before the "8. cmgrd restart" banner.
# =========================================================================

# ------------------- 7c. a stale network that will not go (full mode only)

if (( FULL )); then
step "stale network with a live endpoint: the launch fails with both errors, and a name conflict never costs its worker its place in the fleet"
# The step above plants an EMPTY cmgr-<id> network, which dockerd removes on
# request, so startNetwork's recovery arm always reaches its second
# NetworkCreate and the launch succeeds. The other half of that arm has no
# cover at all: a network that still has an endpoint -- what a DB-only stop
# leaves on a box that has not rejoined placement, and what plant_orphan makes
# -- refuses removal, and then two things must hold at once. Both errors have
# to reach the caller ("%w (stale network not removed: %w)", cmgr/docker.go
# startNetwork), because the removal failure alone names nothing the launch
# collided with; and the refusal must not be read as the daemon being
# unreachable, or one leftover network costs a production worker until an
# operator re-adds it.
# Every record this step opens is closed again, so the row count both workers
# report is the ledger it is checked against at the end.
stale_rows_before=$(api GET /workers | jq -r 'map(.instances) | add')
[[ "$stale_rows_before" =~ ^[0-9]+$ ]] || fail "could not read the recorded instance count before the stale-network step"
probe=$(launch "$OD_BUILD" "e2e-user-26" "e2e-value-26")
probe_id=$(jq -r .id <<<"$probe")
[[ "$(api_status DELETE "/instances/$probe_id")" == 204 ]] ||
  fail "could not stop the probe instance $probe_id this step launches only to learn the next instance id"
# Ids are AUTOINCREMENT (452d96f) and nothing else launches between here and
# the launch below, so the next one takes $stale. Which worker round robin
# hands it to is not ours to choose, so both boxes get an orphan of that id.
stale=$(( probe_id + 1 ))
declare -A STALE_ORPHAN=()
for ip in "${WORKER_IPS[@]}"; do
  STALE_ORPHAN[$ip]=$(plant_orphan "$ip" "$stale")
done
note "planted cmgr-$stale on both workers, each held open by a running cmgr.managed container"

out=$(try_launch "$OD_BUILD" "e2e-user-27" "e2e-value-27")
code=${out%%$'\n'*}
body=${out#*$'\n'}
# (a) 500, not 503. A leftover network is not something a retry fixes, and the
# retryable classes are a closed set (ErrWorkerBusy, ErrWorkerDown,
# ErrPullTimeout, ErrAllWorkersOverloaded, ErrDatabaseBusy at
# cmd/cmgrd/main.go): dressing this up as one would have the platform's celery
# worker re-place the same launch onto the same undeletable network forever.
[[ "$code" == 500 ]] ||
  fail "the launch onto cmgr-$stale, a network whose removal is refused, answered HTTP $code, expected 500: a name conflict is not one of cmgrd's retryable failures (cmd/cmgrd/main.go) ($(head -c 200 <<<"$body"))"
# (b) The chain, both halves of it. The conflict says what the launch ran into,
# the removal failure says why the recovery could not clear it; a wrapper that
# replaced the first with the second would leave an operator reading "network
# not removed" with nothing naming the collision. "already exists" is dockerd's
# own wording for a duplicate network name; "stale network not removed" is
# cork's, so only one of the two can drift with a docker upgrade.
grep -q "already exists" <<<"$body" ||
  fail "the refused launch does not report the original conflict: startNetwork must keep the 'already exists' error in the chain rather than replacing it with the removal failure (cmgr/docker.go) ($(head -c 200 <<<"$body"))"
grep -q "stale network not removed" <<<"$body" ||
  fail "the refused launch reports the conflict but never says the stale network could not be removed: the removal failure was swallowed, so the caller cannot tell a self-healed launch from one that will fail again (cmgr/docker.go) ($(head -c 200 <<<"$body"))"

# The negative control. Without it every assertion here would pass just as well
# if dockerd had quietly removed the network and the launch had failed for some
# other reason: the endpoint must still be live, on both boxes, now that the
# launch is over.
for ip in "${WORKER_IPS[@]}"; do
  [[ "$(worker_api "$ip" "/containers/${STALE_ORPHAN[$ip]}/json" | jq -r .State.Running)" == true ]] ||
    fail "the planted container on $ip is no longer running, so cmgr-$stale did not refuse removal for the reason this step is about"
  [[ "$(worker_status "$ip" "/networks/cmgr-$stale")" == 200 ]] ||
    fail "cmgr-$stale is gone from $ip although a container still holds it: this step no longer exercises a refused removal"
done
# (c) The expensive one. A 403 from a daemon that answered is not a transport
# failure (isTransportError, cmgr/workers.go), so noteWorkerTransportError must
# leave both boxes in the fleet. down is sticky and only worker-add clears it,
# so this single read is the whole assertion and can never be a passing sample;
# overloaded can be a telemetry blip on a busy box, which is why the ok check
# below is a separate, retried one.
for ip in "${WORKER_IPS[@]}"; do
  if health_is "$ip" down; then
    fail "worker $ip was marked down by a launch that failed on a name conflict: a docker API error arrived over a working connection and says nothing about the daemon's health (isTransportError, cmgr/workers.go)"
  fi
done
retry 20 "both workers to report ok after the refused launch" all_workers_ok

# Housekeeping. Not reap(): its single-shot network DELETE goes through
# worker_api (--fail-with-body), and dockerd can hold an endpoint for a moment
# after its container is gone -- the same race the blocker network is retried
# against at the incomplete-reconcile step -- so a 403 there would abort the
# run instead of failing an assertion.
stale_net_gone() { # stale_net_gone <worker ip>
  local c
  c=$(worker_status "$1" "/networks/cmgr-$stale" -X DELETE)
  [[ "$c" == 204 || "$c" == 404 ]]
}
for ip in "${WORKER_IPS[@]}"; do
  cid=${STALE_ORPHAN[$ip]}
  # 409: a removal already in progress, exactly as reap() tolerates it.
  dcode=$(worker_status "$ip" "/containers/$cid?force=true" -X DELETE)
  case "$dcode" in
    204|404|409) ;;
    *) fail "could not remove the planted container $cid from $ip: HTTP $dcode" ;;
  esac
  retry 15 "the planted container to leave $ip" container_gone "$ip" "$cid"
  retry 15 "cmgr-$stale to leave $ip once its container is gone" stale_net_gone "$ip"
  orphan_gone "$ip" "$stale" "{\"containers\":[\"$cid\"]}" ||
    fail "the planted orphan of cmgr-$stale is still on $ip after this step reaped it"
done

# The row the refused launch opened outlives it on purpose, and is deliberately
# NOT asserted against: launchStages reports a startNetwork failure with
# started=true, so newInstance calls stopInstance, whose teardown fails on the
# very same undeletable network before it can remove the record (cmgr/api.go).
# Prune's unfinalized sweep collects it five minutes later. With the network
# now reaped that stop goes through, so clearing it here hands the run back a
# database in the state it found it in, instead of a row holding one of the 30
# ports for the rest of the run. The status is checked only so a failure to
# tidy up is loud.
dcode=$(api_status DELETE "/instances/$stale")
[[ "$dcode" == 204 || "$dcode" == 404 ]] ||
  fail "the record the refused launch left behind (instance $stale) could not be cleared once its network was reaped: HTTP $dcode"
inst=$(launch "$OD_BUILD" "e2e-user-28" "e2e-value-28")
id=$(jq -r .id <<<"$inst")
if [[ "$dcode" == 204 ]]; then
  # The 204 proves the refused launch really had opened row $stale, so this is
  # the id-reuse invariant asked on the one path that burns an id and then
  # gives it back: never reuse (452d96f). A 404 means Prune got there first and
  # the question cannot be asked.
  (( id > stale )) ||
    fail "the launch after the refused one took instance id $id, at or below the $stale the refused launch burned and gave back: ids must never be reused (452d96f)"
fi
track "$inst" 28
check_ondemand "$inst" 28
cmgrd-cli stop "$id"
assert_gone "$id" "${INST_WORKER[$id]}" "$inst"
forget "$id"
stale_rows_after=$(api GET /workers | jq -r 'map(.instances) | add')
(( stale_rows_after == stale_rows_before )) ||
  fail "$(( stale_rows_after - stale_rows_before )) instance record(s) outlived this step ($stale_rows_before -> $stale_rows_after): the row the refused launch opened, or its reserved port, is still held"
ok "a launch onto a cmgr-$stale that refuses removal was a 500 naming both the conflict and the failed removal, neither worker left the fleet, and with the orphans reaped the next launch (instance $id) served normally"
else
  deselect "stale network with a live endpoint on it"
fi


# =========================================================================

# ------------------------------------------------- 8. cmgrd restart

step "cmgrd restart: workers and instances come back from the database"
if (( OUTER )); then
  cork=$(compose_container cork)
  [[ -n "$cork" ]] || fail "cork container not found via the outer docker API"
  planted=$(plant_orphan "$WA" 999)
  note "planted an orphan (cmgr.managed container on network cmgr-999, no record) on $WA"
  outer_ctl "$cork" restart
  retry 60 "cmgrd after restart" quiet api GET /version
  # Ordering, not eventual consistency: runWorker reconciles and only then
  # starts the poller, so the first ok verdict must already be past the
  # removals. Waiting for ok and then giving the orphan another 30s would
  # pass just as well with the poller started first, which is the regression
  # that puts launches on a box whose leftovers still hold its host ports.
  retry 60 "worker $WA to report ok after the restart" health_is "$WA" ok
  orphan_gone "$WA" 999 "{\"containers\":[\"$planted\"]}" ||
    fail "worker $WA reported ok while the planted orphan was still on it: the reconcile did not finish before the poller started"
  retry 30 "every worker to report ok again" all_workers_ok
  pafter=$(api GET "/instances/$PERSIST_INST")
  [[ -n "$pafter" && "$pafter" != null ]] || fail "persistent instance $PERSIST_INST forgotten across the restart"
  # The record surviving says nothing about the containers: a startup
  # reconcile that mistook a persistent instance's containers for orphans
  # would leave the row and its ports untouched.
  [[ "$(jq -c '.containers | sort' <<<"$pafter")" == "$(jq -c '.containers | sort' <<<"$PERSIST_META")" ]] ||
    fail "persistent instance $PERSIST_INST changed containers across the restart"
  assert_on_worker "$PERSIST_WORKER" "$pafter"
  id=${OD_IDS[0]}
  meta=$(api GET "/instances/$id")
  [[ "$(jq -r .worker <<<"$meta")" == "${INST_WORKER[$id]}" ]] || fail "instance $id lost its worker across the restart"
  check_ondemand "$meta" "${INST_USER[$id]}"
  inst=$(launch "$OD_BUILD" "e2e-user-5" "e2e-value-5")
  track "$inst" 5
  check_ondemand "$inst" 5
  ok "after a restart the workers are re-polled to ok, the orphan on $WA was removed at startup, instance $id is still served, new launches work"
else
  skip "needs the outer docker socket"
fi

# ---------------------------------------------- 9. rebuild (update)

step "update, generation 2: two updates at once (the second waits for the first and finds nothing left to rebuild), edited sources rebuild both changed challenges, push new tags, restart the persistent instance, tear down on-demand ones"
OD_BUILD_G1=$(api GET "/builds/$OD_BUILD")
OD_TAG_G1=${IMAGE_TAG[$CH_ONDEMAND]}
PERSIST_TAG_G1=${IMAGE_TAG[$CH_PERSISTENT]}
PERSIST_META_G1=$(api GET "/instances/$PERSIST_INST")
set_generation 2 both
# Both updates go out together. UpdateWithOptions takes updateMu as its very
# first statement, before DetectChanges (cmgr/api.go:183-190, cmgr/types.go:57-59),
# and updateHandler touches no state before calling it (cmd/cmgrd/main.go:670-700),
# so the loser blocks for the whole rebuild and only looks at the tree
# afterwards -- by which time updateChallenges has persisted the new checksums
# (cmgr/database_challenges.go:364-369) and nothing is left to do. Nothing else
# on this path serializes: unserialized, both calls would detect the edit, both
# would build, and both would restart this one persistent instance -- each
# tearing down what the other had just started and handing out the same port
# twice.
upd_tmp=$(mktemp -d)
t=$(date +%s)
for n in 1 2; do
  # Each call records its own exit status and wall time in its own files, so
  # neither reading depends on which of the two finished first.
  ( urc=0; s=$(date +%s)
    cmgrd-cli update --prune-old >"$upd_tmp/out.$n" 2>"$upd_tmp/err.$n" || urc=$?
    printf '%s %s\n' "$urc" "$(( $(date +%s) - s ))" >"$upd_tmp/rc.$n" ) &
done
wait # a bare wait is always 0; each call's own recorded status is what bites
declare -a UPD_TOOK=()
upd_worked=() upd_idle=()
for n in 1 2; do
  [[ -s "$upd_tmp/rc.$n" ]] || fail "concurrent update $n never finished: no exit status was recorded"
  read -r urc usecs <"$upd_tmp/rc.$n"
  # An update is an operator action, so the second one waits: neither call may
  # be refused or error out. Report whichever failed with its own output rather
  # than blaming the mutex -- the rebuilding call can fail here too, and
  # cmgrd-cli prints its "Errors:" section on stdout, its transport errors on
  # stderr (cmd/cmgrd-cli/commands.go:105-111).
  if (( urc != 0 )); then
    sed 's/^/       /' "$upd_tmp/out.$n" "$upd_tmp/err.$n" || true
    fail "concurrent update $n exited $urc: both updates must succeed, the second by blocking on updateMu (cmgr/api.go:189)"
  fi
  UPD_TOOK[$n]=$usecs
  if [[ -s "$upd_tmp/err.$n" ]]; then note "update $n wrote to stderr:"; sed 's/^/       /' "$upd_tmp/err.$n"; fi
  if [[ -s "$upd_tmp/out.$n" ]]; then upd_worked+=("$n"); else upd_idle+=("$n"); fi
done
# The sharp one: an unserialized pair both reach DetectChanges before either
# has rebuilt, so both report "Updated:" for the same two challenges. An update
# with no work prints nothing at all -- printSection skips empty sections and
# Unmodified is only printed under --verbose (cmd/cmgrd-cli/commands.go:89-104)
# -- so a non-empty stdout is exactly "this call did work".
if (( ${#upd_worked[@]} != 1 )); then
  for n in 1 2; do printf '       update %s stdout:\n' "$n"; sed 's/^/         /' "$upd_tmp/out.$n"; done
  fail "${#upd_worked[@]} of 2 concurrent updates reported work: exactly one must rebuild and the other must find nothing changed (updateMu in UpdateWithOptions, cmgr/api.go:189)"
fi
win=${upd_worked[0]}
lose=${upd_idle[0]}
out=$(cat "$upd_tmp/out.$win")
# The negative control for the assertion above: had the second call started
# only once the first was over, it would find nothing changed for a reason that
# has nothing to do with the mutex, and this step would pass vacuously.
# Blocking for (nearly) the whole rebuild is what proves it waited. Only the
# early side is bounded -- the loser still runs its own DetectChanges after the
# handover -- and the 5s of slack absorbs the whole-second stamps on both sides
# (this image's date has no %N) plus fork/exec skew between the two subshells.
(( ${UPD_TOOK[$win]} >= 15 )) ||
  note "the rebuild took only ${UPD_TOOK[$win]}s, so the blocking check below is weak rather than wrong"
(( ${UPD_TOOK[$lose]} >= ${UPD_TOOK[$win]} - 5 )) ||
  fail "the second update returned after ${UPD_TOOK[$lose]}s while the rebuild took ${UPD_TOOK[$win]}s: it did not block on updateMu (cmgr/api.go:189), so its empty output proves nothing"
note "update $win rebuilt in ${UPD_TOOK[$win]}s; update $lose blocked ${UPD_TOOK[$lose]}s and printed nothing"
rm -rf "$upd_tmp"
sed 's/^/       /' <<<"$out"
note "updated in $(( $(date +%s) - t ))s"
ok "two updates at once serialized: one rebuilt both challenges, the other blocked ${UPD_TOOK[$lose]}s on updateMu and found nothing left to rebuild"
grep -q "^Updated:" <<<"$out" || fail "update did not report updated challenges"
grep -q "  $CH_ONDEMAND$" <<<"$out" || fail "$CH_ONDEMAND was not rebuilt"
grep -q "  $CH_PERSISTENT$" <<<"$out" || fail "$CH_PERSISTENT was not rebuilt"
OD_BUILD_G2=$(api GET "/builds/$OD_BUILD")
[[ "$(jq -r .checksum <<<"$OD_BUILD_G2")" != "$(jq -r .checksum <<<"$OD_BUILD_G1")" ]] || fail "build $OD_BUILD checksum did not change"
[[ "$(jq -r .prev_checksum <<<"$OD_BUILD_G2")" == "$(jq -r .checksum <<<"$OD_BUILD_G1")" ]] || fail "build $OD_BUILD does not retain generation 1 as its rollback target"
[[ "$(jq -r .flag <<<"$OD_BUILD_G2")" == "$OD_FLAG" ]] || fail "the flag changed on rebuild"
OD_TAG_G2=$(image_tag "$OD_BUILD_G2" challenge)
tags=$(registry_tags "$CH_ONDEMAND")
grep -qx "$OD_TAG_G2" <<<"$tags" || fail "generation 2 tag $OD_TAG_G2 was not pushed"
grep -qx "$OD_TAG_G1" <<<"$tags" || fail "generation 1 tag $OD_TAG_G1 left the registry although it is the rollback target"
note "registry now holds $OD_TAG_G1 (rollback) and $OD_TAG_G2 (current)"
for id in "${OD_IDS[@]}"; do
  assert_torn_down "$id"
done
note "${#OD_IDS[@]} on-demand instances were torn down on their workers and removed from cmgrd, not restarted"
OD_IDS=()
PERSIST_META_G2=$(api GET "/instances/$PERSIST_INST")
[[ "$(jq -c '.containers | sort' <<<"$PERSIST_META_G2")" != "$(jq -c '.containers | sort' <<<"$PERSIST_META_G1")" ]] || fail "persistent instance $PERSIST_INST kept its old containers"
w=$(jq -r .worker <<<"$PERSIST_META_G2")
pub=$(jq -r .worker_public <<<"$PERSIST_META_G2")
port=$(jq -r '.ports.socat // 0' <<<"$PERSIST_META_G2")
[[ "$w" == "$(jq -r .worker <<<"$PERSIST_META_G1")" ]] || fail "persistent instance $PERSIST_INST moved workers on rebuild"
[[ "$port" == "$(jq -r .ports.socat <<<"$PERSIST_META_G1")" ]] || fail "persistent instance $PERSIST_INST changed port on rebuild: $(jq -r .ports.socat <<<"$PERSIST_META_G1") -> $port"
assert_on_worker "$w" "$PERSIST_META_G2"
# The recorded container ids differ as soon as new ones exist; what matters is
# that restartInstance tore the old ones down on the box rather than only
# forgetting them. Nothing else would notice: reconcileWorker attributes a
# container to the instance id in its network name, so a leftover of the same
# instance is never an orphan.
for cid in $(jq -r '.containers[]' <<<"$PERSIST_META_G1"); do
  container_gone "$w" "$cid" ||
    fail "generation 1 container $cid of the persistent instance is still on $w after the rebuild"
done
retry 30 "generation 2 of the persistent instance at $pub:$port" tcp_says "$pub" "$port" '' 'e2e-generation-2'
inst=$(launch "$OD_BUILD" "e2e-user-6" "e2e-value-6")
track "$inst" 6
check_ondemand "$inst" 6 "E2E_GENERATION=2"
ok "persistent instance $PERSIST_INST restarted in place on $w:$port (same address) with new containers serving generation 2; a fresh launch serves generation 2"

# BLOCK 2 -- insert after e2e/scenario.sh:1228 (the `ok` that closes the
# "update, generation 2" step), before the generation-3 step. Uses ART_TMP,
# ART_G1 and PERSIST_FLAG from block 1 and defines ART_G2 and
# PERSIST_CHECKSUM_G2 for block 3; all three blocks share one FULL gate, so
# the chain is never half-defined.
# ============================================================================

# ------------------------------ 9b. the rebuilt build's artifacts

if (( FULL )); then
  step "artifacts after the rebuild: the bundle served for the rebuilt build is generation 2, promoted in place"
  # cacheArtifacts stages the new archive as .<id>.tar.gz.staged and only
  # renames it onto <id>.tar.gz once validateBuild has passed (cmgr/docker.go:
  # 777-789 and 813-824). The rename itself is invisible from here -- this
  # container cannot see cork's artifact directory -- but its consequence is
  # not: the same URL, for the same build id, must now serve the new
  # generation's files. A rebuild that promoted nothing serves generation 1
  # forever, and until this step nothing in the run ever looked.
  ART_G2="$ART_TMP/persist-g2.tar.gz"
  out=$(cmgrd-cli artifacts "$PERSIST_BUILD" "$ART_G2" 2>&1) ||
    fail "cmgrd-cli artifacts $PERSIST_BUILD did not succeed after the rebuild: $out"
  members=$(art_members "$ART_G2") ||
    fail "the bundle served for build $PERSIST_BUILD after the rebuild is not a readable gzip tar"
  [[ "$members" == "secret.enc time.txt" ]] ||
    fail "the rebuilt bundle for build $PERSIST_BUILD holds '$members', expected 'secret.enc time.txt'"
  gen=$(tar -xzOf "$ART_G2" time.txt) ||
    fail "could not read time.txt out of the rebuilt bundle for build $PERSIST_BUILD"
  # The load-bearing one, and the reason set_generation stamps the marker into
  # time.txt as well as into the advertised command: time.txt is inside the
  # archive, so this is the only assertion in the scenario that can tell the
  # promoted archive from the one it replaced. cmgr builds with the cache on
  # (executeBuild sets no NoCache) and the generation-2 edit alone touches only
  # the CMD, after the tar, so without that stamp the two generations' bundles
  # are byte-identical and there would be nothing here to assert.
  [[ "$gen" == "e2e-generation-2" ]] ||
    fail "the bundle served for build $PERSIST_BUILD carries '$gen' in time.txt after the generation-2 rebuild: the staged archive was never promoted onto <id>.tar.gz (the os.Rename at cmgr/docker.go:813-824), so players still download generation 1"
  if same_bytes "$ART_G1" "$ART_G2"; then
    fail "the bundle served for build $PERSIST_BUILD is byte for byte the one it served before the rebuild: nothing was promoted"
  fi

  # Promoted in place, and still coherent: same build id, same URL, still
  # flagged as publishing, and the new ciphertext still decrypts to the build's
  # unchanged flag. A promotion that installed another build's archive (or a
  # flag that moved under a rebuild) fails here rather than in front of a
  # competitor.
  pbuild=$(api GET "/builds/$PERSIST_BUILD")
  [[ "$(jq -r .has_artifacts <<<"$pbuild")" == true ]] ||
    fail "build $PERSIST_BUILD reports has_artifacts=false after its rebuild although it still publishes two files"
  [[ "$(jq -r .flag <<<"$pbuild")" == "$PERSIST_FLAG" ]] ||
    fail "build $PERSIST_BUILD's flag changed across the rebuild: every artifact a player had already downloaded would decrypt to the wrong answer"
  PERSIST_CHECKSUM_G2=$(jq -r .checksum <<<"$pbuild") # the failed-rebuild step below asserts the rolled-back row keeps exactly this
  code=$(artifact_get "$PERSIST_BUILD" secret.enc "$ART_TMP/secret-g2.enc")
  [[ "$code" == 200 ]] ||
    fail "GET /builds/$PERSIST_BUILD/secret.enc answered HTTP $code after the rebuild"
  if (( ${ART_OPENSSL:-0} )); then
    dec=$(openssl aes-256-cbc -d -k unguessable -pbkdf2 -in "$ART_TMP/secret-g2.enc" 2>/dev/null || true)
    [[ "$dec" == "$PERSIST_FLAG" ]] ||
      fail "secret.enc from the rebuilt bundle decrypts to '$dec', not to build $PERSIST_BUILD's flag '$PERSIST_FLAG'"
  fi
  ok "build $PERSIST_BUILD serves a new bundle at the same URL after the rebuild: generation 2 inside time.txt, the same two members, and a secret.enc that still decrypts to the build's flag"
else
  deselect "artifacts after the rebuild: the promoted bundle is generation 2"
fi

# ============================================================================

step "update, generation 3 with --prune-old: the generation displaced from rollback retention is untagged on the builder and in the registry"
set_generation 3 ondemand
out=$(cmgrd-cli update --prune-old)
sed 's/^/       /' <<<"$out"
grep -q "  $CH_ONDEMAND$" <<<"$out" || fail "$CH_ONDEMAND was not rebuilt"
if grep -q "  $CH_PERSISTENT$" <<<"$out"; then fail "$CH_PERSISTENT was rebuilt although its source did not change"; fi
OD_BUILD_G3=$(api GET "/builds/$OD_BUILD")
OD_TAG_G3=$(image_tag "$OD_BUILD_G3" challenge)
[[ "$(jq -r .prev_checksum <<<"$OD_BUILD_G3")" == "$(jq -r .checksum <<<"$OD_BUILD_G2")" ]] || fail "generation 2 is not the rollback target after the third build"
tags=$(registry_tags "$CH_ONDEMAND")
grep -qx "$OD_TAG_G3" <<<"$tags" || fail "generation 3 tag $OD_TAG_G3 was not pushed"
grep -qx "$OD_TAG_G2" <<<"$tags" || fail "generation 2 tag $OD_TAG_G2 left the registry although it is the rollback target"
if grep -qx "$OD_TAG_G1" <<<"$tags"; then fail "generation 1 tag $OD_TAG_G1 is still in the registry after --prune-old"; fi
# Retention is asserted against the registry above, which is where it lives for
# a registry deployment: the builder drops every generation at push, so its
# copies say nothing about what cork retains. What the builder is still good
# for is proving the purge ran at all.
btags=$(builder_tags)
if grep -qx "$E2E_REGISTRY/$CH_ONDEMAND:$OD_TAG_G3" <<<"$btags"; then
  fail "generation 3 is tagged on the builder after the rebuild: purgeBuiltImages should have dropped it at push (cmgr/purge.go)"
fi
for id in "${OD_IDS[@]}"; do
  assert_torn_down "$id"
done
OD_IDS=()
for i in 7 8 9 10; do
  inst=$(launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
  track "$inst" "$i"
done
for id in "${OD_IDS[@]}"; do
  check_ondemand "${INST_META[$id]}" "${INST_USER[$id]}" "E2E_GENERATION=3"
done
[[ -n "$(instance_on "$WA")" && -n "$(instance_on "$WB")" ]] || fail "the fresh launches did not land on both workers"
IMAGE_TAG[$CH_ONDEMAND]=$OD_TAG_G3
IMAGE_TAG[$CH_PERSISTENT]=$(image_tag "$(api GET "/builds/$PERSIST_BUILD")" challenge)
ok "registry and builder hold generations 2 and 3 only; 4 fresh instances across both workers serve generation 3"

############################################################################
# BLOCK 4 -- full-mode step, goes after the generation-3 step's ok (line 1260)
############################################################################

if (( FULL )); then
  step "missing tag: with $CH_MAKE's tag deleted from zot, the worker that already holds the image still launches it, and the worker that does not fails without the registry's fault being charged to the box"
  MK_TAG=${IMAGE_TAG[$CH_MAKE]}
  MK_REF="$E2E_REGISTRY/$CH_MAKE:$MK_TAG"
  COLD_WORKER=$WA
  if [[ "$MK_WORKER" == "$WA" ]]; then COLD_WORKER=$WB; fi
  # IMAGE_TAG holds the last non-builder host the registry step saw, so a
  # multi-image $CH_MAKE would leave a second tag in the registry and on the
  # cold worker, and both halves below would be about an image the launch does
  # not need. Say so rather than pass vacuously.
  mk_images=$(api GET "/builds/$MK_BUILD" | jq -r '[.images[] | select(.host != "builder")] | length')
  [[ "$mk_images" == 1 ]] ||
    fail "$CH_MAKE builds $mk_images launchable images: this step deletes the one tag IMAGE_TAG[$CH_MAKE] names, which only makes the pull fail while the challenge has exactly one"
  has_line "$MK_REF" worker_tags "$MK_WORKER" ||
    fail "worker $MK_WORKER does not hold $MK_REF although the remote-make step launched an instance of it there: the warm half of this step would have nothing to prove"
  # The restore path is a push from the builder, so it needs a local copy --
  # and with CMGR_PURGE_AFTER_PUSH on there is none, because cork drops it the
  # moment the push succeeds. Pull it back while the tag still resolves, which
  # makes the delete below reversible whichever way the purge is configured
  # rather than dependent on the builder having retained anything.
  builder_pull "$E2E_REGISTRY/$CH_MAKE" "$MK_TAG" ||
    fail "could not pull $MK_REF onto the builder, so the tag this step deletes from zot could not be pushed back afterwards; refusing to delete it rather than leave the registry short"
  has_line "$MK_REF" builder_tags ||
    fail "the builder does not hold $MK_REF after pulling it back from the registry"
  # Deterministic fixture: an earlier run may have placed binex101 on the other
  # worker too, and one leftover image there turns the cold half into a warm one.
  code=$(worker_status "$COLD_WORKER" "/images/$MK_REF" -X DELETE)
  case "$code" in
    200|404) ;;
    *) fail "could not remove $MK_REF from worker $COLD_WORKER (HTTP $code): the cold half of this step needs a worker that does not hold the image" ;;
  esac
  if has_line "$MK_REF" worker_tags "$COLD_WORKER"; then
    fail "worker $COLD_WORKER still holds $MK_REF after the delete"
  fi
  # Armed before the delete, not after: a run that dies in between still owes
  # the registry this tag, and the EXIT trap pushes it back from the builder.
  MAKE_TAG_DELETED=$MK_TAG
  # Deleted BY TAG, exactly as registryDeleteTag issues it and with the 202 it
  # accepts (cmgr/registry.go:57-110), so the manifest's blobs stay in zot and
  # the push that restores the tag at the end of this step is cheap.
  [[ "$(registry_status "/v2/$CH_MAKE/manifests/$MK_TAG" -X DELETE)" == 202 ]] ||
    fail "deleting the tag $MK_TAG of $CH_MAKE from $E2E_REGISTRY was not a 202: the fixture this step needs (a tag no pull can resolve) could not be made"
  if has_line "$MK_TAG" registry_tags "$CH_MAKE"; then
    fail "$MK_REF is still listed by the registry after its delete"
  fi
  note "deleted $MK_REF from zot: $MK_WORKER still holds the image, $COLD_WORKER does not"

  MK_CODE=""; MK_BODY=""; MK_TOOK=0
  mk_launch() { # mk_launch <dir> <tag> <the worker that is up>: one $MK_BUILD launch
    local dir=$1 tag=$2 up=$3 attempt
    for attempt in 1 2 3; do
      MK_CODE=000; MK_TOOK=0
      # An empty json body: binex101 takes no user_id or env, and timed_launch
      # records a refusal instead of raising it under errexit.
      timed_launch "$dir" "$tag" "$MK_BUILD" '{}'
      read -r MK_CODE MK_TOOK <"$dir/$tag.code" || true
      MK_BODY=$(cat "$dir/$tag.body" 2>/dev/null || true)
      # A 503 that names no pull is the one worker left standing being busy or
      # momentarily overloaded -- retryable by contract and none of this step's
      # business. A 503 that DOES name the pull is the mapping this step is
      # about and must reach the assertions below unretried.
      if [[ "$MK_CODE" == 503 ]] && ! grep -qE 'failed to pull image|image pull timed out' <<<"$MK_BODY"; then
        note "the launch was refused as retryable after ${MK_TOOK}s ($(head -c 140 <<<"$MK_BODY")); waiting for $up and trying again"
        retry 30 "worker $up to report ok before retrying the launch" health_is "$up" ok
        continue
      fi
      return
    done
  }

  # --- cold half: the pull the registry cannot serve ---
  retry 30 "both workers to report ok before the cold half" all_workers_ok
  DOWNED_WORKER=$MK_WORKER # armed before the call, so the trap owes a worker-add either way
  down_worker "$MK_WORKER" # $COLD_WORKER is the only worker up, so the launch is placed there
  cold_nets_before=$(worker_api "$COLD_WORKER" /networks | jq -r '.[].Name' | sort)
  cold_rows_before=$(worker_instances "$COLD_WORKER")
  mk_dir=$(mktemp -d)
  mk_launch "$mk_dir" cold "$COLD_WORKER"
  note "the launch on the image-less $COLD_WORKER answered HTTP $MK_CODE after ${MK_TOOK}s: $(head -c 160 <<<"$MK_BODY")"
  [[ "$MK_CODE" == 500 ]] ||
    fail "a launch whose pull the registry cannot serve answered HTTP $MK_CODE, expected 500: a missing manifest is none of the retryable classes (cmd/cmgrd/main.go:447-449), so the platform must not be told to place it again. If the team decides this should be a 503, this is the assertion that says so ($(head -c 200 <<<"$MK_BODY"))"
  if grep -qi '^Retry-After' "$mk_dir/cold.head"; then
    fail "the failed pull carried a Retry-After header: only the retryable classes set it (cmd/cmgrd/main.go:447-452), and the platform would re-place a launch no retry can fix until the build is repaired"
  fi
  # The failure has to name the image it could not get, so an operator reading
  # a 500 can tell a broken build from a broken box. Two wordings are legitimate
  # and which one appears depends on where the pull died: pullImage wraps a
  # STREAM error as "failed to pull image '<ref>'" (cmgr/docker.go:531-535),
  # but a manifest zot does not have fails ImagePull outright, and that path
  # returns the daemon's own error unwrapped (docker.go:521-523 logs the same
  # sentence but returns err). Both name the reference; only the reference is
  # asserted here, because the wrapper asymmetry is cork's to settle, not this
  # step's to pin.
  grep -qF "$MK_REF" <<<"$MK_BODY" || grep -q "failed to pull image" <<<"$MK_BODY" ||
    fail "the 500 names neither the image it could not pull nor the pull itself, so an operator cannot tell a missing tag from a broken worker ($(head -c 200 <<<"$MK_BODY"))"
  rm -rf "$mk_dir"
  # The negative control, and the assertion that costs a production box when it
  # regresses: a registry that cannot serve a manifest is a docker API error
  # that arrived over a working connection, not a transport failure
  # (isTransportError, cmgr/workers.go:514-539), so the worker must still be
  # taking placements. down is sticky and only a worker-add clears it, so this
  # single read is the whole assertion -- one broken tag would otherwise walk
  # the fleet down. overloaded is not a failure here (it clears itself), which
  # is why this asks about down and then waits for ok.
  ! health_is "$COLD_WORKER" down ||
    fail "worker $COLD_WORKER was marked down by a pull the registry refused: the failure was charged to the daemon (the noteWorkerTransportError call in ensureImages, cmgr/launch.go:142-150), and every worker that tried this tag would leave the fleet until an operator re-added it"
  retry 20 "worker $COLD_WORKER to report ok after the refused pull" health_is "$COLD_WORKER" ok
  if has_line "$MK_REF" worker_tags "$COLD_WORKER"; then
    fail "worker $COLD_WORKER holds $MK_REF although the tag is not in the registry: the launch did not fail for the reason this step arranged"
  fi
  # Nothing reached the daemon, so nothing may be recorded or left behind:
  # ensureImages runs before acquireLaunchSlot and startNetwork
  # (cmgr/launch.go:108-119), so started is false and clearInstanceRecords takes
  # the row, its ports and its container rows back (cmgr/api.go:369-380).
  cold_rows_after=$(worker_instances "$COLD_WORKER")
  [[ "$cold_rows_after" == "$cold_rows_before" ]] ||
    fail "the refused launch left an instance record on $COLD_WORKER ($cold_rows_before -> $cold_rows_after): a launch that never reached docker must cost no row and no port"
  # Read from the schema state, not from GET /builds/<id>: only GetSchemaState
  # populates BuildMetadata.Instances (cmgr/api.go), so a count taken from the
  # build alone would be zero however badly this leaked.
  mk_rows=$(api GET "/schemas/$SCHEMA_NAME" |
    jq -r --arg c "$CH_MAKE" '[.[] | select(.id == $c) | .builds[].instances[]?] | length')
  [[ "$mk_rows" == 0 ]] ||
    fail "cmgrd records $mk_rows instance(s) of $CH_MAKE after a launch that never reached a daemon"
  [[ "$(worker_api "$COLD_WORKER" /networks | jq -r '.[].Name' | sort)" == "$cold_nets_before" ]] ||
    fail "the refused launch left a network on $COLD_WORKER: the image stage runs before startNetwork (cmgr/launch.go:108-119), so nothing of this launch may have reached docker"
  readd "$MK_WORKER"
  DOWNED_WORKER=""

  # --- warm half: the launch that does not need the registry at all ---
  DOWNED_WORKER=$COLD_WORKER
  down_worker "$COLD_WORKER" # $MK_WORKER, which holds the image, is the only worker up
  mk_dir=$(mktemp -d)
  mk_launch "$mk_dir" warm "$MK_WORKER"
  [[ "$MK_CODE" == 200 || "$MK_CODE" == 201 ]] ||
    fail "the launch on $MK_WORKER, which already holds $MK_REF, answered HTTP $MK_CODE although nothing about it needs the registry: imagePresent must skip the pull for a tag the daemon already has, on the argument that content-addressed tags name the right content (cmgr/launch.go:155-176) ($(head -c 200 <<<"$MK_BODY"))"
  inst=$MK_BODY
  rm -rf "$mk_dir"
  id=$(jq -r .id <<<"$inst")
  [[ "$(jq -r .worker <<<"$inst")" == "$MK_WORKER" ]] ||
    fail "instance $id was placed on $(jq -r .worker <<<"$inst"), not on the only worker that was up ($MK_WORKER)"
  assert_on_worker "$MK_WORKER" "$inst"
  pub=$(jq -r .worker_public <<<"$inst")
  port=$(jq -r .ports.socat <<<"$inst")
  # Not merely a 201: a blind pull would have failed the launch outright with
  # the tag gone from zot, and an image that was not this build's would not
  # prompt. The prompt is what says the skipped pull started the right content.
  retry 30 "the BinEx101 prompt at $pub:$port" tcp_says "$pub" "$port" '1\n1\n' 'Give me a number'
  note "instance $id launched on $MK_WORKER in ${MK_TOOK}s and answered, with $MK_REF absent from the registry"
  cmgrd-cli stop "$id"
  assert_gone "$id" "$MK_WORKER" "$inst"
  readd "$COLD_WORKER"
  DOWNED_WORKER=""

  # Put the registry back exactly as this step found it -- the teardown step
  # reads these tags, and a tag this step removed would make its check pass for
  # the wrong reason. The builder still holds the image and carries cork's
  # read-write certs.d identity, so this is also the positive control for the
  # registry-identity step above: the very operation the worker's certificate
  # was refused succeeds with cork's.
  push_out=$(curl -sS --fail-with-body --connect-timeout 5 --max-time 180 \
    -X POST -H 'X-Registry-Auth: e30=' \
    "$E2E_BUILDER/images/$E2E_REGISTRY/$CH_MAKE/push?tag=$MK_TAG") ||
    fail "could not push $MK_REF back into $E2E_REGISTRY from the builder: the registry is now one tag short of what this step was handed"
  if grep -qiE 'errorDetail|denied|unauthorized' <<<"$push_out"; then
    fail "the builder's push of $MK_REF was refused although it carries cork's read-write identity (e2e/zot/config.json grants \"cmgr\" create/update/delete): $(tail -c 200 <<<"$push_out")"
  fi
  has_line "$MK_TAG" registry_tags "$CH_MAKE" ||
    fail "$MK_REF is not back in the registry after the restoring push: the teardown step's registry checks would now be reading a tag this step removed"
  MAKE_TAG_DELETED=""
  retry 30 "both workers to report ok after the missing-tag step" all_workers_ok
  ok "with $MK_TAG deleted from zot: the cold $COLD_WORKER answered 500 naming the failed pull, carried no Retry-After, stayed ok and kept no row, port or network; the warm $MK_WORKER launched from the image it already held and served the BinEx101 prompt; the tag is back in the registry"
else
  deselect "missing tag: a warm worker launches with the tag gone from zot, a cold one fails without being downed"
fi

# ---------------------------------- 9c. rebuild while launches are in flight

step "rebuild under live traffic: launches in flight during an update are left alone, never answered 500, and never left half torn down"
set_generation 4 ondemand
TRAFFIC_DIR=$(mktemp -d)
TRAFFIC_STOP="$TRAFFIC_DIR/stop" # cleanup() touches this if the run dies mid-step
TRAFFIC_REC="$TRAFFIC_DIR/attempts" # "<http status> <instance id or ->", one line per launch
TRAFFIC_DEL="$TRAFFIC_DIR/deleted"  # the ids a launcher tried to stop again
TRAFFIC_PIDS=()
for n in 1 2 3 4; do
  # One pair of files per launcher, concatenated once they are all reaped:
  # four writers appending to one file is only atomic by convention, and the
  # whole step is judged from these records.
  : >"$TRAFFIC_DIR/attempts.$n"
  : >"$TRAFFIC_DIR/deleted.$n"
  (
    # errexit off inside the launcher: a launch the fleet refuses must end the
    # attempt, not the loop, or the window stops being populated. Nothing is
    # judged in here either - fail in a background subshell would exit only the
    # subshell - every verdict below is taken in the foreground from the files.
    set +e
    # A wedged cmgrd must cost the drain below one attempt, not API_TIMEOUT's
    # ten minutes; a launch here is seconds even with a cold pull. Local to the
    # subshell, so the foreground keeps its 600s.
    API_TIMEOUT=120
    held=""
    while [[ ! -e "$TRAFFIC_STOP" ]]; do
      out=$(try_launch "$OD_BUILD" "e2e-traffic-$n" "e2e-traffic-$n")
      code=${out%%$'\n'*}
      body=${out#*$'\n'}
      id=$(jq -r '.id // empty' <<<"$body" 2>/dev/null)
      printf '%s %s\n' "$code" "${id:--}" >>"$TRAFFIC_DIR/attempts.$n"
      # Stop the *previous* instance, not this one: each launcher then leaves
      # exactly one instance standing when the loop ends - the one it launched
      # deepest into the rebuild, which is what assertion (c) inspects - while
      # never holding more than two instances' ports at a time. The id is
      # recorded as attempted-stopped before the call, so an instance whose
      # stop was refused (503) is out of (c)'s scope rather than judged as a
      # survivor; the sweep below stops it for real.
      if [[ -n "$held" ]]; then
        printf '%s\n' "$held" >>"$TRAFFIC_DIR/deleted.$n"
        api_status DELETE "/instances/$held" >/dev/null
      fi
      held=$id
      # A refusal costs nothing, so without this a window in which both workers
      # are momentarily overloaded would spin thousands of requests at cmgrd
      # and measure the spin rather than the rebuild.
      [[ -n "$id" ]] || sleep 1
    done
    exit 0
  ) &
  TRAFFIC_PIDS+=("$!")
done
t=$(date +%s)
# --prune-old as in the other update steps, and it is not optional here: a
# fourth generation rotates retention to {4, 3}, and plain `update` would leave
# generation 2 tagged forever, which the teardown step reads as a leak. The
# pruned generation is the displaced one (2), which no launch in flight can be
# reading - the generation-3 step already proved fresh launches serve 3.
# The status is captured rather than left to errexit: the launchers must be
# stopped and their records printed before this step fails, or a failing update
# leaves four loops hammering a fleet nobody is reading.
rc=0
out=$(cmgrd-cli update --prune-old) || rc=$?
touch "$TRAFFIC_STOP"
wait "${TRAFFIC_PIDS[@]}" || true # every launcher exits 0; the guard is for a killed one
cat "$TRAFFIC_DIR"/attempts.* >"$TRAFFIC_REC"
cat "$TRAFFIC_DIR"/deleted.* >"$TRAFFIC_DEL"
TRAFFIC_STOP="" # the loops are reaped; cleanup() has nothing left to stop
sed 's/^/       /' <<<"$out"
(( rc == 0 )) ||
  fail "cmgrd-cli update exited $rc with launches in flight: a rebuild must not fail because instances of the build it rebuilds are being launched"
attempts=$(wc -l <"$TRAFFIC_REC" | tr -d ' ')
admitted=$(grep -cE '^(200|201) ' "$TRAFFIC_REC" || true)
note "rebuilt in $(( $(date +%s) - t ))s under $attempts launch attempt(s), $admitted of them admitted"
grep -q "  $CH_ONDEMAND$" <<<"$out" || fail "$CH_ONDEMAND was not rebuilt under live traffic"
if grep -q "  $CH_PERSISTENT$" <<<"$out"; then fail "$CH_PERSISTENT was rebuilt although only the on-demand source changed"; fi
# The instances the generation-3 step left had finalized long before the rebuild
# read the build's instance list, so they are the ordinary case: torn down, not
# skipped. Clearing them here also lets assertion (d) read against a single
# legitimate id.
for id in "${OD_IDS[@]}"; do
  assert_torn_down "$id"
done
OD_IDS=()

# (a) The window really was populated. Without this the rest passes just as well
# when the rebuild finished before the launchers got a request away. Four is one
# attempt per launcher: anything less means the loops never ran.
(( attempts >= 4 )) ||
  fail "only $attempts launch attempt(s) overlapped the rebuild: the window was not populated, so nothing here was exercised"
# Only an admitted launch reaches openInstance and can be the unfinalized row
# the rebuild has to skip. A window in which every attempt was refused (both
# workers over the telemetry agent's 90% threshold, which is a busy host, not a
# cork bug) is not a failure - but it is not coverage either, and the run must
# say so rather than report this step as proof.
if (( admitted == 0 )); then
  skip "rebuild under live traffic: every launch in the window was refused, so no launch was in flight for the rebuild to skip"
fi

# (b) The regression. A 503 during a rebuild is legitimate - a busy or
# overloaded worker, a worker that went down under the launch, a lost database
# write - and the platform's celery worker retries it (see the retryable classes
# in cmd/cmgrd/main.go:449). A 500 is what it cannot retry, and it is exactly
# what the rebuild produces without the guard at database_challenges.go:719:
# stopInstance would delete an unfinalized row under a running launch, its
# container and port rows cascade with it, and that launch's finalizeInstance
# then fails on the foreign key - an error no retryable class covers. 000 means
# cmgrd did not answer at all.
if bad=$(grep -vE '^(200|201|503) ' "$TRAFFIC_REC"); then
  fail "launches during the rebuild were answered outside {200,201,503}: $(tr '\n' ';' <<<"$bad" | head -c 200) - a 500 here is the rebuild stopping an instance whose launch had not finalized (the !IsFinalized guard at database_challenges.go:719)"
fi

# (c) Nothing may be left half-built. Of the ids the launchers recorded as 2xx,
# the ones they never tried to stop are the instances each was holding when the
# loop ended; whatever cmgrd still reports for one of them has to be whole on
# its worker and answering.
alive=()
while read -r code id; do
  case "$code" in 200|201) ;; *) continue ;; esac
  [[ "$id" != - ]] || fail "cmgrd answered HTTP $code to a launch with no instance id in the body"
  grep -qx "$id" "$TRAFFIC_DEL" || alive+=("$id")
done <"$TRAFFIC_REC"
served=0
for id in ${alive[@]+"${alive[@]}"}; do
  code=$(api_status GET "/instances/$id")
  # 404 is not a hole: an instance that had finalized before the rebuild read
  # the build's instance list is torn down like any other on-demand one. The
  # forbidden state is a record that outlived its containers.
  if [[ "$code" == 404 ]]; then continue; fi
  [[ "$code" == 200 ]] ||
    fail "instance $id, launched during the rebuild, answers HTTP $code on GET /instances/$id"
  meta=$(api GET "/instances/$id")
  assert_on_worker "$(jq -r .worker <<<"$meta")" "$meta"
  pub=$(jq -r .worker_public <<<"$meta")
  port=$(jq -r .ports.server <<<"$meta")
  retry 30 "instance $id at $pub:$port" quiet http_body "$pub" "$port"
  body=$(http_body "$pub" "$port")
  # Deliberately no generation assertion: Start's build read is the launch's
  # whole view of the build (api.go:276), so a launch that began before the
  # rebuild stamped the new checksum legitimately finishes on generation 3.
  # The flag is the invariant across a rebuild, and the generation-2 step
  # proves it does not change.
  grep -qxF "FLAG=$OD_FLAG" <<<"$body" ||
    fail "instance $id survived the rebuild but does not serve the build's flag: $body"
  served=$((served + 1))
done
note "$served of ${#alive[@]} instance(s) still standing when the launchers stopped are whole on their worker and serving"

# Every id the launchers recorded, whether or not they got to stop it: DELETE is
# a 204 no-op on one already gone (the stop-path step proves that), so this is
# what leaves the fleet as it was found. The status is checked, because a stop
# that quietly failed here is a container left on a worker for the rest of the
# run; a 503 is retried once, being the one answer cmgrd documents as retryable.
while read -r code id; do
  [[ "$id" != - ]] || continue
  dcode=$(api_status DELETE "/instances/$id")
  if [[ "$dcode" == 503 ]]; then dcode=$(api_status DELETE "/instances/$id"); fi
  [[ "$dcode" == 204 ]] ||
    fail "stopping instance $id, launched during the rebuild, answered HTTP $dcode twice; its containers are still on its worker"
done <"$TRAFFIC_REC"
rm -rf "$TRAFFIC_DIR"

# (d) Nothing of this step is left running anywhere. The launchers created
# instances the scenario never tracked, so a container or network left behind
# would otherwise surface only as a port collision or a stale-network failure
# several steps later - and a launch refused after it had already reached the
# daemon carries no id for the sweep above to name. Containers and networks are
# attributed to an instance by their cmgr-<id> name, exactly as reconcileWorker
# does. OD_IDS is empty here and the replacements are launched below, so the
# persistent instance is the only id either worker may still hold.
filters=$(jq -rn '{label: ["cmgr.managed=true"]} | tostring | @uri')
for ip in "${WORKER_IPS[@]}"; do
  managed=$(worker_api "$ip" "/containers/json?all=true&filters=$filters")
  nets=$(worker_api "$ip" /networks)
  for sid in $(jq -r '.[] | (.NetworkSettings.Networks // {}) | keys[] | select(startswith("cmgr-")) | ltrimstr("cmgr-")' <<<"$managed"); do
    [[ "$sid" == "$PERSIST_INST" ]] ||
      fail "worker $ip still runs a cmgr.managed container of instance $sid, which no record accounts for: a launch that overlapped the rebuild was left behind"
  done
  for sid in $(jq -r '.[].Name | select(startswith("cmgr-")) | ltrimstr("cmgr-")' <<<"$nets"); do
    [[ "$sid" == "$PERSIST_INST" ]] ||
      fail "worker $ip still has the network cmgr-$sid, which no record accounts for: a launch that overlapped the rebuild was torn down only halfway"
  done
done

# Retention rotated by one: 4 current, 3 the rollback target, 2 retired. The
# teardown step checks the generation the scenario last built, so hand it this
# one - and check generation 2 out of the registry here, since after this step
# remove-schema no longer reaches it.
OD_BUILD_G4=$(api GET "/builds/$OD_BUILD")
OD_TAG_G4=$(image_tag "$OD_BUILD_G4" challenge)
[[ "$(jq -r .prev_checksum <<<"$OD_BUILD_G4")" == "$(jq -r .checksum <<<"$OD_BUILD_G3")" ]] ||
  fail "generation 3 is not the rollback target after the fourth build"
tags=$(registry_tags "$CH_ONDEMAND")
grep -qx "$OD_TAG_G4" <<<"$tags" || fail "generation 4 tag $OD_TAG_G4 was not pushed"
grep -qx "$OD_TAG_G3" <<<"$tags" || fail "generation 3 tag $OD_TAG_G3 left the registry although it is the rollback target"
if grep -qx "$OD_TAG_G2" <<<"$tags"; then fail "generation 2 tag $OD_TAG_G2 is still in the registry after --prune-old"; fi
IMAGE_TAG[$CH_ONDEMAND]=$OD_TAG_G4

# Put the fleet back the way the generation-3 step left it: tracked on-demand
# instances, one of them on $WB, which the worker-down step takes as given. A
# worker the storm pushed to overloaded recovers by itself (the agent's
# hysteresis clears below 80%); one that went down would not, and saying so here
# beats a confusing placement failure below. ensure_instance_on, not a placement
# assertion on two consecutive launches: round robin over a worker that is
# briefly skipped is not something this step should fail on.
retry 30 "both workers to report ok after the launch storm" all_workers_ok
for i in 23 24; do
  inst=$(launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
  track "$inst" "$i"
done
ensure_instance_on "$WB" 25
for id in "${OD_IDS[@]}"; do
  check_ondemand "${INST_META[$id]}" "${INST_USER[$id]}" "E2E_GENERATION=4"
done
ok "$attempts launches overlapped the rebuild ($admitted admitted): none was answered 500, the ones left standing are whole and serving the build's flag, no untracked container or network survived on either worker, retention rotated to generations 4 and 3, and fresh launches serve generation 4"

# BLOCK 2 — goes after the "rebuild under live traffic" step's ok line
# (scenario.sh:1477), before the "10. worker-down path" banner.
# =========================================================================

# ------------------- 9d. an update without --prune-old (full mode only)

if (( FULL )); then
step "update, generation 5 without --prune-old: the generation it displaces stays tagged on the builder and in the registry"
# --prune-old is opt-in, and this is the only step that runs an update without
# it. There is no free way to get here: at generation 2 nothing is displaced
# yet (displacedPruneCandidate is false, cmgr/database_challenges.go), so
# dropping the flag from an earlier step would prove nothing -- the case needs
# its own generation and therefore its own rebuild. What it pins is that an
# update untags NOTHING of its own accord: a content tag may be referenced by
# another cmgr instance on the same daemon, so reclaiming one is the operator's
# call (UpdateOptions.PruneOldImages, cmgr/api.go; the displaced tags are
# collected only under `if pruneOldImages && displacedPruneCandidate(...)`,
# cmgr/database_challenges.go).
set_generation 5 ondemand
t=$(date +%s)
rc=0
out=$(cmgrd-cli update) || rc=$? # no --prune-old: that omission is the whole subject
sed 's/^/       /' <<<"$out"
(( rc == 0 )) ||
  fail "cmgrd-cli update exited $rc building generation 5 of $CH_ONDEMAND"
# Armed the moment the rebuild returns, not once the assertions are through:
# from here the displaced generation is orphaned, and nothing in cork ever
# reclaims it, so a run that dies below still owes the builder a cleanup.
ORPHAN_TAG="$E2E_REGISTRY/$CH_ONDEMAND:$OD_TAG_G3"
note "rebuilt in $(( $(date +%s) - t ))s"
grep -q "  $CH_ONDEMAND$" <<<"$out" || fail "$CH_ONDEMAND was not rebuilt"
if grep -q "  $CH_PERSISTENT$" <<<"$out"; then fail "$CH_PERSISTENT was rebuilt although only the on-demand source changed"; fi
OD_BUILD_G5=$(api GET "/builds/$OD_BUILD")
OD_TAG_G5=$(image_tag "$OD_BUILD_G5" challenge)
[[ "$(jq -r .checksum <<<"$OD_BUILD_G5")" != "$(jq -r .checksum <<<"$OD_BUILD_G4")" ]] ||
  fail "build $OD_BUILD checksum did not change: generation 5 was not built, so nothing was displaced"
# The precondition, and the reason this step is not vacuous: retention rotated
# to {5, 4}, which is what pushes generation 3 out of it. Were the rotation
# broken, generation 3 would still be the rollback target and its survival
# below would say nothing about --prune-old.
[[ "$(jq -r .prev_checksum <<<"$OD_BUILD_G5")" == "$(jq -r .checksum <<<"$OD_BUILD_G4")" ]] ||
  fail "generation 4 is not the rollback target after the fifth build: retention did not rotate, so generation 3 was never displaced and this step proves nothing (rotatedPrevChecksum, cmgr/database_challenges.go)"
[[ "$(jq -r .checksum <<<"$OD_BUILD_G5")" != "$(jq -r .checksum <<<"$OD_BUILD_G3")" &&
   "$(jq -r .prev_checksum <<<"$OD_BUILD_G5")" != "$(jq -r .checksum <<<"$OD_BUILD_G3")" ]] ||
  fail "generation 3 is still cork's current or rollback generation after the fifth build: it was not displaced"

# The assertions. Generation 3 is now referenced by nothing cork retains, and
# it must still be tagged in both places all the same.
tags=$(registry_tags "$CH_ONDEMAND")
grep -qx "$OD_TAG_G5" <<<"$tags" || fail "generation 5 tag $OD_TAG_G5 was not pushed"
grep -qx "$OD_TAG_G4" <<<"$tags" || fail "generation 4 tag $OD_TAG_G4 left the registry although it is the rollback target"
grep -qx "$OD_TAG_G3" <<<"$tags" ||
  fail "an update without --prune-old untagged the displaced generation 3 ($OD_TAG_G3) in the registry: displaced tags are collected only when PruneOldImages is set (cmgr/api.go, cmgr/database_challenges.go), because a content tag may be referenced by another cmgr on the same daemon"
# The builder has no copy of any of these to check: cork drops each generation
# at push, so "an update without --prune-old leaves the displaced generation
# alone" is a claim about the registry here, which is the copy that matters.
note "generation 3 ($OD_TAG_G3) is referenced by nothing cork retains and is still tagged in zot"
# What the teardown reads, and what every later plant_orphan and the
# incomplete-reconcile blocker build their container from. Set before the
# relaunches, so the pulls below are the ones that put it on both boxes.
IMAGE_TAG[$CH_ONDEMAND]=$OD_TAG_G5

# The rebuild tore the on-demand instances down like any other, and the fleet
# has to look to the following steps exactly as the live-traffic step left it.
for id in "${OD_IDS[@]}"; do
  assert_torn_down "$id"
done
OD_IDS=()

# Nothing in cork ever reclaims that orphan -- a later --prune-old only
# collects the generation its OWN rebuild displaces, and remove-schema's
# destroyImages knows the current and rollback generations only -- which is the
# second half of the promise and the operator-visible price of the flag. The
# harness therefore has to, exactly as an operator would, and only now that the
# assertions above have been made: the teardown step reads the builder for any
# surviving challenge image, and it must find the same thing in both modes.
# By tag against zot, the way cmgr/registry.go registryDeleteTag does it (a
# digest delete would take every tag sharing the manifest).
dcode=$(registry_status "/v2/$CH_ONDEMAND/manifests/$OD_TAG_G3" -X DELETE)
[[ "$dcode" == 202 || "$dcode" == 200 || "$dcode" == 404 ]] ||
  fail "deleting the orphaned generation 3 tag from the registry answered HTTP $dcode (cork accepts 202 and 404 from the same call, cmgr/registry.go registryDeleteTag)"
orphan_tag_gone() { ! has_line "$OD_TAG_G3" registry_tags "$CH_ONDEMAND"; }
retry 10 "the orphaned generation 3 tag $OD_TAG_G3 to leave the registry" orphan_tag_gone
dcode=$(builder_untag "$E2E_REGISTRY/$CH_ONDEMAND:$OD_TAG_G3")
[[ "$dcode" == 200 || "$dcode" == 404 ]] ||
  fail "untagging the orphaned generation 3 image on the builder answered HTTP $dcode: the teardown's 'the builder holds no challenge images' check will trip over what is left"
if has_line "$E2E_REGISTRY/$CH_ONDEMAND:$OD_TAG_G3" builder_tags; then
  fail "the orphaned generation 3 image is still tagged on the builder after this step untagged it; the teardown invariant would not hold"
fi
ORPHAN_TAG=""

# Back to the shape the live-traffic step hands on: tracked on-demand
# instances, at least one on each worker. Both halves matter -- the worker-down
# step needs one on $WB, and the incomplete-reconcile step builds its blocker
# container on $WB from ${IMAGE_TAG[$CH_ONDEMAND]}, which only a launch there
# pulls. The ok wait comes first: launch() goes through api (--fail-with-body),
# so a launch refused because both boxes are briefly overloaded after the
# rebuild would abort the run with no message.
retry 30 "both workers to report ok after the rebuild" all_workers_ok
for i in 23 24; do
  inst=$(launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
  track "$inst" "$i"
done
# ensure_instance_on rather than an assertion that two launches land one each:
# a worker briefly skipped by round robin is not something this step should
# fail on, and both calls are free whenever the two launches split as usual.
ensure_instance_on "$WB" 25
ensure_instance_on "$WA" 26
for id in "${OD_IDS[@]}"; do
  check_ondemand "${INST_META[$id]}" "${INST_USER[$id]}" "E2E_GENERATION=5"
done
for ip in "${WORKER_IPS[@]}"; do
  has_line "$E2E_REGISTRY/$CH_ONDEMAND:$OD_TAG_G5" worker_tags "$ip" ||
    fail "worker $ip does not hold $E2E_REGISTRY/$CH_ONDEMAND:$OD_TAG_G5 after the relaunches: the later steps that create a container from that tag on this box would fail on a missing image"
done
ok "an update without --prune-old rotated retention to generations 5 and 4 and left the displaced generation 3 tagged on the builder and in zot; the harness reclaimed that orphan, since nothing in cork ever will, and ${#OD_IDS[@]} fresh instances serve generation 5 across both workers"
else
  deselect "update, generation 5 without --prune-old"
fi


# =========================================================================

# --------------------------------------------------- 10. worker-down path

step "worker-down: a down worker is skipped for placement; stops on it clear cmgrd's records without touching docker"
down_worker "$WB"
for i in 11 12; do
  inst=$(launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
  track "$inst" "$i"
  [[ "$(jq -r .worker <<<"$inst")" == "$WA" ]] || fail "instance $(jq -r .id <<<"$inst") was not placed on the remaining worker"
done
victim=$(instance_on "$WB")
[[ -n "$victim" ]] || fail "no instance lives on $WB to exercise the down-worker stop path"
vmeta=${INST_META[$victim]}
cmgrd-cli stop "$victim"
[[ "$(api_status GET "/instances/$victim")" == 404 ]] || fail "instance $victim is still known after stop"
assert_on_worker "$WB" "$vmeta"
note "instance $victim cleared from cmgrd; its container still runs on $WB until the worker is re-added"
readd "$WB" # returns only once $WB reports ok
orphan_gone "$WB" "$victim" "$vmeta" ||
  fail "worker $WB reported ok while the leftovers of instance $victim were still on it"
forget "$victim"
ok "launches went to $WA only, the stop on $WB was DB-only, worker-add brought $WB back and removed the leftovers"

# ------------------------------------------ 10b. a leftover that will not go

# ------------------------------ 10b. a refused removal: an incomplete pass

step "a leftover the daemon refuses to remove: the pass is incomplete, not unreachable, so $WB rejoins the fleet loudly and everything removable still goes"
# Two orphans on $WB, both cmgr-<id> networks of instances no row names, but
# only one of them removable:
#   $BLOCKER_NET   held open by a running container the pass cannot see, so
#                  its NetworkRemove comes back 403 for as long as it runs.
#   cmgr-998       a plain DB-only-stop leftover, exactly what plant_orphan
#                  makes; the pass must still clear it.
# A 403 is not isTransportError (workers.go:521-539), so reconcileFailed
# calls the pass incomplete rather than unreachable (worker_reconcile.go:179-186)
# and reconcileWithRetries then lets the box into placement with an error
# logged instead of marking it down (workers.go:341-348). No other step in
# this scenario ever produces an incomplete pass, so this is the only cover
# that classification has.
BLOCKER_WORKER=$WB # armed before the create: a 409 below means a killed run left one, and the trap should still take it
code=$(worker_status "$WB" /networks/create -X POST -H 'Content-Type: application/json' \
  -d "$(jq -cn --arg n "$BLOCKER_NET" '{Name: $n, Driver: "bridge"}')")
[[ "$code" == 201 ]] ||
  fail "could not plant the blocker network $BLOCKER_NET on $WB: HTTP $code (a run killed before its EXIT trap ran may have left one; sweep_worker clears it at fleet-wait)"
# cmgr.managed is set to a value other than "true" on purpose, and must not be
# left off: a container inherits the labels of its image, and every image cmgr
# builds carries cmgr.managed=true (docker.go:656). Omitting the key would
# make reconcileWorker's label-filtered ContainerList see the blocker, remove
# it, free the network, and the step would test a clean pass instead.
blocker=$(worker_api "$WB" "/containers/create?name=$BLOCKER_NAME" -X POST -H 'Content-Type: application/json' \
  -d "$(jq -cn --arg img "$E2E_REGISTRY/$CH_ONDEMAND:${IMAGE_TAG[$CH_ONDEMAND]}" --arg net "$BLOCKER_NET" \
        '{Image: $img, Labels: {"cmgr.managed": "e2e-blocker"}, HostConfig: {NetworkMode: $net}}')" | jq -r .Id)
[[ -n "$blocker" && "$blocker" != null ]] || fail "could not create the blocker container $BLOCKER_NAME on $WB"
code=$(worker_status "$WB" "/containers/$blocker/start" -X POST)
[[ "$code" == 204 ]] || fail "could not start the blocker container on $WB: HTTP $code"
# Running, not merely created: a stopped container releases its endpoint and
# dockerd then removes the network happily, which is the opposite of the state
# under test.
[[ "$(worker_api "$WB" "/containers/$blocker/json" | jq -r .State.Running)" == true ]] ||
  fail "the blocker container on $WB is not running, so $BLOCKER_NET would not refuse removal"
# The guard on the paragraph above, asked with the filter reconcileWorker
# itself uses: if the daemon ever hands this container back as cmgr-managed,
# the pass removes it and every assertion below would pass on a clean
# reconcile that proves nothing.
blocker_filter=$(jq -rn '{label: ["cmgr.managed=true"]} | tostring | @uri')
if grep -qx "$blocker" <<<"$(worker_api "$WB" "/containers/json?all=true&filters=$blocker_filter" | jq -r '.[].Id')"; then
  fail "the blocker container on $WB matches cmgr.managed=true: reconcileWorker would remove it, and this step would no longer exercise a refused removal"
fi
planted=$(plant_orphan "$WB" 998)
note "planted $BLOCKER_NET, held open by the label-invisible container $BLOCKER_NAME, and a removable orphan (cmgr-998) on $WB"

# worker-add rebuilds the conn, which starts overloaded (workers.go:298) and
# only turns ok once runWorker's reconcile has given up and started the
# poller: the wall clock from here to ok is how long the pass was retried.
# Spelled out rather than calling readd so the timeout message can name the
# behaviour under test.
t=$(date +%s)
cmgrd-cli worker-add "$WB" "${PUBLIC[$WB]}"
retry 40 "worker $WB to rejoin placement with $BLOCKER_NET still on it (a refused removal must leave the pass incomplete, not unreachable: reconcileWithRetries marks the worker down only for the unreachable verdict, workers.go:341-348)" \
  health_is "$WB" ok
took=$(( $(date +%s) - t ))

# The sharp assertion is the lower bound, not an upper one. reconcileWithRetries
# cannot return while the removal keeps being refused until its deadline
# (budget = CMGR_WORKER_MAX_MISSES x poll interval = 20 x 500ms = 10s), so a
# worker that is ok in a second or two never retried: it took the pass for
# done, which is exactly what happens if a refusal stops counting as
# incomplete. Half the budget, so only the budget itself shrinking can fire
# this, never a slow box.
(( took >= 5 )) ||
  fail "worker $WB was ok ${took}s after worker-add although $BLOCKER_NET could not be removed: the pass was accepted as done instead of being retried for its 10s budget (worker_reconcile.go:137-173, workers.go:328-355)"

# The negative control. Without it every assertion below would pass just as
# well if dockerd had quietly removed the network: nothing would have been
# incomplete and the step would prove only that a clean reconcile works.
[[ "$(worker_status "$WB" "/networks/$BLOCKER_NET")" == 200 ]] ||
  fail "$BLOCKER_NET is gone from $WB: the reconcile pass finished after all (did the blocker container stop?), so this step no longer exercises an incomplete pass"

# One refusal costs only that leftover: the removals run independently
# (removeOrphans), so cmgr-998 must be gone even though $BLOCKER_NET is not.
# Not retried on purpose -- health only turned ok after the whole budget, by
# which time twenty passes have run, so a retry here could only hide a pass
# that gave up early.
orphan_gone "$WB" 998 "{\"containers\":[\"$planted\"]}" ||
  fail "worker $WB rejoined with the removable orphan cmgr-998 still on it: the refused network removal aborted the rest of the pass instead of costing only its own leftover"

# Really in placement, not merely reported ok, and while the leftover is still
# sitting on the box -- that is the whole point of the incomplete verdict.
# Round robin over two ok workers needs at most two launches (selectWorker,
# workers.go:699), as ensure_instance_on relies on too.
mine=()
placed=""
for i in 23 24; do
  inst=$(launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
  id=$(jq -r .id <<<"$inst")
  track "$inst" "$i"
  mine+=("$id")
  if [[ "${INST_WORKER[$id]}" == "$WB" ]]; then placed=$id; break; fi
done
[[ -n "$placed" ]] ||
  fail "two launches both avoided $WB although it reports ok after the incomplete pass: the worker rejoined the fleet on paper only"
# Deliberately no generation argument: this step is about a worker rejoining
# placement after a pass it could not finish, and which generation is current
# depends on how many update steps run before it.
check_ondemand "${INST_META[$placed]}" "${INST_USER[$placed]}"
note "instance $placed launched onto $WB and serves, with $BLOCKER_NET still refusing removal"
for id in "${mine[@]}"; do
  cmgrd-cli stop "$id"
  assert_gone "$id" "${INST_WORKER[$id]}" "${INST_META[$id]}"
  forget "$id"
done

# Housekeeping, and the last assertion: the blocker does not carry
# cmgr.managed=true, so neither cmgrd nor sweep_worker's label-scoped loop
# would ever remove it. Container first -- while it runs the network refuses
# to go.
code=$(worker_status "$WB" "/containers/$blocker?force=true" -X DELETE)
case "$code" in
  204|404|409) ;;
  *) fail "could not remove the blocker container $BLOCKER_NAME from $WB: HTTP $code" ;;
esac
retry 15 "the blocker container to leave $WB" container_gone "$WB" "$blocker"
# Retried rather than deleted once: dockerd can hold the endpoint for a moment
# after the container is gone, and a $BLOCKER_NET surviving this step would
# make every later worker-add on $WB spend the whole 10s reconcile budget
# before it could take placements.
blocker_net_removed() {
  local c
  c=$(worker_status "$WB" "/networks/$BLOCKER_NET" -X DELETE)
  [[ "$c" == 204 || "$c" == 404 ]]
}
retry 15 "$BLOCKER_NET to leave $WB once its container is gone" blocker_net_removed
[[ "$(worker_status "$WB" "/networks/$BLOCKER_NET")" == 404 ]] ||
  fail "$BLOCKER_NET is still on $WB after its blocker was removed"
BLOCKER_WORKER=""
ok "the refused removal of $BLOCKER_NET left the pass incomplete: $WB retried for ${took}s of its 10s budget, then rejoined the fleet, removed the orphan it could remove (cmgr-998), and took a real launch (instance $placed); the blocker and $BLOCKER_NET are gone from $WB"

# ------------------------------------------------ 11. telemetry silence

step "telemetry silence: a worker whose agent goes quiet is marked down after 30s and stays down until re-added"
if (( OUTER )); then
  sidecar=$(compose_container "${PUBLIC[$WB]}-telemetry")
  [[ -n "$sidecar" ]] || fail "no container for compose service ${PUBLIC[$WB]}-telemetry"
  ensure_instance_on "$WB" 16
  id=$(instance_on "$WB")
  STOPPED_SIDECAR=$sidecar
  outer_ctl "$sidecar" stop
  note "stopped ${PUBLIC[$WB]}-telemetry; cmgrd tolerates 10s of silence here (CMGR_WORKER_MAX_MISSES=20 at 500ms)"
  retry 20 "worker $WB to be marked down" health_is "$WB" down
  assert_on_worker "$WB" "${INST_META[$id]}"
  ok "worker $WB went down on telemetry silence; instance $id keeps running on it"
  outer_ctl "$sidecar" start
  STOPPED_SIDECAR=""
  retry 30 "telemetry on $WB" quiet curl -sSf --max-time 3 "http://$WB:2136/health"
  sleep 3
  health_is "$WB" down || fail "worker $WB recovered on its own; down must be sticky"
  readd "$WB"
  ok "down stayed sticky with telemetry back; worker-add rebuilt the connection and $WB is ok"
else
  skip "needs the outer docker socket"
fi

# BLOCK 3 — goes after the telemetry-silence step's closing "fi"
# (scenario.sh:1659), before the "12. overloaded" banner.
# =========================================================================

# --------------------------- 11b. a telemetry agent that hangs (full mode)

if (( FULL )); then
  if (( OUTER )); then
    step "wedged telemetry: an agent that accepts every poll and answers none is bounded by the poll timeout, so its worker still goes down"
    # The silence step above STOPS the sidecar, so every poll is refused in
    # microseconds and misses accumulate whatever the per-poll timeout is:
    # nothing else in this scenario makes a poll hang, and pollWorker's
    # `&http.Client{Timeout: timing.pollTimeout}` (cmgr/workers.go) does no
    # work today. An agent that completes the handshake and then never writes
    # is the failure that timeout exists for -- a wedged agent, or a box
    # thrashing so hard it never gets to answer. Without the timeout the
    # worker's single poll goroutine blocks in that one Get forever, `misses`
    # never increments, and the box sits at ok taking placements its dockerd
    # may no longer be able to serve. The minPollInterval floor and the
    # half-interval clamp (cmgr/workers.go) exist so this timeout can never be
    # configured to zero, i.e. to unbounded.
    ensure_instance_on "$WB" 28
    id=$(instance_on "$WB")
    DOWNED_WORKER=$WB # set before the sidecar stops: from here cork will down $WB and the trap owes it a worker-add
    fake_hung_telemetry "$WB"
    fake=$LAST_FAKE
    # The control that separates this step from the silence step, and the one
    # that keeps it from passing for the old reason: a poll must HANG, not be
    # refused. curl exits 28 when the connection was accepted and nothing came
    # back, and 7 when nothing is listening. Retried, because the responder
    # has to bind 2136 after the sidecar has released it.
    retry 20 "the wedged agent on $WB to accept a connection and never answer" telemetry_hangs "$WB"
    # The clock is anchored here, not at the responder's start, and this is
    # what makes the step deterministic: worker-add rebuilds the conn and its
    # poller (AddWorker: "tears down its old connection and poller, starts
    # fresh"), so the miss counter is zero however long the sidecar stop and
    # the responder start took. Without it, a slow container start could spend
    # the whole 10s budget on ordinary refusals and this step would report a
    # down it had not caused. The reconcile that precedes the poller runs
    # against $WB's dockerd, which is healthy, so it costs one fast pass.
    cmgrd-cli worker-add "$WB" "${PUBLIC[$WB]}"
    t=$(date +%s)
    wedged_by=""
    seen=""
    # One reader per sample rather than a bare retry, because "never ok" is
    # half the assertion: a fresh conn starts overloaded and only a poll that
    # COMPLETED can move it to ok, so an ok here means the agent answered
    # something and the step is not exercising a hung poll at all. 25s is well
    # past the 10s budget (CMGR_WORKER_MAX_MISSES=20 at the 500ms default) and
    # well short of the 20s a doubled budget would take.
    wedged_deadline=$(( t + 25 ))
    while (( $(date +%s) < wedged_deadline )); do
      seen=$(worker_health "$WB")
      [[ "$seen" != ok ]] ||
        fail "worker $WB read ok while its agent accepts every poll and answers none: a poll completed, so either the responder is not wedged or a verdict was stored for a request that never returned (pollTelemetry, cmgr/workers.go)"
      if [[ "$seen" == down ]]; then wedged_by=$(( $(date +%s) - t )); break; fi
      sleep 1
    done
    [[ -n "$wedged_by" ]] ||
      fail "worker $WB was still '$seen' 25s after the worker-add that restarted its poller: with the per-poll timeout gone the first poll blocks forever, misses never increment, and a box whose agent has wedged keeps taking placements (httpClient Timeout: timing.pollTimeout, cmgr/workers.go)"
    # The same control again now that the verdict is in: the agent was still
    # accepting and hanging at the end, so the misses that downed $WB were
    # polls the client timeout ended, not connections the agent refused.
    telemetry_hangs "$WB" ||
      fail "the wedged agent on $WB stopped accepting connections before the worker went down: the misses that downed it may have been ordinary refusals, which the telemetry-silence step already covers"
    # Its instances keep running, exactly as under silence: down is a placement
    # and control-plane verdict, not a teardown.
    assert_on_worker "$WB" "${INST_META[$id]}"
    note "worker $WB was down ${wedged_by}s after its poller was restarted: 20 misses at the 500ms cadence, each poll ended by the 250ms poll timeout"
    unfake_overload "$fake"
    retry 30 "telemetry on $WB" quiet curl -sSf --max-time 3 "http://$WB:2136/health"
    readd "$WB"
    DOWNED_WORKER=""
    ok "an agent that accepted every poll and answered none still cost $WB its place in the fleet after ${wedged_by}s, never once reading ok; instance $id kept running on it, and worker-add brought it back once the real sidecar was restored"
  else
    skip "wedged telemetry needs the outer docker socket"
  fi
else
  deselect "wedged telemetry: an agent that accepts and never answers"
fi

# ------------------------------------------------------ 12. overloaded

step "overloaded: a worker reporting overloaded is skipped for placement but not down, and recovers by itself"
if (( OUTER )); then
  ensure_instance_on "$WB" 17
  fake_overload "$WB"
  fake=$LAST_FAKE
  retry 30 "worker $WB to report overloaded" health_is "$WB" overloaded
  for i in 13 14; do
    inst=$(launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
    track "$inst" "$i"
    [[ "$(jq -r .worker <<<"$inst")" == "$WA" ]] || fail "instance $(jq -r .id <<<"$inst") was placed on the overloaded worker"
  done
  id=$(instance_on "$WB")
  cmgrd-cli stop "$id"
  assert_gone "$id" "$WB" "${INST_META[$id]}"
  forget "$id"
  note "launches avoided $WB; stopping instance $id on it still tore it down over docker"
  unfake_overload "$fake"
  retry 30 "worker $WB to recover to ok on its own" health_is "$WB" ok
  ok "overloaded is not sticky: $WB is ok again without a worker-add"
else
  skip "needs the outer docker socket"
fi

# ------------------------------------- 13. every worker unavailable

step "every worker unavailable: all overloaded is a 503 with Retry-After, all down is a 500"
if (( OUTER )); then
  fake_overload "$WA"
  fake_a=$LAST_FAKE
  fake_overload "$WB"
  fake_b=$LAST_FAKE
  both_overloaded() { health_is "$WA" overloaded && health_is "$WB" overloaded; }
  retry 30 "both workers to report overloaded" both_overloaded
  [[ "$(api_status POST "/builds/$OD_BUILD")" == 503 ]] || fail "launch with every worker overloaded was not a 503"
  headers=$(api_headers POST "/builds/$OD_BUILD")
  grep -qi '^Retry-After: 1' <<<"$headers" || fail "503 carried no Retry-After: 1"
  unfake_overload "$fake_a"
  unfake_overload "$fake_b"
  retry 30 "both workers to recover" all_workers_ok
  ok "all overloaded: 503 + Retry-After, then both workers recovered by themselves"
else
  skip "all-overloaded needs the outer docker socket"
fi
down_worker "$WA"
down_worker "$WB"
[[ "$(api_status POST "/builds/$OD_BUILD")" == 500 ]] || fail "launch with every worker down was not a 500"
readd "$WA"
readd "$WB"
ok "all down: 500; worker-add restored both"

# ------------------------------------------ 14. hung docker daemon

step "hung dockerd: a stop that hangs marks the worker down and still clears the records; a launch placed there fails fast as retryable"
if (( OUTER )); then
  ensure_instance_on "$WB" 18
  id=$(instance_on "$WB")
  wb=$(compose_container "${PUBLIC[$WB]}")
  [[ -n "$wb" ]] || fail "compose container for ${PUBLIC[$WB]} not found"
  PAUSED_WORKER=$wb # the EXIT trap unpauses if anything below fails
  outer_ctl "$wb" pause
  note "paused ${PUBLIC[$WB]}'s dockerd (telemetry keeps answering); stopping instance $id, expect one 10s control timeout"
  t=$(date +%s)
  cmgrd-cli stop "$id" >/dev/null 2>&1 || fail "the stop of instance $id did not succeed once its hung worker was declared down"
  took=$(( $(date +%s) - t ))
  note "stop returned success after ${took}s"
  # One timeout, not two: teardown returns a transport failure from
  # stopContainers straight away rather than spending a second
  # CMGR_WORKER_CONTROL_TIMEOUT on a network removal against the same
  # unreachable daemon. 25s passed either way; a single-timeout stop is ~10s.
  (( took < 15 )) || fail "the stop spent more than one control timeout (${took}s): teardown attempted the network removal against the unreachable daemon"
  health_is "$WB" down || fail "worker $WB was not marked down after the hung call"
  [[ "$(api_status GET "/instances/$id")" == 404 ]] || fail "instance $id still known after the stop"
  outer_ctl "$wb" unpause
  PAUSED_WORKER=""
  retry 30 "dockerd on $WB" quiet worker_api "$WB" /_ping
  readd "$WB"
  retry 30 "worker-add to remove the leftovers of instance $id from $WB" orphan_gone "$WB" "$id" "${INST_META[$id]}"
  forget "$id"
  ok "hung stop -> $WB down (sticky) and the records cleared in the same call; worker-add after unpause recovered it and removed the leftovers"

  # The launch side: pause it again while it is ok, so round robin places a
  # launch there. That launch must fail within one control timeout, as a
  # 503 the platform retries, and the other must land on the healthy worker.
  outer_ctl "$wb" pause
  PAUSED_WORKER=$wb
  refused=0
  # A launch refused after placement has already inserted its row, so the
  # rollback is the interesting half: nothing may be left recorded on the
  # worker that refused it.
  wb_before=$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip==$ip) | .instances')
  for i in 19 20; do
    t=$(date +%s)
    out=$(try_launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
    took=$(( $(date +%s) - t ))
    code=${out%%$'\n'*}
    body=${out#*$'\n'}
    case "$code" in
      200|201)
        track "$body" "$i"
        [[ "$(jq -r .worker <<<"$body")" == "$WA" ]] || fail "launch $i succeeded on the paused worker $WB"
        ;;
      503)
        refused=$((refused + 1))
        (( took < 25 )) || fail "the launch on the hung worker took ${took}s to fail; expected one control timeout"
        health_is "$WB" down || fail "worker $WB is not down after the failed launch"
        note "launch $i placed on $WB failed fast: 503 after ${took}s ($(head -c 120 <<<"$body"))"
        ;;
      *) fail "launch $i returned HTTP $code: $(head -c 200 <<<"$body")" ;;
    esac
  done
  (( refused == 1 )) || fail "expected exactly one launch to be refused by the paused worker, got $refused"
  wb_after=$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip==$ip) | .instances')
  [[ "$wb_after" == "$wb_before" ]] ||
    fail "the refused launch left an instance record on $WB ($wb_before -> $wb_after)"
  outer_ctl "$wb" unpause
  PAUSED_WORKER=""
  retry 30 "dockerd on $WB" quiet worker_api "$WB" /_ping
  readd "$WB"
  ok "a launch placed on the hung worker failed fast as retryable and took the worker down; the other landed on $WA; worker-add recovered $WB"
else
  skip "needs the outer docker socket"
fi

# ---------------- 14c. the schema's own hand on its persistent instance

if (( FULL )); then
  step "persistent instances answer to the schema alone: the platform may neither stop nor start one, and update-schema relaunches one the schema is missing"
  # The persistent build carries instance_count 1, which is neither
  # DYNAMIC_INSTANCES (-1) nor LOCKED (-2), so Start and Stop both refuse it as
  # a "locked build" (cmgr/api.go:296 and :411): the only hand that moves this
  # instance is the schema converge. Neither refusal is exercised anywhere in
  # the run, and neither is the converge's instance arithmetic on an EXISTING
  # schema -- add-schema created this instance on an empty database
  # (convergeSchema -> newInstance under m.restartLimits(), cmgr/api.go:605)
  # and the rebuild steps restart it in place through restartInstance, a
  # different caller. This step drives both arms of that arithmetic on the
  # schema the run is already using: a converge that wants no instance of the
  # persistent challenge, then one that wants it back. The second is the
  # documented recovery after a worker-remove takes a persistent instance's
  # record with the box ("a persistent instance it hosted is only relaunched
  # by the next update-schema", cmd/cmgrd-cli/main.go), and nothing else in the
  # scenario reaches it.
  #
  # No outer socket: every call here is cmgrd's own API, so this step runs in
  # full mode whether or not the chaos steps do.
  all_workers_ok ||
    fail "both workers must be ok before the persistent instance is stopped and relaunched: this step pins the relaunch to $WA by downing $WB, and a converge that finds no eligible worker fails the update instead"
  pmeta=$(api GET "/instances/$PERSIST_INST")
  pworker=$(jq -r .worker <<<"$pmeta")
  [[ "$pworker" == "$WA" ]] ||
    fail "the persistent instance $PERSIST_INST is on $pworker, not on $WA: the scenario pins it away from the chaos target (scenario.sh:~627) and this step relaunches it on the worker it is meant to live on"
  wa_rows=$(worker_instances "$WA")
  [[ "$wa_rows" =~ ^[0-9]+$ ]] || fail "could not read the instance count of worker $WA"
  # The instances the persistent build records, straight from the schema state
  # (the read the converge step already uses at scenario.sh:605). One line per
  # instance, so "exactly one" is a regex over the whole output.
  persist_instances() {
    api GET "/schemas/$SCHEMA_NAME" |
      jq -r --arg c "$CH_PERSISTENT" '.[] | select(.id == $c) | .builds[].instances[]?.id'
  }
  lock_tmp=$(mktemp -d)

  # (1) The platform's stop is refused, and the instance is untouched by the
  # attempt. This is what keeps a TTL sweep, an operator's DELETE or a
  # mis-aimed platform call from taking a service's only instance away with
  # nothing in the schema saying so. The 500 is deliberate -- nothing here is
  # retryable -- and the message is what tells it apart from any other 500.
  code=$(curl -sS -o "$lock_tmp/stop" -w '%{http_code}' --connect-timeout 5 --max-time "$API_TIMEOUT" \
    -X DELETE "$CMGRD_SERVER/instances/$PERSIST_INST")
  [[ "$code" == 500 ]] ||
    fail "DELETE /instances/$PERSIST_INST answered HTTP $code: an instance of a build the schema sizes (instance_count 1) is not the platform's to stop, and Stop must refuse it (cmgr/api.go:411)"
  grep -q "locked build" "$lock_tmp/stop" ||
    fail "the refused stop of the persistent instance says '$(head -c 160 "$lock_tmp/stop")' rather than naming the locked build (cmgr/api.go:411): a 500 for some other reason would pass this step for the wrong reason"
  [[ "$(api_status GET "/instances/$PERSIST_INST")" == 200 ]] ||
    fail "the persistent instance $PERSIST_INST is gone from cmgrd after a stop that was refused"
  assert_on_worker "$WA" "$pmeta"

  # (2) And the platform may not start another one either: the same guard on
  # the way in (cmgr/api.go:296), which is what makes a schema's instance count
  # mean something.
  code=$(curl -sS -o "$lock_tmp/start" -w '%{http_code}' --connect-timeout 5 --max-time "$API_TIMEOUT" \
    -X POST -H 'Content-Type: application/json' "$CMGRD_SERVER/builds/$PERSIST_BUILD")
  [[ "$code" == 500 ]] ||
    fail "POST /builds/$PERSIST_BUILD answered HTTP $code: only a schema change may add an instance to a build whose count the schema fixes (cmgr/api.go:296)"
  grep -q "locked build" "$lock_tmp/start" ||
    fail "the refused launch of the persistent build says '$(head -c 160 "$lock_tmp/start")' rather than naming the locked build (cmgr/api.go:296)"
  [[ "$(persist_instances)" == "$PERSIST_INST" ]] ||
    fail "$CH_PERSISTENT records instance(s) '$(persist_instances | tr '\n' ' ')' after a refused launch, expected only $PERSIST_INST: the refusal still created a row"
  note "the platform's stop and start of the persistent instance were both refused as a locked build; instance $PERSIST_INST is untouched"

  # (3) The converge's teardown arm. The schema copy differs from the file the
  # run converged with in exactly one number: writing a schema by hand risks a
  # different flag_format, and a flag-format change rebuilds every challenge in
  # the schema (UpdateSchema's doc comment) -- minutes, and a new flag under
  # every assertion that follows. instance_count 1 appears once in the file
  # (the other three challenges are -1), and the diff below is the guard.
  conv=$(mktemp -d)
  sed 's/^\([[:space:]]*\)instance_count: 1$/\1instance_count: 0/' "$E2E_SCHEMA" >"$conv/zero.yaml"
  # Counted line by line rather than parsed out of diff's output: this image's
  # diff is busybox, which prints unified format by default, so a '^[<>]' count
  # is always zero and the guard would wave through a copy that changed nothing
  # -- which is exactly what it did.
  ndiff=$(awk 'NR==FNR {a[FNR]=$0; next} a[FNR] != $0 {n++} END {print n+0}' "$E2E_SCHEMA" "$conv/zero.yaml")
  [[ "$ndiff" == 1 ]] ||
    fail "the zero-instance copy of $E2E_SCHEMA differs from it on $ndiff line(s), expected exactly the one instance_count: converging a schema that differs in anything else would change what the rest of the run is testing"
  grep -q '^[[:space:]]*instance_count: 0$' "$conv/zero.yaml" ||
    fail "could not write a zero-instance copy of $E2E_SCHEMA: no 'instance_count: 1' line to rewrite"
  crc=0
  t=$(date +%s)
  cmgrd-cli update-schema "$conv/zero.yaml" >"$conv/out0" 2>"$conv/err0" || crc=$?
  if (( crc != 0 )); then
    sed 's/^/       /' "$conv/out0" "$conv/err0" || true
    fail "cmgrd-cli update-schema exited $crc asking for no instance of $CH_PERSISTENT: a converge that wants fewer instances than it finds must stop the extras (cmgr/api.go:590-600)"
  fi
  note "the converge that wants no persistent instance returned in $(( $(date +%s) - t ))s"
  # Stopped for real, not merely forgotten: stopInstance ran the docker
  # teardown on $WA, so the containers and the cmgr-<id> network are gone.
  assert_gone "$PERSIST_INST" "$WA" "$pmeta"
  [[ -z "$(persist_instances)" ]] ||
    fail "$CH_PERSISTENT still records instance(s) '$(persist_instances | tr '\n' ' ')' after a converge that asked for none"
  # The build itself survives: only a build the schema no longer names is
  # released (removedSchemaBuilds selects the rows still marked LOCKED,
  # cmgr/database_builds.go:325). Were it destroyed here its images would leave
  # the registry and the builder, and the teardown step's tag assertions would
  # be checking nothing.
  [[ "$(api_status GET "/builds/$PERSIST_BUILD")" == 200 ]] ||
    fail "build $PERSIST_BUILD was destroyed by a converge that merely asked for no instance of it: only a build the schema stops naming is released (cmgr/api.go:511-517, cmgr/database_builds.go:325)"
  # The other challenges are instance_count -1 (DYNAMIC_INSTANCES), which the
  # converge skips outright (cmgr/api.go:577). A regression that read -1 as a
  # target would have torn down every tracked on-demand instance here, and the
  # steps after this one would fail somewhere far from the cause.
  for id in "${OD_IDS[@]}"; do
    [[ "$(api_status GET "/instances/$id")" == 200 ]] ||
      fail "on-demand instance $id was torn down by a schema converge: a build the schema leaves dynamic (instance_count -1) keeps the instances the platform launched (cmgr/api.go:577)"
  done

  # (4) The relaunch arm. One worker up, so the relaunch is placed back on the
  # box the instance lived on -- which the worker-remove and teardown steps
  # take as given -- rather than on the chaos target.
  DOWNED_WORKER=$WB # armed before the call, so the trap owes a worker-add even if down_worker's own check fails
  down_worker "$WB"
  retry 20 "worker $WA to report ok before the relaunch converge" health_is "$WA" ok
  crc=0
  t=$(date +%s)
  cmgrd-cli update-schema "$E2E_SCHEMA" >"$conv/out1" 2>"$conv/err1" || crc=$?
  # A converge with no eligible worker fails the update, and that is correct
  # behaviour rather than the regression under test. It can only happen here if
  # $WA drifted out of placement between the check above and the launch (its
  # telemetry agent reporting overloaded, which clears by itself), so that one
  # case gets one more converge -- idempotent, and exactly what the CLI
  # documents as the recovery -- and says so in the log.
  if (( crc != 0 )) && ! health_is "$WA" ok; then
    note "the converge failed while $WA read '$(worker_health "$WA")': no worker was eligible for the relaunch, which is a loaded fleet rather than a cork failure; converging once more"
    retry 30 "worker $WA to report ok again" health_is "$WA" ok
    crc=0
    cmgrd-cli update-schema "$E2E_SCHEMA" >"$conv/out1" 2>"$conv/err1" || crc=$?
  fi
  if (( crc != 0 )); then
    sed 's/^/       /' "$conv/out1" "$conv/err1" || true
    fail "cmgrd-cli update-schema exited $crc with one instance of $CH_PERSISTENT to relaunch: a converge that finds a persistent instance missing must launch it again, under the restart limits (cmgr/api.go:598-609, m.restartLimits())"
  fi
  ctook=$(( $(date +%s) - t ))
  new_inst=$(persist_instances)
  [[ "$new_inst" =~ ^[0-9]+$ ]] ||
    fail "$CH_PERSISTENT records instance(s) '$(tr '\n' ' ' <<<"$new_inst")' after update-schema exited 0, expected exactly one: the converge reported success without converging (cmgr/api.go:598-609)"
  [[ "$new_inst" != "$PERSIST_INST" ]] ||
    fail "the relaunched persistent instance kept the id $PERSIST_INST of the one that was stopped: instance ids are never reused (AUTOINCREMENT, 452d96f)"
  nmeta=$(api GET "/instances/$new_inst")
  nworker=$(jq -r .worker <<<"$nmeta")
  [[ "$nworker" == "$WA" ]] ||
    fail "the relaunched persistent instance $new_inst is on $nworker, not on $WA, the only worker up when the converge ran: a converge's launch goes through the same placement as any other (selectWorker in newInstance, cmgr/api.go:328)"
  npub=$(jq -r .worker_public <<<"$nmeta")
  [[ "$npub" == "${PUBLIC[$WA]}" ]] ||
    fail "the relaunched persistent instance $new_inst advertises '$npub', expected '${PUBLIC[$WA]}': players are handed the worker's public address, not its orchestration ip"
  nport=$(jq -r '.ports.socat // 0' <<<"$nmeta")
  (( nport >= PORT_LOW && nport <= PORT_HIGH )) ||
    fail "the relaunched persistent instance $new_inst took port $nport, outside CMGR_PORTS $PORT_LOW-$PORT_HIGH: the port the stopped instance held was never released, or the reservation ran outside the range"
  assert_on_worker "$WA" "$nmeta"
  # It really serves, and serves the generation the run last built for it: the
  # persistent source has carried the generation-2 marker since the
  # generation-2 step (set_generation 3 and 4 pass "ondemand" and leave
  # $PERSISTENT_SRC alone), so a relaunch off a stale image fails here.
  retry 30 "the relaunched persistent instance at $npub:$nport" tcp_says "$npub" "$nport" '' 'e2e-generation-2'
  # A launch the converge issues is a launch like any other as far as the box
  # is concerned: nothing about it may take the worker out of the fleet, and
  # down is sticky, so this single read is the whole assertion.
  if health_is "$WA" down; then
    fail "worker $WA is down after the converge relaunched the persistent instance on it: a relaunch that succeeded and serves must leave its worker in placement"
  fi
  (( $(worker_instances "$WA") == wa_rows )) ||
    fail "worker $WA records $(worker_instances "$WA") instance(s) after the stop and relaunch, expected the $wa_rows it started with: the stopped instance's record outlived it, or the relaunch left a second one"

  # The scenario's handle on the persistent instance moves with it: the
  # converge creates a new row rather than restarting the old one, and the
  # teardown step (scenario.sh:2166-2171) is the only later reader of these
  # three. IMAGE_TAG, PERSIST_TAG_G1 and PERSIST_BUILD are untouched -- a
  # converge neither rebuilds nor re-pushes an existing build (cmgr/docker.go:203-218).
  PERSIST_INST=$new_inst
  PERSIST_META=$nmeta
  PERSIST_WORKER=$WA
  readd "$WB"
  DOWNED_WORKER=""
  rm -rf "$conv" "$lock_tmp"
  ok "the platform's stop and start of the persistent instance were both refused as a locked build; a converge asking for none stopped it on $WA (containers and network gone, the build kept, the on-demand instances untouched) and the next update-schema relaunched it in ${ctook}s as instance $PERSIST_INST on ${PUBLIC[$WA]}:$nport serving generation 2, with $WA still taking placements"
else
  deselect "the converge's stop and relaunch of the persistent instance"
fi

# ------------------------------------------------- 14b. a refused daemon

# --------------------------- 14b. a worker whose dockerd refuses connections

step "refused dockerd: a worker whose daemon is not listening never takes a placement, however healthy its telemetry says it is"
# Every docker-side injection above pauses a daemon, and cmgrd catches a pause
# through the context.DeadlineExceeded branch of isTransportError
# (cmgr/workers.go). A refused connection -- the box answers, dockerd is not
# listening: a daemon that died, or never came back after a reboot -- takes the
# other branch, client.IsErrConnectionFailed, and nothing here covers it. This
# container is that box: it sits on the workers' network, nothing listens on
# its 2376, and it can be made to serve telemetry on 2136.
phantom_ip=$(hostname -i 2>/dev/null | tr ' ' '\n' | grep -m1 "^${WA%.*}\." || true)
if [[ -z "$phantom_ip" ]]; then
  phantom_ip=$(getent ahostsv4 "$(hostname)" 2>/dev/null | awk 'NR == 1 {print $1}' || true)
fi
# It has to be an address on the workers' own /24: cmgrd must dial it exactly
# as it dials a real box, and a loopback or off-network address would make the
# step pass for the wrong reason.
[[ "$phantom_ip" == "${WA%.*}."* && "$phantom_ip" != "$WA" && "$phantom_ip" != "$WB" ]] ||
  fail "could not find this container's own address on the workers' network (got '$phantom_ip'): a worker registered there would not be dialled the way cmgrd dials a real worker"
# And nothing may listen on its 2376, or the dial would not be refused.
if quiet nc -z -w 1 "$phantom_ip" 2376; then
  fail "something is listening on $phantom_ip:2376; a worker registered there would not refuse cmgrd's docker connection"
fi
# A phantom row outlives a run that died between the worker-add and the
# worker-remove below, in the cork-data volume, and cmgrd reloads it at
# startup. Clear it rather than miscount workers at the end of this step.
for ip in $(api GET /workers | jq -r --arg a "$WA" --arg b "$WB" '.[] | select(.ip != $a and .ip != $b) | .ip'); do
  note "removing worker $ip, left behind by an earlier run"
  cmgrd-cli worker-remove "$ip"
done

# Live, healthy telemetry is the trap, not a convenience. Were the refusal
# taken for an API error instead of a transport one, reconcileWorker would
# report the pass incomplete rather than unreachable, reconcileWithRetries
# would fall through to "it takes placements, loudly" instead of marking the
# worker down, pollWorker would start, and this responder would drive the
# worker straight to ok. Same shape as fake_overload, run here rather than in
# a sidecar's network namespace.
phantom_body='{"overloaded":false}'
phantom_resp=$(printf 'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
  "${#phantom_body}" "$phantom_body")
PHANTOM_IP=$phantom_ip # from here the EXIT trap removes the worker and drains the responder
( while :; do printf '%s' "$phantom_resp" | nc -l -q 0 2136 >/dev/null 2>&1 || true; done ) &
PHANTOM_RESPONDER=$!
retry 10 "the phantom's telemetry on $phantom_ip:2136" quiet curl -sSf --max-time 3 "http://$phantom_ip:2136/health"
note "phantom worker $phantom_ip: telemetry answers healthy, nothing listens on its 2376"

# One reader for every sample, because "ok at any instant" is the sharp
# assertion: ok means the refusal was not classified as a transport error, the
# worker was handed to the poller, and the fake telemetry polled it healthy --
# a worker in that state takes launches its dockerd cannot serve. Nothing can
# take it back out of ok once there (the poller keeps reading this responder),
# so sampling at the step's own pace cannot miss it.
PHANTOM_SEEN=""
phantom_sample() {
  PHANTOM_SEEN=$(worker_health "$PHANTOM_IP" || true)
  [[ "$PHANTOM_SEEN" != ok ]] ||
    fail "phantom worker $PHANTOM_IP reports ok although its dockerd refuses every connection: the refusal was not treated as a transport error (client.IsErrConnectionFailed in isTransportError, cmgr/workers.go), so its reconcile did not report it unreachable and it was handed to the poller"
}

t=$(date +%s)
cmgrd-cli worker-add "$phantom_ip" phantom
phantom_sample
[[ -n "$PHANTOM_SEEN" ]] || fail "worker $phantom_ip is not listed right after worker-add"
note "the phantom reads '$PHANTOM_SEEN' the moment it is added"

# Free coverage inside the 10s the reconcile budget costs anyway: a worker
# whose first pass has not finished sits at overloaded, because newWorkerConn
# fails closed for placement until the first successful poll, so a launch now
# must land on a real worker. Step 12's overloaded worker got there through
# telemetry; this window is the only place that initial state is exercised.
out=$(try_launch "$OD_BUILD" e2e-user-23 e2e-value-23)
code=${out%%$'\n'*}
body=${out#*$'\n'}
case "$code" in
  200|201) ;;
  *) fail "a launch with the unreachable phantom worker registered returned HTTP $code: $(head -c 200 <<<"$body")" ;;
esac
id=$(jq -r .id <<<"$body")
w=$(jq -r .worker <<<"$body")
[[ "$w" == "$WA" || "$w" == "$WB" ]] ||
  fail "instance $id was placed on $w, the phantom worker whose dockerd refuses connections: a worker whose first reconcile has not finished must not be eligible for placement"
track "$body" 23
note "a launch $(( $(date +%s) - t ))s after worker-add landed on $w, not on the phantom"
cmgrd-cli stop "$id"
assert_gone "$id" "${INST_WORKER[$id]}" "${INST_META[$id]}"
forget "$id"
phantom_sample

# The reconcile retries the refusal for maxMisses * pollInterval (10s here:
# CMGR_WORKER_MAX_MISSES=20 at the 500ms default poll interval) and then marks
# the worker down. 18s is well past that and well short of the 20s a doubled
# budget would take, so this bounds the budget without racing it -- and
# without depending on how long the launch above took, since the deadline runs
# from the worker-add.
phantom_down_by=""
phantom_deadline=$(( t + 18 ))
while (( $(date +%s) < phantom_deadline )); do
  phantom_sample
  if [[ "$PHANTOM_SEEN" == down ]]; then phantom_down_by=$(( $(date +%s) - t )); break; fi
  sleep 1
done
[[ -n "$phantom_down_by" ]] ||
  fail "phantom worker $phantom_ip was still '$PHANTOM_SEEN' 18s after worker-add: a daemon that stays unreachable must end at down once its reconcile budget is spent (reconcileUnreachable in reconcileWithRetries, cmgr/workers.go), not sit out of placement forever"
note "the phantom was down within ${phantom_down_by}s of worker-add, never once reading ok"

cmgrd-cli worker-remove "$phantom_ip"
kill "$PHANTOM_RESPONDER" >/dev/null 2>&1 || true
# The nc blocked in accept() outlives the loop that respawns it; one
# connection makes it serve its canned response and exit. A stray one would be
# harmless (this script is the container's PID 1), but the port is left free.
curl -s --max-time 2 "http://$phantom_ip:2136/" >/dev/null 2>&1 || true
PHANTOM_RESPONDER=""
PHANTOM_IP=""
wlist=$(api GET /workers)
[[ -z "$(jq -r --arg ip "$phantom_ip" '.[] | select(.ip == $ip) | .ip' <<<"$wlist")" ]] ||
  fail "phantom worker $phantom_ip is still listed after worker-remove"
[[ "$(jq -r length <<<"$wlist")" == "$NWORKERS" ]] ||
  fail "cmgrd holds $(jq -r length <<<"$wlist") worker(s) after the phantom was removed, expected $NWORKERS"
all_workers_ok || fail "worker $WA or $WB is not ok after the phantom was registered and removed"
ok "worker $phantom_ip, telemetry healthy and dockerd refusing connections, never read ok, took no placement while it reconciled, went down within ${phantom_down_by}s, and left $WA/$WB serving launches"

########################################################################
# BLOCK 2 of 4 — after the "refused dockerd" step (see insertAfter)
########################################################################

# ------------------------- 14c. a worker-add onto a wedged (frozen) daemon

if (( FULL )); then
  if (( OUTER )); then
    step "worker-add on a box whose dockerd is wedged: the worker stays out of placement while its reconcile retries, and is marked down when the budget is spent"
    # The refused-dockerd step above covers one branch of isTransportError
    # (client.IsErrConnectionFailed): nothing listening, every reconcile
    # attempt failing in milliseconds, the 10s budget spent over ~20 of them.
    # This is the other branch (context.DeadlineExceeded), and it is the one
    # production actually sees -- an OOMing or IO-starved box whose dockerd
    # accepts the connection and never answers. It differs in more than the
    # error: one ContainerList against a frozen daemon burns the whole
    # CMGR_WORKER_CONTROL_TIMEOUT (10s), which is also the whole reconcile
    # budget (CMGR_WORKER_MAX_MISSES 20 x the 500ms poll interval), so one or
    # two attempts spend it and the down verdict must still arrive. A
    # reconcileWithRetries that counted attempts instead of holding a deadline
    # (cmgr/workers.go:328-356) would hold a wedged box out of the fleet for
    # 20 x 10s and mark it down only then. It is also the only worker-add in
    # this scenario that lands on a worker cork already has, taking
    # AddWorker's replace branch (workers.go:589-595) rather than adding a row.
    wb=$(compose_container "${PUBLIC[$WB]}")
    [[ -n "$wb" ]] || fail "compose container for ${PUBLIC[$WB]} not found"
    # Planted while the daemon still answers: this is what the worker-add at
    # the end of the step has to clear, and it is what distinguishes a box
    # that was really reconciled on its way back from one merely polled to ok.
    planted=$(plant_orphan "$WB" 996)
    ORPHAN_WORKER=$WB; ORPHAN_NET=cmgr-996; ORPHAN_CID=$planted
    note "planted an orphan (cmgr.managed container on cmgr-996, no record) on $WB while its daemon still answered"
    DOWNED_WORKER=$WB # armed before the pause: the trap owes a worker-add from here on
    PAUSED_WORKER=$wb # and an unpause
    outer_ctl "$wb" pause
    t=$(date +%s)
    cmgrd-cli worker-add "$WB" "${PUBLIC[$WB]}"
    add_took=$(( $(date +%s) - t ))
    # (a) The operator's call does not wait for the box. AddWorker builds the
    # client and hands the worker to runWorker's goroutine (workers.go:299,
    # 585-595) without a single docker call of its own, so a worker-add on a
    # daemon that is merely still starting costs the operator nothing.
    (( add_took < 5 )) ||
      fail "cmgrd-cli worker-add on the wedged $WB blocked for ${add_took}s: the reconcile must run in runWorker's goroutine, not in the operator's request (cmgr/workers.go:299)"
    # The negative control for everything below, and the reason a paused
    # daemon is the sharp injection: the telemetry sidecar is a separate
    # container sharing the worker's network namespace (compose.yaml
    # network_mode: service:worker-b), so the pause did not touch it and it
    # keeps answering "not overloaded" every 500ms. If the rebuilt conn were
    # handed to the poller before its reconcile finished, this responder alone
    # would drive the worker to ok.
    quiet curl -sSf --max-time 3 "http://$WB:2136/health" ||
      fail "the telemetry sidecar of $WB stopped answering when its dockerd was paused: without it this step cannot tell a poller that never started from one with nothing to read"
    # (b) Out of placement, not merely reported so, and asked while the budget
    # is still running rather than after it. try_launch rather than launch: a
    # launch that did go to the frozen box comes back 503 after a control
    # timeout, and that is this step's failure to report rather than curl's.
    out=$(try_launch "$OD_BUILD" e2e-user-26 e2e-value-26)
    code=${out%%$'\n'*}
    body=${out#*$'\n'}
    case "$code" in
      200|201) ;;
      *) fail "a launch $(( $(date +%s) - t ))s into $WB's reconcile of its frozen daemon answered HTTP $code with $WA reading '$(worker_health "$WA")': $(head -c 200 <<<"$body")" ;;
    esac
    [[ "$(jq -r .worker <<<"$body")" == "$WA" ]] ||
      fail "instance $(jq -r .id <<<"$body") was placed on $WB, whose reconcile has not finished against a frozen dockerd: selectWorker takes only workerOk (cmgr/workers.go:712-715), and newWorkerConn fails a fresh conn closed as overloaded until its pass is done"
    track "$body" 26
    id=$(jq -r .id <<<"$body")
    cmgrd-cli stop "$id"
    assert_gone "$id" "$WA" "${INST_META[$id]}"
    forget "$id"
    # (c) And never ok while the budget runs. One-sided by construction --
    # newWorkerConn stores overloaded (workers.go:297) and only
    # reconcileWithRetries returning true starts the poller, so with the
    # ordering intact every sample here is "overloaded" and this cannot flake
    # false. It samples to 9s, inside the 10s budget, so a legitimate early
    # down verdict cannot fire it either (down is not ok).
    seen=""
    while (( $(date +%s) - t < 9 )); do
      seen=$(worker_health "$WB" || true)
      [[ -n "$seen" ]] || fail "worker $WB is not listed at all $(( $(date +%s) - t ))s after its worker-add"
      [[ "$seen" != ok ]] ||
        fail "worker $WB read ok $(( $(date +%s) - t ))s into a worker-add its dockerd cannot answer: the poller was started before the reconcile pass finished (runWorker), so a box that can serve no launch is back in placement"
      sleep 0.5
    done
    note "$WB read '$seen' throughout the 9s after the worker-add, with its telemetry answering healthy"
    # (d) The verdict at the end of the budget, sampled against a deadline
    # measured from the worker-add rather than from here, as the phantom step
    # does. 30s and not 12: the control timeout and the budget are both 10s,
    # so whether the first attempt's deadline lands just inside or just
    # outside the budget decides between one attempt (~10s) and two (~21s),
    # and neither is a regression. Both are far short of the 200s an
    # attempt-counting loop would take, which is what this bounds.
    down_took=""
    down_deadline=$(( t + 30 ))
    while (( $(date +%s) < down_deadline )); do
      seen=$(worker_health "$WB" || true)
      [[ "$seen" != ok ]] ||
        fail "worker $WB read ok while its dockerd was still frozen: its reconcile cannot have finished (runWorker)"
      if [[ "$seen" == down ]]; then down_took=$(( $(date +%s) - t )); break; fi
      sleep 1
    done
    [[ -n "$down_took" ]] ||
      fail "worker $WB was still '$seen' 30s after a worker-add onto its frozen dockerd: a daemon that stays unreachable must end at down once the reconcile budget is spent (reconcileWithRetries, cmgr/workers.go:343-345), not sit out of placement forever"
    # (e) And it is terminal: reconcileWithRetries returned false, so
    # pollWorker never ran and nothing reads this box's telemetry any more. A
    # daemon that comes back does not bring the worker back with it.
    outer_ctl "$wb" unpause
    PAUSED_WORKER=""
    retry 30 "dockerd on $WB" quiet worker_api "$WB" /_ping
    sleep 3 # six poll intervals, as in the telemetry-silence step
    health_is "$WB" down ||
      fail "worker $WB recovered to '$(worker_health "$WB")' by itself once its dockerd was unpaused: a worker its own reconcile gave up on must stay down until a worker-add rebuilds the conn"
    readd "$WB"
    DOWNED_WORKER=""
    # readd returns at ok, and reconcileWorker runs before the poller
    # (runWorker), so the orphan must already be gone: asserted once, never
    # retried -- a retry here could only hide a poller started too early.
    orphan_gone "$WB" 996 "{\"containers\":[\"$planted\"]}" ||
      fail "worker $WB reported ok after its worker-add with the orphan cmgr-996 still on it: the reconcile did not finish before the poller started"
    ORPHAN_WORKER=""; ORPHAN_NET=""; ORPHAN_CID=""
    all_workers_ok || fail "worker $WA or $WB is not ok after $WB was wedged and re-added"
    ok "a worker-add onto a frozen dockerd answered in ${add_took}s, never read ok while its reconcile retried (telemetry answering throughout), took no placement, went down after ${down_took}s, stayed down when its daemon came back, and cleared cmgr-996 on the worker-add that recovered it"
  else
    skip "worker-add against a wedged dockerd needs the outer docker socket"
  fi
else
  deselect "worker-add against a wedged dockerd"
fi

########################################################################
# BLOCK 3 of 4 — immediately after BLOCK 2
########################################################################

# --------------------- 14d. a cmgrd restart with one worker's daemon wedged

if (( FULL )); then
  if (( OUTER )); then
    step "cmgrd restart with a wedged worker: startup is not blocked, the healthy worker takes every launch, and the wedged one is downed by its own reconcile"
    # The restart step earlier in this run brings cmgrd back with a healthy
    # fleet. This is the production morning after: one box wedged (dockerd
    # frozen, telemetry still answering) when cork itself is restarted. Three
    # separate promises, none of them covered anywhere else -- startup does
    # not block on the wedged box (initWorkers spawns a goroutine per worker
    # and returns, cmgr/workers.go:202-227 with :299), the healthy box
    # reconciles and takes every placement while the other is still being
    # retried, and the wedged one ends at down through reconcileWithRetries'
    # unreachable verdict (workers.go:343-345) rather than through telemetry,
    # which never stops.
    cork=$(compose_container cork)
    [[ -n "$cork" ]] || fail "cork container not found via the outer docker API"
    wb=$(compose_container "${PUBLIC[$WB]}")
    [[ -n "$wb" ]] || fail "compose container for ${PUBLIC[$WB]} not found"
    # Planted before the pause, for the worker-add at the end to clear.
    planted=$(plant_orphan "$WB" 995)
    ORPHAN_WORKER=$WB; ORPHAN_NET=cmgr-995; ORPHAN_CID=$planted
    DOWNED_WORKER=$WB
    PAUSED_WORKER=$wb
    outer_ctl "$wb" pause
    outer_ctl "$cork" restart
    # The clock starts where the container does, not before it: docker's
    # restart stops cork first (up to its stop timeout), and folding that into
    # the measurement would make a healthy start look like a blocked one.
    t=$(date +%s)
    retry 60 "cmgrd to answer after the restart" quiet api GET /version
    up_took=$(( $(date +%s) - t ))
    # (a) The ordering assertion, deliberately not a stopwatch. initWorkers
    # runs before ListenAndServe, so the reconcile budget and the listener
    # start together: a startup that ran $WB's pass inline would have spent
    # the whole 10s budget before answering, and the first thing this read
    # would see is a worker already down. Anything else means cmgrd was
    # serving while that pass was still running. The duration below is only
    # the guard that keeps that reading meaningful.
    restart_health=$(worker_health "$WB" || true)
    [[ -n "$restart_health" ]] || fail "worker $WB is not listed after the restart: cmgrd did not reload its worker rows (initWorkers)"
    [[ "$restart_health" != down ]] ||
      fail "cmgrd answered its first request ${up_took}s after its container started with $WB already down: startup ran the wedged worker's whole reconcile budget before it began listening (initWorkers must hand each worker to a goroutine, cmgr/workers.go:299)"
    (( up_took < 20 )) ||
      fail "cmgrd took ${up_took}s from container start to answer, longer than the ~10s a startup blocked behind the wedged $WB would take: the reading above can no longer tell the two apart"
    note "cmgrd answered ${up_took}s after its container started, with $WB reading '$restart_health'"
    # (b) The healthy box is not held up by the wedged one either: its own
    # reconcile is fast and its poller starts on schedule.
    retry 20 "worker $WA to report ok after the restart" health_is "$WA" ok
    if health_is "$WB" ok; then
      fail "worker $WB reads ok after the restart although its dockerd is frozen: either the pause did not take (so this step is testing nothing) or its reconcile pass was skipped and its poller started anyway (runWorker)"
    fi
    # (c) And $WA takes every launch. Two of them: with one eligible worker
    # the round-robin cursor must land on $WA twice (selectWorker skips
    # anything but workerOk), and a cursor that still counted $WB would send
    # the second into a control timeout.
    restart_ids=()
    for i in 27 28; do
      out=$(try_launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
      code=${out%%$'\n'*}
      body=${out#*$'\n'}
      case "$code" in
        200|201) ;;
        *) fail "launch $i after the restart answered HTTP $code with $WA reading '$(worker_health "$WA")' and $WB wedged: $(head -c 200 <<<"$body")" ;;
      esac
      [[ "$(jq -r .worker <<<"$body")" == "$WA" ]] ||
        fail "launch $i was placed on the wedged worker $WB after the restart: a worker whose startup reconcile has not finished must not be eligible"
      track "$body" "$i"
      restart_ids+=("$(jq -r .id <<<"$body")")
    done
    # One full check: the fleet is not merely accepting launches after the
    # restart, it is serving them. No generation argument -- which generation
    # is current depends on how many update steps ran before this one.
    check_ondemand "${INST_META[${restart_ids[0]}]}" 27
    for id in "${restart_ids[@]}"; do
      cmgrd-cli stop "$id"
      assert_gone "$id" "$WA" "${INST_META[$id]}"
      forget "$id"
    done
    # (d) The verdict, and where it came from. The sidecar never stopped
    # answering, so this down cannot be the telemetry-silence path the earlier
    # step covers: it is the startup reconcile giving up on an unreachable
    # daemon. 30s from here, not from the restart, because the launches above
    # already spent part of the budget -- what is bounded is that the verdict
    # arrives at all, against the 200s an attempt-counting loop would need.
    retry 30 "worker $WB to be marked down by its startup reconcile" health_is "$WB" down
    quiet curl -sSf --max-time 3 "http://$WB:2136/health" ||
      fail "the telemetry of $WB stopped answering during this step: $WB must be marked down here by its reconcile, not by telemetry silence, or the assertion above proves nothing"
    outer_ctl "$wb" unpause
    PAUSED_WORKER=""
    retry 30 "dockerd on $WB" quiet worker_api "$WB" /_ping
    sleep 3
    health_is "$WB" down ||
      fail "worker $WB came back to '$(worker_health "$WB")' on its own once its dockerd was unpaused: a worker downed by its startup reconcile takes a worker-add, or an operator would never know a box had been out"
    readd "$WB"
    DOWNED_WORKER=""
    orphan_gone "$WB" 995 "{\"containers\":[\"$planted\"]}" ||
      fail "worker $WB reported ok after its worker-add with the orphan cmgr-995 still on it: the reconcile did not finish before the poller started"
    ORPHAN_WORKER=""; ORPHAN_NET=""; ORPHAN_CID=""
    all_workers_ok || fail "worker $WA or $WB is not ok after the restart with a wedged worker"
    ok "cmgrd served ${up_took}s after its container restarted with $WB frozen ($WB reading '$restart_health'), $WA reconciled and took both launches, $WB was marked down by its own startup reconcile with its telemetry still answering, stayed down through its daemon's return, and was recovered by a worker-add that cleared cmgr-995"
  else
    skip "a cmgrd restart with a wedged worker needs the outer docker socket"
  fi
else
  deselect "cmgrd restart with a wedged worker"
fi

# ------------------------------------------------- 15. worker-remove

step "worker-remove: purges the worker and its instance records, leaving its containers alone until it is re-added"
ensure_instance_on "$WB" 15
victims=()
for id in "${OD_IDS[@]}"; do
  if [[ "${INST_WORKER[$id]}" == "$WB" ]]; then victims+=("$id"); fi
done
cmgrd-cli worker-remove "$WB"
[[ -z "$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip == $ip) | .ip')" ]] || fail "worker $WB is still listed"
for id in "${victims[@]}"; do
  [[ "$(api_status GET "/instances/$id")" == 404 ]] || fail "instance $id survived the removal of its worker"
  assert_on_worker "$WB" "${INST_META[$id]}"
done
readd "$WB"
[[ "$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip == $ip) | .instances')" == 0 ]] || fail "re-added worker $WB still counts instances"
for id in "${victims[@]}"; do
  orphan_gone "$WB" "$id" "${INST_META[$id]}" ||
    fail "worker $WB reported ok while the leftovers of instance $id were still on it"
  forget "$id"
done
ok "worker $WB purged with ${#victims[@]} instance record(s), containers left running until worker-add removed them, re-added clean"

# --------------------------------------------- 15b. autoscaling lifecycle

step "autoscaling lifecycle: a box joins on scale-out, keeps serving while it drains on scale-in, and is purged before its machine is terminated"
# What the autoscaler does around a lifecycle event, in its order: POST
# /workers for a box that has just come up, PATCH it down when the ASG hands
# it a termination window, watch GET /workers until its instances have
# drained, DELETE it. Nothing on the box is ever cleaned up — termination
# wipes it — so every call here is the platform's (curl), not the operator's
# (cmgrd-cli). $WB stands in for the autoscaled box: purged first so cork has
# never seen it, and re-added at the end because this box is not really going
# away and later steps expect the fleet whole.
for id in "${OD_IDS[@]}"; do
  [[ "${INST_WORKER[$id]}" != "$WB" ]] ||
    fail "instance $id is still tracked on $WB: the purge below deletes its record behind the scenario's bookkeeping — this step belongs where $WB holds nothing tracked"
done
DRAINED_WORKER=$WB # the EXIT trap re-adds $WB if anything below fails
[[ "$(api_status DELETE "/workers/$WB")" == 204 ]] || fail "could not purge $WB to stage the scale-out"
[[ -z "$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip==$ip) | .ip')" ]] ||
  fail "worker $WB is still listed after the purge that stages the scale-out"

# Scale-out: the platform's registration, and what the very next call sees.
[[ "$(api_status POST /workers "$(jq -cn --arg ip "$WB" --arg p "${PUBLIC[$WB]}" '{ip: $ip, public: $p}')")" == 201 ]] ||
  fail "POST /workers for $WB was not a 201"
joined=$(worker_health "$WB")
# Registration is synchronous: the box is in the fleet on the read after its
# own 201. It is also out of placement until its reconcile and first poll have
# run — newWorkerConn stores overloaded and runWorker polls only once
# reconcileWorker is done — but which of the two states this read catches is a
# race whose window is one reconcile, so only "down" is a failure here: a box
# that joined down would never take a placement, down being sticky. The
# ordering itself is asserted deterministically at the end of this step, where
# $WB rejoins with leftovers on it.
[[ -n "$joined" ]] ||
  fail "worker $WB is absent from GET /workers on the read right after its own 201: registration is not visible to the next call"
[[ "$joined" != down ]] ||
  fail "worker $WB joined the fleet down: newWorkerConn must fail a fresh box closed as overloaded, which recovers on its first poll, not down, which only a worker-add clears"
t=$(date +%s)
retry 30 "worker $WB to finish provisioning and report ok" health_is "$WB" ok
note "scale-out: 201, '$joined' on the read right after it, eligible after $(( $(date +%s) - t ))s"

# Two launches over two ok workers are one each whatever the round-robin
# cursor (selectWorker advances it per placement), so this both says the
# re-registered box is really back in placement and seeds the drain with an
# instance that was serving before anything went down.
wb_ids=() wa_ids=()
for i in 30 31; do
  inst=$(launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i")
  track "$inst" "$i"
  id=$(jq -r .id <<<"$inst")
  if [[ "${INST_WORKER[$id]}" == "$WB" ]]; then wb_ids+=("$id"); else wa_ids+=("$id"); fi
done
(( ${#wb_ids[@]} == 1 )) ||
  fail "two launches over two ok workers put ${#wb_ids[@]} instance(s) on the re-registered $WB, expected one: it did not rejoin round robin"

# Scale-in under load. Six launches at once are three per worker over round
# robin, and a daemon has two launch slots (CMGR_CONCURRENT_LAUNCHES), so one
# of $WB's three is still queued when the PATCH lands. That one is the
# interesting case: acquireSlot waits on the worker's down channel as well as
# on its timer, so it must come back at once and retryable instead of sitting
# out the whole launch wait.
wb_before=$(worker_instances "$WB")
burst=$(mktemp -d)
for i in 32 33 34 35 36 37; do
  try_launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i" >"$burst/$i" &
done
# Wait for the placements themselves rather than guessing at them with a flat
# sleep: a launch inserts its row before it touches the daemon (the ordering
# reconcileWorker relies on), so the count rising by three says all of $WB's
# share is placed and past its image check. This costs ~100ms, far inside the
# second-odd a network and container create take, so the third is still queued.
placed_rows=$wb_before
for ((n = 0; n < 40; n++)); do
  placed_rows=$(worker_instances "$WB")
  if (( placed_rows >= wb_before + 3 )); then break; fi
  sleep 0.05
done
[[ "$(api_status PATCH "/workers/$WB" '{"health":"down"}')" == 204 ]] ||
  fail "PATCH /workers/$WB {\"health\":\"down\"} was not a 204"
# Synchronous, exactly as cmgrd-cli worker-down is (see down_worker): a poll
# already in flight cannot put it back.
health_is "$WB" down ||
  fail "worker $WB is not down on the read right after the PATCH: an in-flight telemetry poll overwrote it (the race fixed in ab72f5d)"
wait
refused=0 woken=0 busy=0
for i in 32 33 34 35 36 37; do
  out=$(cat "$burst/$i")
  code=${out%%$'\n'*}
  body=${out#*$'\n'}
  case "$code" in
    200|201)
      track "$body" "$i"
      id=$(jq -r .id <<<"$body")
      # A launch already holding a slot on $WB runs to completion there —
      # nothing interrupts a launch mid-flight and the daemon is healthy — so
      # its instance joins the drain rather than being an error.
      if [[ "$(jq -r .worker <<<"$body")" == "$WB" ]]; then wb_ids+=("$id"); else wa_ids+=("$id"); fi
      ;;
    503)
      if grep -q "worker went down" <<<"$body"; then
        refused=$(( refused + 1 ))
        grep -qF "$WB" <<<"$body" ||
          fail "launch $i was refused as worker-down but names no worker $WB: $(head -c 200 <<<"$body")"
        # The sharp one, and the reason it reads the message rather than the
        # clock: without the down-channel wake in acquireSlot the queued launch
        # waits out CMGR_WORKER_LAUNCH_WAIT and comes back with the slot
        # timeout, which launch() re-wraps in the same ErrWorkerDown and cmgrd
        # answers with the same 503. Only the inner message tells them apart.
        if grep -q "no launch slot" <<<"$body"; then
          fail "the launch aimed at $WB waited out its launch wait before being refused ($(head -c 200 <<<"$body")): acquireSlot no longer wakes waiters on the worker's down channel"
        fi
        if grep -q "waited for a launch slot" <<<"$body"; then woken=$(( woken + 1 )); fi
      elif grep -qE "worker is busy|database busy|image pull timed out" <<<"$body"; then
        # A burst may legitimately be refused by a worker's own launch queue or
        # lose the race for the database's write lock (ErrDatabaseBusy exists
        # for exactly this); both are retryable and may hit either worker.
        busy=$(( busy + 1 ))
      else
        fail "launch $i was refused with a 503 that is none of the retryable failures: $(head -c 200 <<<"$body")"
      fi
      ;;
    *) fail "launch $i during the scale-in of $WB returned HTTP $code, expected a placement or a retryable 503: $(head -c 200 <<<"$body")" ;;
  esac
done
rm -rf "$burst"
# Only assert this when the poll above saw all three of $WB's launches placed:
# then refused == 0 means every one of them ran to completion on a box that had
# been given up on, and the queue behind a draining daemon is no longer drained.
if (( placed_rows >= wb_before + 3 )); then
  (( refused > 0 )) ||
    fail "all three launches placed on $WB ran to completion although it was marked down while they were in flight: a launch aimed at a box being drained is no longer refused (acquireSlot's down channel and its post-acquire check)"
fi
note "six launches in flight at the PATCH: ${#wb_ids[@]} instance(s) now on $WB, $refused refused as retryable ($woken of them woken out of a launch queue), $busy refused by a queue or the write lock"

# down is sticky both ways. The API refuses to hand the box back to placement
# without rebuilding its connection...
[[ "$(api_status PATCH "/workers/$WB" '{"health":"ok"}')" == 400 ]] ||
  fail "PATCH /workers/$WB {\"health\":\"ok\"} was accepted: only \"down\" may be set by hand (cmd/cmgrd/workers_api.go), so a drain could be undone without a worker-add"
# ...and its instances keep running and serving through all of it.
for id in "${wb_ids[@]}"; do
  check_ondemand "${INST_META[$id]}" "${INST_USER[$id]}"
done
[[ "$(worker_instances "$WB")" == "${#wb_ids[@]}" ]] ||
  fail "GET /workers reports $(worker_instances "$WB") instance(s) on the draining $WB, expected ${#wb_ids[@]}: the count the autoscaler drains on is wrong"
# Its telemetry never stopped: the box is healthy and answers every poll, which
# is what down has to be sticky against, or a box inside its termination window
# drifts back into placement. Two poll intervals on top of the serving checks
# above are plenty (pollerSetHealth must refuse to leave down).
sleep 1
health_is "$WB" down ||
  fail "worker $WB left down on its own while its telemetry kept answering: down is not sticky (pollerSetHealth)"
note "$WB is out of placement with ${#wb_ids[@]} instance(s) still serving on it"

# The drain. Every stop on a down worker takes the DB-only path: the record
# goes, the containers stay on a box that is about to be wiped anyway, and the
# count falls by exactly one so the autoscaler can watch it reach zero.
remaining=${#wb_ids[@]}
for id in "${wb_ids[@]}"; do
  [[ "$(api_status DELETE "/instances/$id")" == 204 ]] || fail "the stop of instance $id on the down worker $WB was not a 204"
  [[ "$(api_status GET "/instances/$id")" == 404 ]] || fail "instance $id is still known to cmgrd after its stop"
  # The containers still being there is the whole proof that the stop skipped
  # docker: the daemon is healthy, so a stop that went out to it would have
  # succeeded quietly and left nothing to see.
  assert_on_worker "$WB" "${INST_META[$id]}"
  remaining=$(( remaining - 1 ))
  [[ "$(worker_instances "$WB")" == "$remaining" ]] ||
    fail "after stopping instance $id, GET /workers reports $(worker_instances "$WB") instance(s) on $WB, expected $remaining"
  forget "$id"
done
(( $(worker_instances "$WA") > 0 )) ||
  fail "GET /workers reports no instances on $WA while $WB drained to zero: the count is not per worker"

# The purge, once the count says zero. Only cork's records go: what the DB-only
# stops left keeps running until the machine goes away.
[[ "$(api_status DELETE "/workers/$WB")" == 204 ]] || fail "DELETE /workers/$WB was not a 204"
[[ -z "$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip==$ip) | .ip')" ]] ||
  fail "worker $WB is still listed after its purge"
[[ "$(api_status DELETE "/workers/$WB")" == 404 ]] ||
  fail "a second DELETE of the purged worker $WB was not a 404: its record outlived the purge"
for id in "${wb_ids[@]}"; do
  assert_on_worker "$WB" "${INST_META[$id]}"
done
note "purged $WB with the containers of ${#wb_ids[@]} drained instance(s) still running on the box"

# The box is not really terminated here, so put it back and let the worker-add
# do what termination would have done. readd returns only once $WB reports ok,
# and the leftovers must already be gone by then: reconcileWorker runs before
# the poller (runWorker), which is what keeps a launch off a box whose
# leftovers still hold its host ports.
readd "$WB"
DRAINED_WORKER=""
for id in "${wb_ids[@]}"; do
  orphan_gone "$WB" "$id" "${INST_META[$id]}" ||
    fail "worker $WB reported ok while the leftovers of instance $id were still on it: the reconcile did not finish before the poller started"
done
[[ "$(worker_instances "$WB")" == 0 ]] || fail "re-added worker $WB still counts instances"
# Everything this step put on $WA goes too: later steps count instances.
stop_tmp=$(mktemp -d)
for id in "${wa_ids[@]}"; do
  api_status DELETE "/instances/$id" >"$stop_tmp/$id" &
done
wait
for id in "${wa_ids[@]}"; do
  [[ "$(cat "$stop_tmp/$id")" == 204 ]] || fail "the stop of instance $id on $WA answered HTTP $(cat "$stop_tmp/$id")"
  assert_gone "$id" "${INST_WORKER[$id]}" "${INST_META[$id]}"
  forget "$id"
done
rm -rf "$stop_tmp"
ok "scale-out: $WB answered 201 and was in the fleet on the next read, not down, and eligible once its reconcile and first poll were through; scale-in: PATCH down took it out of placement at once, refused $refused launch(es) aimed at it as retryable without any of them waiting out the launch wait, held down against healthy telemetry and a PATCH back to ok, kept ${#wb_ids[@]} instance(s) serving, and drained to zero over the DB-only stop path; DELETE purged the records and left the containers on the box for its termination"

# BLOCK 3 -- insert after e2e/scenario.sh:2140 (the `ok` that closes the
# autoscaling lifecycle step), immediately before the "16. teardown" banner.
# It launches nothing and stops nothing, so it disturbs neither OD_IDS nor the
# teardown's counts.
# ============================================================================

# ------------------------- 15c. a rebuild that fails validation

if (( FULL )); then
  step "a rebuild that fails validation keeps serving the previous archive"
  # The staged path exists for exactly this case (the comment at
  # cmgr/docker.go:770-776): a rebuild that fails validation must leave the
  # previous archive alone, because the build row is rolled back rather than
  # removed and cork goes on serving it. Delete or half-write that file and
  # every player download of a challenge whose last update was broken becomes an
  # error, with nothing in the update's own output to say so.
  pm_before=$(api GET "/instances/$PERSIST_INST")
  ppub=$(jq -r .worker_public <<<"$pm_before")
  pport=$(jq -r .ports.socat <<<"$pm_before")
  pworker=$(jq -r .worker <<<"$pm_before")
  # What the builder and the registry hold for this challenge before the failed
  # rebuild. The push waits for validation (publishImages, cmgr/docker.go), so
  # a generation that fails it never reaches zot; the local untag on the
  # failure path (executeBuild's failure block) is the only reclamation on the
  # builder, and this step has to leave both as it found them.
  persist_local_tags() {
    local btags
    btags=$(builder_tags)
    grep "^$E2E_REGISTRY/$CH_PERSISTENT:" <<<"$btags" | sort || true
  }
  btags_before=$(persist_local_tags)
  rtags_before=$(registry_tags "$CH_PERSISTENT")

  # Generation 4 of the persistent challenge, publishing one file its problem.md
  # does not reference. set_generation is called with "both" because only its
  # "both" arm re-stamps the persistent source; that arm rewrites the on-demand
  # source too, with the generation the live-traffic step already built, so
  # those bytes do not change and that challenge is not rebuilt. Checked rather
  # than asserted in a comment: an on-demand rebuild here would tear down every
  # tracked on-demand instance behind the scenario's back, and the teardown --
  # where a stop is idempotent and the containers really are gone -- would not
  # notice.
  cp "$CHALLENGES/$ONDEMAND_SRC" "$ART_TMP/ondemand.before"
  persist_gen_before=$PERSIST_GEN
  set_generation 6 persistent
  # Kept even though the mode above cannot touch it: this is the assertion that
  # says the on-demand challenge must not be rebuilt here, and it would catch a
  # later edit to set_generation that broke that.
  same_bytes "$ART_TMP/ondemand.before" "$CHALLENGES/$ONDEMAND_SRC" ||
    fail "the persistent-only generation bump changed $ONDEMAND_SRC: this step's update would rebuild the on-demand challenge and tear down the instances the scenario still tracks"
  # cacheArtifacts stages the three-entry archive and validateBuild then refuses
  # the build (cmgr/loader.go:487-491) -- the cheapest failure that happens
  # AFTER the archive is staged. A build that failed earlier (a broken
  # Dockerfile) never reaches the promotion at all and would prove nothing about
  # it. The extra file is planted by the Dockerfile rather than taken from
  # whatever docker happens to provide (/etc/hostname inside a RUN is a bind
  # mount, not image content), so the fixture and the expected message are
  # deterministic. BOTH ampersands are escaped: an unescaped & in a sed
  # replacement stands for the whole match, which would splice the tar line into
  # itself and break the build for the wrong reason.
  sed -i "s|^RUN tar czvf /challenge/artifacts.tar.gz -C / secret.enc time.txt\$|RUN echo e2e-unreferenced > /e2e-extra.txt \&\& tar czvf /challenge/artifacts.tar.gz -C / secret.enc time.txt e2e-extra.txt|" \
    "$CHALLENGES/$PERSISTENT_SRC"
  grep -q 'e2e-extra.txt' "$CHALLENGES/$PERSISTENT_SRC" ||
    fail "could not make $PERSISTENT_SRC publish an artifact its problem.md does not reference (did examples/custom/Dockerfile change?)"
  t=$(date +%s)
  rc=0
  # No --prune-old: nothing may be reclaimed here, and a build that never
  # finalized displaces nothing anyway. The status is captured because a
  # non-zero exit is the expected outcome, and errexit would abort the run
  # before a word of the output was printed.
  out=$(cmgrd-cli update 2>&1) || rc=$?
  sed 's/^/       /' <<<"$out"
  note "the failing rebuild took $(( $(date +%s) - t ))s"
  (( rc != 0 )) ||
    fail "cmgrd-cli update exited 0 although the rebuilt image publishes an artifact the challenge text never references: validateBuild (cmgr/loader.go:487-491) let it through, so everything below would pass for the wrong reason"
  grep -q '^Errors:' <<<"$out" ||
    fail "the update printed no Errors section although the rebuild of $CH_PERSISTENT had to fail validation"
  # The negative control: without it a build that failed for any other reason (a
  # broken Dockerfile, a registry hiccup) would satisfy every archive assertion
  # below without ever having staged anything.
  grep -q "artifact file 'e2e-extra.txt' published but not referenced" <<<"$out" ||
    fail "the update failed for some reason other than the unreferenced artifact this step planted, so the archive assertions below would prove nothing: $(head -c 300 <<<"$out")"
  grep -q "  $CH_PERSISTENT\$" <<<"$out" ||
    fail "the update did not report $CH_PERSISTENT as changed, so no rebuild ran and nothing was staged"
  if grep -q "  $CH_ONDEMAND\$" <<<"$out"; then
    fail "the update rebuilt $CH_ONDEMAND as well, although its source was supposed to be byte-identical to the generation the live-traffic step built: its instances are still tracked and have just been torn down"
  fi

  # The row is rolled back, not removed: finalizeBuild is never reached
  # (cmgr/database_challenges.go:661-666), so the build keeps the checksum, the
  # flag and the has_artifacts of the generation that is still installed.
  [[ "$(api_status GET "/builds/$PERSIST_BUILD")" == 200 ]] ||
    fail "build $PERSIST_BUILD is gone after a rebuild that failed validation: a failed update rolls the row back, it does not delete it"
  pbuild=$(api GET "/builds/$PERSIST_BUILD")
  [[ "$(jq -r .checksum <<<"$pbuild")" == "$PERSIST_CHECKSUM_G2" ]] ||
    fail "build $PERSIST_BUILD's checksum moved to $(jq -r .checksum <<<"$pbuild") after a rebuild that failed validation, expected the generation-2 value $PERSIST_CHECKSUM_G2: the row was finalized for a build that never validated"
  [[ "$(jq -r .flag <<<"$pbuild")" == "$PERSIST_FLAG" ]] ||
    fail "build $PERSIST_BUILD's flag changed although its rebuild failed"
  [[ "$(jq -r .has_artifacts <<<"$pbuild")" == true ]] ||
    fail "build $PERSIST_BUILD reports has_artifacts=false after a failed rebuild: the platform would stop offering a download cork is still able to serve"

  # The assertion this step exists for: the download still works, and it is the
  # old archive, byte for byte. Not merely "a 200" -- a staged archive promoted
  # by a failure would answer 200 too, with three members and a time.txt from a
  # build cork has rolled back.
  ART_FAIL="$ART_TMP/persist-after-fail.tar.gz"
  out=$(cmgrd-cli artifacts "$PERSIST_BUILD" "$ART_FAIL" 2>&1) ||
    fail "cmgrd-cli artifacts $PERSIST_BUILD failed after a failed rebuild: the previous archive was removed or truncated, so every player download of this challenge is now an error ($out)"
  members=$(art_members "$ART_FAIL") ||
    fail "the archive served after the failed rebuild is not a readable gzip tar"
  [[ "$members" == "secret.enc time.txt" ]] ||
    fail "the archive served for build $PERSIST_BUILD after the failed rebuild holds '$members': the staged generation-4 archive (the one publishing e2e-extra.txt) was promoted although validateBuild rejected the build; the failure path must remove the staged file, not rename it (cmgr/docker.go:826-829)"
  gen=$(tar -xzOf "$ART_FAIL" time.txt) ||
    fail "could not read time.txt out of the archive served after the failed rebuild"
  [[ "$gen" == "e2e-generation-2" ]] ||
    fail "the archive served for build $PERSIST_BUILD carries '$gen' in time.txt after the failed rebuild, expected the generation-2 archive that is still the installed one"
  same_bytes "$ART_G2" "$ART_FAIL" ||
    fail "the archive served for build $PERSIST_BUILD is not byte for byte the one it served before the failed rebuild: something wrote to <id>.tar.gz on a path that may only ever touch the staged copy"

  # And what the rolled-back build still runs: untouched, at the same address,
  # still serving generation 2. A failed update that tore down its persistent
  # instance would take the challenge off the board entirely.
  pm_after=$(api GET "/instances/$PERSIST_INST")
  [[ "$(jq -r .worker <<<"$pm_after")" == "$pworker" && "$(jq -r .ports.socat <<<"$pm_after")" == "$pport" ]] ||
    fail "persistent instance $PERSIST_INST moved from $pworker:$pport to $(jq -r .worker <<<"$pm_after"):$(jq -r .ports.socat <<<"$pm_after") over a rebuild that failed: a build that never validated must not restart anything"
  retry 15 "persistent instance $PERSIST_INST at $ppub:$pport to still serve generation 2 after the failed rebuild" \
    tcp_says "$ppub" "$pport" '' 'e2e-generation-2'
  if (( ${#OD_IDS[@]} )); then
    [[ "$(api_status GET "/instances/${OD_IDS[0]}")" == 200 ]] ||
      fail "on-demand instance ${OD_IDS[0]} was torn down by an update that only rebuilt $CH_PERSISTENT, and failed at that"
  fi

  # Two assertions about what the failure left behind. The failed generation's
  # images are untagged locally on the failure path (executeBuild's failure
  # block), so the builder must hold exactly what it held. The registry must
  # hold exactly what it held too, in both directions: nothing on the failure
  # path touches it, so no tag may have left; and the push waits for
  # validation (publishImages), so the failed generation's tag must never have
  # arrived. Before that ordering, the tag was pushed inside the build loop and
  # this step had to delete it by hand.
  btags_after=$(persist_local_tags)
  [[ "$btags_after" == "$btags_before" ]] ||
    fail "the builder's tags for $CH_PERSISTENT changed over a rebuild that failed validation ('$(tr '\n' ' ' <<<"$btags_before")' -> '$(tr '\n' ' ' <<<"$btags_after")'): a failed generation no row retains must be untagged again (executeBuild's failure block, cmgr/docker.go)"
  rtags_after=$(registry_tags "$CH_PERSISTENT")
  while read -r tg; do
    [[ -n "$tg" ]] || continue
    grep -qxF -- "$tg" <<<"$rtags_after" ||
      fail "tag $E2E_REGISTRY/$CH_PERSISTENT:$tg left the registry over a rebuild that failed validation: the failure path untags locally, never remotely, and that tag is still a generation cork serves"
  done <<<"$rtags_before"
  while read -r tg; do
    [[ -n "$tg" ]] || continue
    grep -qxF -- "$tg" <<<"$rtags_before" ||
      fail "tag $E2E_REGISTRY/$CH_PERSISTENT:$tg appeared in the registry over a rebuild that failed validation: the push must wait for the build to validate (publishImages, cmgr/docker.go), or every failed generation leaves a tag in zot that no row names"
  done <<<"$rtags_after"

  # What a later update would make of this, before the source is restored. On
  # disk and in the challenge row the persistent challenge is at the failed
  # generation (updateChallenges commits the metadata before it builds), and
  # its build still serves generation 2. Compared by checksums alone that is
  # "unmodified", and the failed rebuild would be invisible to every update
  # after this one; the source generation recorded on the build
  # (builds.sourcechecksum) is what keeps it reported, as Stale, until a
  # rebuild succeeds. Nothing else may be affected: the on-demand challenge is
  # current, and nothing changed on disk since the failing update, so no
  # challenge is Updated.
  out=$(cmgrd-cli update --dry-run 2>&1) ||
    fail "cmgrd-cli update --dry-run failed after the failed rebuild: $out"
  sed 's/^/       /' <<<"$out"
  stale_section=$(sed -n '/^Stale:/,/^[A-Z]/p' <<<"$out")
  grep -q "^  $CH_PERSISTENT\$" <<<"$stale_section" ||
    fail "the dry run after the failed rebuild does not list $CH_PERSISTENT under Stale: build $PERSIST_BUILD serves generation 2 while the challenge row is at the failed generation, and an update that cannot see that leaves the challenge stale for good (DetectChanges, cmgr/api.go)"
  if grep -q "^  $CH_ONDEMAND\$" <<<"$stale_section"; then
    fail "the dry run lists $CH_ONDEMAND as Stale although every build of it is at the current generation"
  fi
  if grep -q "^Updated:" <<<"$out"; then
    fail "the dry run after the failed rebuild reports a challenge as Updated although nothing changed on disk since the failing update: $(sed -n '/^Updated:/,/^[A-Z]/p' <<<"$out" | tr '\n' ' ')"
  fi

  # The persistent source back exactly as this step found it, and the on-demand
  # source not touched at all -- this step never edits it, and the assertion
  # above is what says so. The generation is read from PERSIST_GEN rather than
  # named: which one the earlier steps left behind depends on the mode.
  #
  # cork's challenge row still holds this generation's source checksum, so an
  # update inserted after this point sees the source as changed and rebuilds
  # the persistent challenge (Updated outranks Stale) and restarts its
  # instance. The base-pins step below is the one that does, deliberately: it
  # absorbs this restore alongside its own edit, which is why its update reports
  # two challenges rebuilt and why it does not assert that only one was.
  set_generation "$persist_gen_before" persistent
  rm -rf "$ART_TMP"
  ok "the rebuild was refused for publishing an unreferenced artifact; the build row kept its checksum, flag and has_artifacts; cork went on serving the generation-2 archive byte for byte; instance $PERSIST_INST kept serving at $ppub:$pport; a dry run named $CH_PERSISTENT Stale; the builder is back to the tags it held and the registry holds exactly the tags it held"
else
  deselect "a failed rebuild keeps serving the previous archive"
fi

# ----------------------- 15c. a registry that accepts and never answers

if (( FULL )); then
  step "slow registry: a launch whose image pull runs out of time is a retryable 503 that leaves its worker in the fleet, and a worker that already holds the tag launches with the registry dead"
  # The stand-in that accepts a connection and answers nothing is an openssl
  # s_server run from this image, so without openssl the fixture cannot be
  # built at all. That is a hole in the run, not a choice: skip, not deselect.
  if (( OUTER )) && command -v openssl >/dev/null 2>&1; then
    # Two promises, one fixture. ensureImages offers every launch failure but
    # one to noteWorkerTransportError (cmgr/launch.go:142-150); a pull that
    # merely ran out of time is exempt, because the registry is the likelier
    # culprit. The regression answers the platform the same retryable 503
    # (ErrPullTimeout, cmd/cmgrd/main.go:447-452) while quietly taking the box
    # out of the fleet until an operator re-adds it -- in production one slow
    # registry would walk the whole fleet down. The second promise is
    # imagePresent (cmgr/launch.go:163-178): a tag the daemon already holds is
    # never pulled, so a launch onto a warm worker does not touch the registry
    # at all. Nothing else in this scenario reaches pullImage's timeout.
    PULL_TIMEOUT=30 # cork sets no CMGR_WORKER_PULL_TIMEOUT, so the default (cmgr/workers.go:76)
    PROBE_SEED=99   # outside schema.yaml's seeds (1, 3, 5, 7): this build is nobody else's

    # A run killed before its EXIT trap leaves its probe build behind, and a
    # second build with the same challenge, seed and checksum makes
    # contentReferenced true (cmgr/database_builds.go:224), so destroyImages
    # would keep the very images this step asserts it untags -- and the next
    # teardown would fail on a builder tag nobody can explain. Clear it here,
    # as the schema and phantom-worker steps clear their own leftovers.
    stale=$(manual_builds "$CH_ONDEMAND" "$PROBE_SEED") ||
      fail "could not read the build list (cmgrd-cli system-dump) to look for a leftover probe build"
    for b in $stale; do
      note "destroying build $b of $CH_ONDEMAND seed $PROBE_SEED, left behind by an earlier run"
      cmgrd-cli destroy "$b" || fail "could not destroy the leftover probe build $b of $CH_ONDEMAND"
    done

    zot=$(compose_container zot)
    [[ -n "$zot" ]] || fail "compose container for the registry (service zot) not found"
    zot_json=$(outer "/containers/$zot/json")
    zot_net=$(jq -r '(.NetworkSettings.Networks | keys | .[0]) // empty' <<<"$zot_json")
    # .Name is what a Bind takes for the compose volume; .Source covers a
    # checkout that mounts the registry's certificates from the host instead.
    zot_certs=$(jq -r '[(.Mounts // [])[] | select(.Destination == "/etc/zot/certs") | (.Name // .Source)] | .[0] // empty' <<<"$zot_json")
    [[ -n "$zot_net" && -n "$zot_certs" ]] ||
      fail "could not read the registry's network ('$zot_net') and certificate volume ('$zot_certs') from the outer docker API"

    # A build of its own, made while the registry is still up. It is the only
    # way to have a tag no worker holds this late in the run -- both have
    # launched every generation of $CH_ONDEMAND -- and every layer below the
    # FLAG is one they already carry, so the pull that finally succeeds moves
    # kilobytes rather than the ubuntu base. The seed is outside schema.yaml,
    # so the build is manual (cmgr/api.go:254): it can be destroyed again (a
    # schema's build cannot, cmgr/api.go:451) and nothing else shares its
    # challenge+seed+checksum, which is what makes destroyImages untag it.
    out=$(cmgrd-cli build --flag-format 'e2e{%s}' "$CH_ONDEMAND" "$PROBE_SEED" 2>&1) ||
      fail "cmgrd-cli build $CH_ONDEMAND $PROBE_SEED failed: $out"
    PROBE_BUILD=$(awk 'NF == 1 && $1 ~ /^[0-9]+$/ {print $1; exit}' <<<"$out") # "Build IDs:" then one indented id
    [[ "$PROBE_BUILD" =~ ^[0-9]+$ ]] || fail "cmgrd-cli build printed no build id: $out"
    probe_meta=$(api GET "/builds/$PROBE_BUILD")
    probe_tag=""
    for host in $(jq -r '.images[].host' <<<"$probe_meta"); do
      if [[ "$host" == builder ]]; then continue; fi
      probe_tag=$(image_tag "$probe_meta" "$host")
    done
    [[ -n "$probe_tag" ]] || fail "build $PROBE_BUILD of $CH_ONDEMAND has no launchable image to pull"
    has_line "$probe_tag" registry_tags "$CH_ONDEMAND" ||
      fail "build $PROBE_BUILD did not push $E2E_REGISTRY/$CH_ONDEMAND:$probe_tag; there is nothing for a worker to pull"
    # The negative control for the whole step: a worker that already held this
    # tag would skip the pull entirely (imagePresent) and the launch below
    # would succeed with the registry dead, proving the opposite of the point.
    for ip in "${WORKER_IPS[@]}"; do
      if has_line "$E2E_REGISTRY/$CH_ONDEMAND:$probe_tag" worker_tags "$ip"; then
        fail "worker $ip already holds $probe_tag, so the launch below would not pull at all"
      fi
    done
    # Whether the warm rider below can run: it needs a build whose image both
    # workers hold, which every launch since the generation-4 rebuild has
    # arranged. Checked rather than assumed, because a fleet that is behaving
    # perfectly must never fail on a bonus assertion.
    warm=1
    for ip in "${WORKER_IPS[@]}"; do
      has_line "$E2E_REGISTRY/$CH_ONDEMAND:${IMAGE_TAG[$CH_ONDEMAND]}" worker_tags "$ip" || warm=0
    done

    # A registry that accepts the connection, finishes the handshake, and then
    # never answers -- what a wedged one looks like to a puller. Pausing zot
    # would not do it: a paused container still completes the TCP handshake in
    # the kernel, so what stalls is the TLS handshake, and dockerd's registry
    # transport bounds that at ten seconds, well inside the ${PULL_TIMEOUT}s
    # pull timeout. A pause therefore comes back as a pull *error* and this
    # step would be asserting the other branch. The stand-in completes the
    # handshake with zot's own certificate and key (the workers verify it
    # against the same CA out of certs.d) and then holds the request open,
    # which only the pull timeout can end.
    STOPPED_REGISTRY=$zot
    outer_ctl "$zot" stop
    stall_body=$(jq -cn --arg img "$E2E_IMAGE" --arg net "$zot_net" --arg vol "$zot_certs" --arg reg "$E2E_REGISTRY" '{
      Image: $img,
      Entrypoint: ["bash", "-c",
        "tail -f /dev/null | openssl s_server -quiet -accept 443 -cert /certs/zot-server-cert.pem -key /certs/zot-server-key.pem"],
      HostConfig: {Binds: [$vol + ":/certs:ro"], NetworkMode: $net, AutoRemove: true},
      NetworkingConfig: {EndpointsConfig: {($net): {Aliases: [$reg]}}}}')
    STALL_REGISTRY=$(outer /containers/create -X POST -H 'Content-Type: application/json' -d "$stall_body" | jq -r '.Id // empty')
    [[ -n "$STALL_REGISTRY" ]] || fail "could not create the stand-in registry from $E2E_IMAGE"
    outer_ctl "$STALL_REGISTRY" start
    # curl exit 28 is the fixture: the connection was made, the handshake
    # finished, and nothing answered. 7 (refused) or 35/60 (TLS) mean the
    # stand-in is wrong, not cork, so they are not accepted as ready.
    stall_stalls() {
      local rc=0
      registry_curl /v2/ --max-time 5 -o /dev/null >/dev/null 2>&1 || rc=$?
      (( rc == 28 ))
    }
    retry 25 "the stand-in registry to accept a connection and stall on it (openssl s_server with zot's own certificate; if this is what times out, the fixture is broken, not cork)" stall_stalls
    # s_server serves one connection at a time and the probe above just held
    # one for five seconds: give it a moment to be back in accept() before the
    # pull arrives, or the pull would sit in the kernel's backlog and hit
    # dockerd's ten-second TLS bound instead of cork's pull timeout.
    sleep 1
    note "the registry now accepts connections at $E2E_REGISTRY:443 and never answers"

    pull_dir=$(mktemp -d)
    rows_before=$(api GET /workers | jq -r 'map(.instances) | add')
    [[ "$rows_before" =~ ^[0-9]+$ ]] || fail "could not read the fleet's instance count before the stalled launch"
    probe_body=$(jq -cn '{user_id: "e2e-user-98", env: {CUSTOM_VAR: "e2e-value-98"}}')
    # In the background, so the rider below runs inside its own dead time.
    timed_launch "$pull_dir" pull "$PROBE_BUILD" "$probe_body" &
    pull_pid=$!

    if (( warm )); then
      # The rider, free because it runs inside the ${PULL_TIMEOUT}s the pull
      # timeout costs anyway: a launch whose tag its daemon already holds is
      # never pulled, so a dead registry must cost it nothing (imagePresent
      # before the pull, cmgr/launch.go:163-178; and the whole image stage
      # runs before acquireLaunchSlot, cmgr/launch.go:108-115, so a stalled
      # pull holds none of the daemon's launch slots either). A blind pull in
      # place of imagePresent stalls this launch on the same dead registry and
      # fails it with the pull timeout's own wording, which the 503 arm below
      # tells apart from the refusals that say nothing about the registry.
      sleep 3
      out=$(try_launch "$OD_BUILD" e2e-user-97 e2e-value-97)
      code=${out%%$'\n'*}
      body=${out#*$'\n'}
      case "$code" in
        200|201)
          warm_id=$(jq -r .id <<<"$body")
          track "$body" 97
          check_ondemand "$body" 97
          cmgrd-cli stop "$warm_id"
          assert_gone "$warm_id" "${INST_WORKER[$warm_id]}" "${INST_META[$warm_id]}"
          forget "$warm_id"
          note "instance $warm_id launched, served and stopped on a warm worker while the registry was dead"
          ;;
        503)
          if grep -q "image pull timed out" <<<"$body"; then
            fail "a launch of $CH_ONDEMAND, whose image both workers already hold, timed out pulling from the dead registry: imagePresent no longer skips the pull for a tag the daemon has (cmgr/launch.go:163-178), so every launch in the fleet now depends on the registry ($(head -c 200 <<<"$body"))"
          fi
          # A busy queue or a lost race for the database's write lock is
          # retryable and says nothing about the registry: it costs the rider
          # its assertion rather than failing a fleet that behaved correctly.
          note "the warm launch was refused as retryable while the registry was dead, so it proved nothing here: $(head -c 160 <<<"$body")"
          ;;
        *)
          fail "a launch of $CH_ONDEMAND, whose image both workers already hold, answered HTTP $code with the registry dead: a present tag is never pulled (imagePresent, cmgr/launch.go:163-178), so a dead registry must cost this launch nothing ($(head -c 200 <<<"$body"))"
          ;;
      esac
    else
      note "not both workers hold ${IMAGE_TAG[$CH_ONDEMAND]}, so the warm launch was not attempted"
    fi
    wait "$pull_pid" || true # timed_launch records curl's own failure rather than raising it

    code=000; took=0
    read -r code took <"$pull_dir/pull.code" || true
    took_ms=$(jq -rn --arg t "$took" '($t | tonumber) * 1000 | floor' 2>/dev/null || echo "")
    [[ "$took_ms" =~ ^[0-9]+$ ]] || fail "could not read how long the stalled launch took (curl reported '$took')"
    body=$(cat "$pull_dir/pull.body" 2>/dev/null || true)
    if [[ "$code" != 503 ]]; then
      # Separated from a cork verdict on purpose: these are the answers a
      # stand-in that failed to stall produces, and blaming cork for them
      # would send the next reader down the wrong path entirely.
      if grep -qiE 'tls handshake|connection refused|no such host|certificate' <<<"$body"; then
        fail "the stalled launch answered HTTP $code after ${took_ms}ms with a registry error rather than a timeout: the stand-in did not hold the pull open (dockerd bounds a TLS handshake long before cork's ${PULL_TIMEOUT}s pull timeout), so this is the fixture failing, not cork: $(head -c 200 <<<"$body")"
      fi
      fail "the launch whose pull hung on the registry answered HTTP $code after ${took_ms}ms, expected 503: a pull that ran out of time is retryable (ErrPullTimeout at cmd/cmgrd/main.go:447-452), and a 500 is what makes the platform's celery worker give the player up ($(head -c 200 <<<"$body"))"
    fi
    grep -q "image pull timed out" <<<"$body" ||
      fail "the 503 does not name the pull timeout, so it is some other retryable refusal and this step proved nothing: $(head -c 200 <<<"$body")"
    # Which of the two deadlines ended the wait. Both wordings carry the text
    # above, so without this the step cannot tell the daemon's registry
    # timeout (registryTimedOut, cmgr/docker.go) from cmgrd's own context
    # expiring -- and it is the first that fires here and the first that had
    # no coverage. If this ever trips because the daemon stopped bounding its
    # registry requests, the fix is to say so, not to drop the check.
    grep -q "on the daemon's own registry timeout" <<<"$body" ||
      fail "the 503 names a pull timeout but not the daemon's own registry timeout, so cmgrd's context deadline is what expired: registryTimedOut no longer classifies what the daemon reports, and a pull that hung would be a 500 again on any deployment where the daemon gives up first ($(head -c 250 <<<"$body"))"
    # The regression's fingerprint, visible in the body before any health
    # read: noteWorkerTransportError on a timed-out pull marks the worker
    # down, and launch (cmgr/launch.go:91-96) then re-wraps that very error as
    # ErrWorkerDown -- still a 503, still naming the pull timeout, with
    # "worker went down" in front of it.
    if grep -q "worker went down" <<<"$body"; then
      fail "the timed-out pull came back as a worker-down failure ($(head -c 200 <<<"$body")): ensureImages offered ErrPullTimeout to noteWorkerTransportError (cmgr/launch.go:142-150), so one slow registry now takes boxes out of the fleet"
    fi
    grep -qi '^Retry-After: 1' "$pull_dir/pull.head" ||
      fail "the 503 of the timed-out pull carried no Retry-After, so the platform will not place the retry: $(tr -d '\r' <"$pull_dir/pull.head" | head -1)"
    # It waited for a pull, and it was cmgrd's ceiling that bounded the wait
    # from above. The lower bound is deliberately loose and not read off
    # PULL_TIMEOUT: the daemon bounds its own registry requests more tightly
    # than cmgrd does (about 15s against 30s here), so against a registry that
    # never answers it is the daemon's deadline that ends the wait and cmgrd's
    # that never gets to. That asymmetry is the subject of registryTimedOut
    # (cmgr/docker.go) -- without it this launch is a 500. What must not
    # happen is an instant refusal, which would mean the pull failure had been
    # reclassified as something else; and the ceiling must still be cmgrd's,
    # not restartPullTimeout's five minutes (cmgr/launch.go:70) leaking into a
    # request launch.
    (( took_ms >= 2000 )) ||
      fail "the launch was refused after only ${took_ms}ms: it never waited for a pull at all, so what failed was not a timeout of either kind"
    (( took_ms <= (PULL_TIMEOUT + 15) * 1000 )) ||
      fail "the launch took ${took_ms}ms to give up, past cmgrd's ${PULL_TIMEOUT}s pull timeout: the pull is bounded by something else (restartLimits' five-minute ceiling leaking into a request launch, or the transport timeout)"
    # down is sticky, so these single reads are the whole assertion, and they
    # are the ones that cost a production box if the exemption ever goes.
    for ip in "${WORKER_IPS[@]}"; do
      if health_is "$ip" down; then
        fail "worker $ip was marked down by a launch whose image pull timed out: the pull timeout indicts the registry, not the box, and a worker downed here takes no further placement until an operator's worker-add (cmgr/launch.go:142-150)"
      fi
    done
    retry 20 "both workers to report ok after the stalled launch" all_workers_ok
    rows_after=$(api GET /workers | jq -r 'map(.instances) | add')
    (( rows_after == rows_before )) ||
      fail "the fleet records $rows_after instance(s) after the refused launch, expected the $rows_before it started with: nothing of it reached a daemon, so its row and its ports must have been cleared (clearInstanceRecords, cmgr/api.go:369-376)"
    [[ "$(api GET "/builds/$PROBE_BUILD" | jq -r '.instances // [] | length')" == 0 ]] ||
      fail "build $PROBE_BUILD still records an instance although its only launch was refused"

    # The registry comes back and the same launch goes through, on a worker
    # nobody re-added. The stand-in goes first: while it holds the
    # $E2E_REGISTRY alias a lookup could land on either of them. Its
    # entrypoint is a pipeline, so bash swallows the SIGTERM and only the kill
    # ends it -- t=1 rather than waiting out docker's ten-second default,
    # which is why this is not outer_ctl.
    outer "/containers/$STALL_REGISTRY/stop?t=1" -X POST -o /dev/null
    STALL_REGISTRY="" # AutoRemove, so stopping it removes it
    retry 20 "the registry container to start" quiet outer_ctl "$zot" start
    STOPPED_REGISTRY=""
    retry 60 "the registry to answer again" quiet registry_api /v2/
    out=$(try_launch "$PROBE_BUILD" e2e-user-98 e2e-value-98)
    code=${out%%$'\n'*}
    body=${out#*$'\n'}
    case "$code" in
      200|201) ;;
      *) fail "the same launch answered HTTP $code with the registry back: a pull that ran out of time must leave its build launchable and its worker taking placements with no worker-add ($(head -c 200 <<<"$body"))" ;;
    esac
    probe_id=$(jq -r .id <<<"$body")
    track "$body" 98
    probe_worker=${INST_WORKER[$probe_id]}
    assert_on_worker "$probe_worker" "$body"
    has_line "$E2E_REGISTRY/$CH_ONDEMAND:$probe_tag" worker_tags "$probe_worker" ||
      fail "worker $probe_worker launched instance $probe_id without holding $probe_tag: the retry did not pull what the timed-out launch could not"
    pub=$(jq -r .worker_public <<<"$body")
    port=$(jq -r .ports.server <<<"$body")
    (( port >= PORT_LOW && port <= PORT_HIGH )) || fail "port $port is outside CMGR_PORTS $PORT_LOW-$PORT_HIGH"
    # http_says rather than check_ondemand: this build has a seed of its own
    # and therefore a flag of its own, which is not $OD_FLAG.
    retry 30 "instance $probe_id at $pub:$port" http_says "$pub" "$port" "CMGR_USER_ID=e2e-user-98"
    cmgrd-cli stop "$probe_id"
    assert_gone "$probe_id" "$probe_worker" "$body"
    forget "$probe_id"

    # Leave the fleet as it was found: the probe image off both workers (only
    # one of them pulled it, but a 404 costs nothing and a leftover image
    # would outlive the run -- destroyImages only untags on the builder), the
    # probe build gone from cork, and the builder and registry back to the
    # tags they held. With the registry up the untag is not best effort --
    # that contract is the next step's -- so it is asserted here.
    for ip in "${WORKER_IPS[@]}"; do
      code=$(worker_status "$ip" "/images/$E2E_REGISTRY/$CH_ONDEMAND:$probe_tag?force=true" -X DELETE)
      case "$code" in
        200|404) ;;
        *) fail "could not remove the probe image $probe_tag from worker $ip: HTTP $code (409 means a container still holds it)" ;;
      esac
    done
    cmgrd-cli destroy "$PROBE_BUILD" || fail "cmgrd-cli destroy $PROBE_BUILD failed; its images would outlive the run"
    [[ "$(api_status GET "/builds/$PROBE_BUILD")" == 404 ]] || fail "build $PROBE_BUILD survived its destroy"
    if has_line "$probe_tag" registry_tags "$CH_ONDEMAND"; then
      fail "destroying build $PROBE_BUILD left $probe_tag in the registry although it could reach it"
    fi
    if has_line "$E2E_REGISTRY/$CH_ONDEMAND:$probe_tag" builder_tags; then
      fail "destroying build $PROBE_BUILD left $probe_tag on the builder"
    fi
    PROBE_BUILD=""
    rm -rf "$pull_dir"
    ok "a pull stalled on the registry failed its launch as a 503 + Retry-After after ${took_ms}ms, inside cmgrd's ${PULL_TIMEOUT}s ceiling (the daemon's own registry timeout is the one that fires), naming the timeout and not the worker; both workers stayed in the fleet with no worker-add and no record left behind, a warm worker launched and served with the registry dead, and the same launch went through once it answered again"
  else
    skip "slow registry: needs the outer docker socket and an openssl in this image (see e2e/cork.Dockerfile)"
  fi
else
  deselect "slow registry: a pull that times out is a retryable 503 that does not down its worker"
fi

# ------------------------------ 15d. a destroy with no registry to call

if (( FULL )); then
  step "registry down: a build is still destroyed and untagged on the builder, and the registry tag it leaks is recoverable"
  if (( OUTER )); then
    # cmgr/registry.go:57-70 states the contract in as many words: the
    # registry untag is best effort, a leaked registry tag is recoverable and
    # a failed operation is not. Its two callers, destroyImages
    # (cmgr/docker.go:1387-1391) and pruneReplacedImages
    # (cmgr/docker.go:1319-1325), only log what it returns, so an unreachable
    # registry may cost the operator a tag and nothing else: not the destroy,
    # not the local untag, and not the operator's time waiting out
    # registryRequestTimeout.
    FLAGONLY_SEED=55 # outside schema.yaml's seeds, as in the step above
    stale=$(manual_builds "$CH_FLAGONLY" "$FLAGONLY_SEED") ||
      fail "could not read the build list (cmgrd-cli system-dump) to look for a leftover probe build"
    for b in $stale; do
      note "destroying build $b of $CH_FLAGONLY seed $FLAGONLY_SEED, left behind by an earlier run"
      cmgrd-cli destroy "$b" || fail "could not destroy the leftover probe build $b of $CH_FLAGONLY"
    done
    # The flag-only challenge because it is never launched and nothing else
    # refers to it, and a seed of its own because destroyImages keeps the
    # images of content another build still names (contentReferenced,
    # cmgr/database_builds.go:224): with the schema's seed the destroy would
    # untag nothing and assert nothing.
    out=$(cmgrd-cli build --flag-format 'e2e{%s}' "$CH_FLAGONLY" "$FLAGONLY_SEED" 2>&1) ||
      fail "cmgrd-cli build $CH_FLAGONLY $FLAGONLY_SEED failed: $out"
    PROBE_BUILD=$(awk 'NF == 1 && $1 ~ /^[0-9]+$/ {print $1; exit}' <<<"$out")
    [[ "$PROBE_BUILD" =~ ^[0-9]+$ ]] || fail "cmgrd-cli build printed no build id: $out"
    fo_build=$PROBE_BUILD
    fo_meta=$(api GET "/builds/$fo_build")
    fo_tag=""
    for host in $(jq -r '.images[].host' <<<"$fo_meta"); do
      if [[ "$host" == builder ]]; then continue; fi
      fo_tag=$(image_tag "$fo_meta" "$host")
    done
    [[ -n "$fo_tag" ]] || fail "build $fo_build of $CH_FLAGONLY has no pushed image"
    # Both negative controls: without them the assertions below would pass on
    # a build that was never tagged anywhere.
    has_line "$fo_tag" registry_tags "$CH_FLAGONLY" ||
      fail "build $fo_build did not push $E2E_REGISTRY/$CH_FLAGONLY:$fo_tag, so there is no tag for the destroy to leak"
    # There is no second control on the builder to make: cork dropped its copy
    # at push, so there is no local untag left to assert and the step rests on
    # its registry half -- which is its actual subject anyway: a destroy that
    # cannot reach the registry still answers 204, fast, and leaks exactly one
    # recoverable tag.
    if has_line "$E2E_REGISTRY/$CH_FLAGONLY:$fo_tag" builder_tags; then
      fail "build $fo_build is tagged on the builder: it should have been dropped at push (cmgr/purge.go)"
    fi

    zot=$(compose_container zot)
    [[ -n "$zot" ]] || fail "compose container for the registry (service zot) not found"
    # Stopped, not paused. A paused registry accepts the delete's connection
    # and answers nothing, so the call burns the whole 30s registryRequestTimeout
    # (cmgr/registry.go:15) and this step would be timing that constant instead
    # of the contract; a stopped container takes its $E2E_REGISTRY alias with
    # it, so cork's delete fails on the name at once. That the operation
    # survives a *fast* failure without retrying it into a hang is what the
    # bound below is for.
    STOPPED_REGISTRY=$zot
    outer_ctl "$zot" stop
    # Really unreachable before the delete is issued: a delete that quietly
    # succeeded against a still-running registry would make the leak
    # assertion below fail for a reason that has nothing to do with cork.
    registry_dead() { ! quiet registry_api /v2/; }
    retry 15 "the registry to stop answering" registry_dead
    t=$(date +%s)
    code=$(api_status DELETE "/builds/$fo_build")
    took=$(( $(date +%s) - t ))
    [[ "$code" == 204 ]] ||
      fail "destroying build $fo_build with the registry down answered HTTP $code: the registry untag is best effort by contract (cmgr/registry.go:57-70) and must never fail the operation around it"
    PROBE_BUILD="" # cork no longer has the row; only the leaked tag is left
    # 20s rather than the ~10 the contract would allow: what must be caught is
    # the 30s registryRequestTimeout being waited out (or retried into a hang),
    # and a name lookup that has to be refused by an upstream resolver can
    # cost seconds of its own on a box with no working DNS.
    (( took < 20 )) ||
      fail "the destroy took ${took}s with the registry down: a registry call that cannot even connect must not be waited out (registryRequestTimeout is 30s) or retried"
    [[ "$(api_status GET "/builds/$fo_build")" == 404 ]] ||
      fail "build $fo_build is still known to cmgrd after a 204 destroy"
    if has_line "$E2E_REGISTRY/$CH_FLAGONLY:$fo_tag" builder_tags; then
      fail "the destroy left $fo_tag tagged on the builder although the local removal has nothing to do with the registry (cmgr/docker.go:1378-1392); a second build sharing this challenge, seed and checksum would explain it too, i.e. a killed earlier run left its seed-$FLAGONLY_SEED build behind"
    fi

    retry 20 "the registry container to start" quiet outer_ctl "$zot" start
    STOPPED_REGISTRY=""
    retry 60 "the registry to answer again" quiet registry_api /v2/
    # The documented leak, asserted as documented: the tag is still there.
    has_line "$fo_tag" registry_tags "$CH_FLAGONLY" ||
      fail "$fo_tag is gone from the registry although the destroy could not reach it: something other than registryDeleteTag removed it, and the leak this contract trades for is no longer the one operators are told about"
    # And recoverable by hand with cork's own identity, which is the other
    # half of "best effort" -- and what puts the registry back the way the
    # teardown step expects to find it.
    [[ "$(registry_status "/v2/$CH_FLAGONLY/manifests/$fo_tag" -X DELETE)" == 202 ]] ||
      fail "the leaked tag $fo_tag could not be deleted by hand: the leak is not recoverable"
    if has_line "$fo_tag" registry_tags "$CH_FLAGONLY"; then
      fail "$fo_tag is still listed after its delete returned 202"
    fi
    ok "a destroy with the registry stopped returned 204 in ${took}s, removed the build and its builder tag, and left exactly one recoverable tag behind in the registry"
  else
    skip "registry down: needs the outer docker socket"
  fi
else
  deselect "registry down: a destroy still succeeds and untags locally, leaking only the registry tag"
fi

# ------------- 15e. a second schema, and the multi-container delivery shape

if (( FULL )); then
  step "a second schema alongside the first: its multi-container challenge launches two networked containers, publishes only the front box, gives each stage its own limits, and keeps its private builder stage out of the registry"
  # Everything else in this scenario is one schema of single-container
  # challenges. Three promises are only testable here.
  #
  # The first is that a schema is not the unit of exclusion: production runs a
  # schema per event, and adding one must not disturb the builds or instances
  # of another (convergeSchema locks the schema it is given, cmgr/api.go:513).
  #
  # The second is the `# LAUNCH` directive: two build stages become two
  # containers on the instance's one cmgr-<id> network, each answering to its
  # stage name (Aliases, cmgr/docker.go:1130), while the `builder` stage that
  # mints the secrets is neither launched nor pushed (cmgr/docker.go:634). A
  # regression that published it would hand every competitor the flag.
  #
  # The third is `overrides:`, which replaces rather than merges
  # (cmgr/docker.go:1048, and examples/specification.md says so in as many
  # words): `work` must come up with its own 0.5 CPU and the back box with the
  # top-level 0.25. Nothing else here launches an instance whose containers
  # carry different limits, so a change that applied opts[""] to every host
  # would pass every other step.
  MULTI_SCHEMA=e2e-multi
  CH_MULTI=cmgr/examples/aptitude-and-privileges
  MULTI_FRONT=work           # the stage with the published port
  MULTI_BACK=randomDnsName   # the stage holding the flag, reachable only from work
  MULTI_PRIVATE=builder      # mints the secrets; never launched, never pushed

  # Reads the flag off the back box the way a solver would once they have
  # Alice's key, through the front box and over the instance's own network.
  # It goes through the challenge's own ssh_config, so the hostname resolved
  # is the stage-name alias cmgr assigns rather than an address this harness
  # worked out for itself. Sets `got`; retried because sshd on two freshly
  # started containers is not up the instant the launch returns.
  multi_flag_over_ssh() {
    # Tty:true puts the command's stderr on the same stream as its stdout, so
    # the flag is extracted from the output rather than being assumed to be all
    # of it: an ssh banner or a warning would otherwise fail this for the wrong
    # reason. grep failing (pipefail) is the retry signal.
    got=$(exec_in "$MULTI_WORKER" "${MULTI_CID[$MULTI_FRONT_K]}" asmith \
      ssh -F /home/asmith/.ssh/config -i /home/asmith/.ssh/id_ed25519 \
      -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=5 -o LogLevel=ERROR home cat flag.txt |
      grep -o 'e2e{[^}]*}' | head -1)
    [[ -n "$got" ]]
  }

  # A run killed before its EXIT trap leaves the schema behind, and add-schema
  # errors if it already exists.
  if has_line "$MULTI_SCHEMA" cmgrd-cli list-schemas; then
    note "removing schema $MULTI_SCHEMA, left behind by an earlier run"
    cmgrd-cli remove-schema "$MULTI_SCHEMA" || fail "could not remove the leftover schema $MULTI_SCHEMA"
  fi

  before_builds=$(cmgrd-cli system-dump | jq -r '[.[] | .builds[]?.id] | sort | join(" ")')
  t=$(date +%s)
  cmgrd-cli add-schema "$E2E_SCHEMA_FULL" || fail "add-schema $E2E_SCHEMA_FULL failed"
  MULTI_SCHEMA_ADDED=$MULTI_SCHEMA
  note "the second schema converged in $(( $(date +%s) - t ))s"

  has_line "$MULTI_SCHEMA" cmgrd-cli list-schemas || fail "schema $MULTI_SCHEMA is not listed after add-schema"
  has_line "$SCHEMA_NAME" cmgrd-cli list-schemas ||
    fail "schema $SCHEMA_NAME disappeared when $MULTI_SCHEMA was added: a converge must lock and rebuild its own schema only"
  # The first schema's work is untouched: its build still answers and the
  # persistent instance it started is still on its worker.
  [[ "$(api_status GET "/builds/$OD_BUILD")" == 200 ]] ||
    fail "build $OD_BUILD of schema $SCHEMA_NAME did not survive the converge of $MULTI_SCHEMA"
  pmeta=$(api GET "/instances/$PERSIST_INST") ||
    fail "the persistent instance of schema $SCHEMA_NAME did not survive the converge of $MULTI_SCHEMA"
  assert_on_worker "$(jq -r .worker <<<"$pmeta")" "$pmeta"

  MULTI_STATE=$(api GET "/schemas/$MULTI_SCHEMA")
  MULTI_BUILD=$(jq -r --arg id "$CH_MULTI" '.[] | select(.id == $id) | .builds[0].id' <<<"$MULTI_STATE")
  [[ "$MULTI_BUILD" =~ ^[0-9]+$ ]] || fail "$CH_MULTI has no build in schema $MULTI_SCHEMA"
  after_builds=$(cmgrd-cli system-dump | jq -r '[.[] | .builds[]?.id] | sort | join(" ")')
  [[ " $after_builds " == *" $MULTI_BUILD "* ]] ||
    fail "the build list does not contain $MULTI_BUILD after adding schema $MULTI_SCHEMA: '$after_builds'"
  for b in $before_builds; do
    [[ " $after_builds " == *" $b "* ]] ||
      fail "build $b vanished when schema $MULTI_SCHEMA was added: a converge must touch no other schema's builds (was '$before_builds', now '$after_builds')"
  done

  MULTI_META=$(api GET "/builds/$MULTI_BUILD")
  MULTI_FLAG=$(jq -r .flag <<<"$MULTI_META")
  [[ "$MULTI_FLAG" == e2e{* ]] || fail "build $MULTI_BUILD has no e2e flag: '$MULTI_FLAG'"
  # finalize.py writes the per-build account into metadata.json, which is what
  # the problem.md template looks up. A multi-container build that published
  # no lookup data would render a challenge nobody could log in to.
  for k in username password; do
    [[ -n "$(jq -r --arg k "$k" '.lookup_data[$k] // empty' <<<"$MULTI_META")" ]] ||
      fail "build $MULTI_BUILD published no '$k' in its lookup data, although finalize.py writes one"
  done

  # The private stage is built like the others -- buildImages appends an Image
  # for every host (cmgr/docker.go:697) -- but it is local to the builder
  # daemon: the push skips it (publishImages, cmgr/docker.go) and so does the launch
  # (cmgr/api.go:353). So all three stages are in the build...
  hosts=$(jq -r '[.images[].host] | sort | join(" ")' <<<"$MULTI_META")
  for host in "$MULTI_FRONT" "$MULTI_BACK" "$MULTI_PRIVATE"; do
    [[ "$(jq -r --arg h "$host" '[.images[].host] | index($h) != null' <<<"$MULTI_META")" == true ]] ||
      fail "build $MULTI_BUILD has no image for stage $host (it has: $hosts)"
  done
  [[ "$(jq -r '.images | length' <<<"$MULTI_META")" == 3 ]] ||
    fail "build $MULTI_BUILD has $(jq -r '.images | length' <<<"$MULTI_META") images ($hosts), expected exactly the three stages of $CH_MULTI"
  multi_tags=$(registry_tags "$CH_MULTI" | sort | tr '\n' ' ' | sed 's/ $//')
  for host in "$MULTI_FRONT" "$MULTI_BACK"; do
    has_line "$(image_tag "$MULTI_META" "$host")" registry_tags "$CH_MULTI" ||
      fail "the $host image of build $MULTI_BUILD was not pushed to the registry (tags: $multi_tags)"
  done
  # ...and exactly those two are in the registry. This is the assertion that
  # matters: the private stage carries this build's flag and Alice's private
  # key in plain text, and every worker holds a credential that can pull.
  if has_line "$(image_tag "$MULTI_META" "$MULTI_PRIVATE")" registry_tags "$CH_MULTI"; then
    fail "the private '$MULTI_PRIVATE' stage of build $MULTI_BUILD reached the registry: it carries this build's flag and ssh key in plain text and every worker can pull it"
  fi
  [[ "$(registry_tags "$CH_MULTI" | grep -c .)" == 2 ]] ||
    fail "the registry holds $(registry_tags "$CH_MULTI" | grep -c .) tag(s) of $CH_MULTI, expected the 2 launched stages: $(registry_tags "$CH_MULTI" | tr '\n' ' ')"

  # Only the front box publishes: `# PUBLISH 22 AS ssh` sits under the work
  # stage alone, so the instance's port map has exactly one entry.
  minst=$(launch "$MULTI_BUILD" multi-1 e2e) || fail "launching build $MULTI_BUILD failed"
  MULTI_INST=$(jq -r .id <<<"$minst")
  track "$minst" multi-1
  MULTI_WORKER=$(jq -r .worker <<<"$minst")
  mpub=$(jq -r .worker_public <<<"$minst")
  [[ "$(jq -r '.ports | keys | join(" ")' <<<"$minst")" == ssh ]] ||
    fail "instance $MULTI_INST published ports $(jq -c .ports <<<"$minst"), expected exactly one named ssh: only the $MULTI_FRONT stage carries a PUBLISH directive"
  mport=$(jq -r .ports.ssh <<<"$minst")
  (( mport >= PORT_LOW && mport <= PORT_HIGH )) || fail "instance $MULTI_INST got host port $mport, outside CMGR_PORTS $PORT_LOW-$PORT_HIGH"
  [[ "$(jq -r '.containers | length' <<<"$minst")" == 2 ]] ||
    fail "instance $MULTI_INST has $(jq -r '.containers | length' <<<"$minst") container(s), expected 2: the '# LAUNCH $MULTI_FRONT $MULTI_BACK' directive names two stages"
  assert_on_worker "$MULTI_WORKER" "$minst"

  # Which container is which, and what each was given. The stage name is the
  # container's hostname (cConfig.Hostname, cmgr/docker.go:1025) and its alias
  # on the instance's network.
  declare -A MULTI_CID=()
  declare -A MULTI_INSPECT=()
  for cid in $(jq -r '.containers[]' <<<"$minst"); do
    cjson=$(worker_api "$MULTI_WORKER" "/containers/$cid/json") ||
      fail "could not inspect container $cid of instance $MULTI_INST on $MULTI_WORKER"
    # Lower-cased throughout: the back stage's name is deliberately mixed case
    # and docker is free to normalise a hostname or an alias, so nothing here
    # depends on which case comes back.
    host=$(jq -r .Config.Hostname <<<"$cjson" | tr 'A-Z' 'a-z')
    MULTI_CID[$host]=$cid
    MULTI_INSPECT[$host]=$cjson
    # Exactly one network, and it is the instance's own: a container that also
    # sat on the default bridge would reach the registry, the workers' own
    # subnet and every other instance on the box.
    nets=$(jq -r '.NetworkSettings.Networks | keys | join(" ")' <<<"$cjson")
    [[ "$nets" == "cmgr-$MULTI_INST" ]] ||
      fail "container $host of instance $MULTI_INST is on networks '$nets', expected only cmgr-$MULTI_INST"
    [[ "$(jq -r --arg h "$host" --arg n "cmgr-$MULTI_INST" '(.NetworkSettings.Networks[$n].Aliases // []) | map(ascii_downcase) | index($h) != null' <<<"$cjson")" == true ]] ||
      fail "container $host of instance $MULTI_INST does not answer to '$host' on cmgr-$MULTI_INST: the other container reaches it by that name alone"
    # problem.md sets pidslimit 50 and init true at the top level, and the
    # work override repeats both. Whichever set the container took, they hold.
    [[ "$(jq -r .HostConfig.PidsLimit <<<"$cjson")" == 50 ]] ||
      fail "container $host of instance $MULTI_INST got PidsLimit $(jq -r .HostConfig.PidsLimit <<<"$cjson"), expected 50"
    [[ "$(jq -r .HostConfig.Init <<<"$cjson")" == true ]] ||
      fail "container $host of instance $MULTI_INST was not given an init process, although its challenge options ask for one"
  done
  # The lower-cased stage names, for indexing the two maps above.
  MULTI_FRONT_K=$(printf '%s' "$MULTI_FRONT" | tr 'A-Z' 'a-z')
  MULTI_BACK_K=$(printf '%s' "$MULTI_BACK" | tr 'A-Z' 'a-z')
  for host in "$MULTI_FRONT_K" "$MULTI_BACK_K"; do
    [[ -n "${MULTI_CID[$host]:-}" ]] ||
      fail "instance $MULTI_INST has no container for stage $host (hostnames seen: ${!MULTI_CID[*]})"
  done
  # The override replaces rather than merges: 0.5 for work, and the back box
  # keeps the top-level 0.25. NanoCpus, not NanoCPUs -- that is the Engine
  # API's spelling, whatever the Go field is called.
  front_cpu=$(jq -r .HostConfig.NanoCpus <<<"${MULTI_INSPECT[$MULTI_FRONT_K]}")
  back_cpu=$(jq -r .HostConfig.NanoCpus <<<"${MULTI_INSPECT[$MULTI_BACK_K]}")
  [[ "$front_cpu" == 500000000 ]] ||
    fail "the $MULTI_FRONT container of instance $MULTI_INST got NanoCpus $front_cpu, expected 500000000 (its 'overrides: work: cpus: 0.5')"
  [[ "$back_cpu" == 250000000 ]] ||
    fail "the $MULTI_BACK container of instance $MULTI_INST got NanoCpus $back_cpu, expected 250000000: it has no override, so it keeps the top-level 'cpus: 0.25'. An override must replace one host's options, not every host's"
  # Only the front box is bound to a host port. The back box holds the flag.
  [[ "$(jq -r '.HostConfig.PortBindings | length' <<<"${MULTI_INSPECT[$MULTI_BACK_K]}")" == 0 ]] ||
    fail "the $MULTI_BACK container of instance $MULTI_INST is bound to a host port: it holds the flag and must be reachable only from $MULTI_FRONT"
  [[ "$(jq -r '.HostConfig.PortBindings["22/tcp"][0].HostPort' <<<"${MULTI_INSPECT[$MULTI_FRONT_K]}")" == "$mport" ]] ||
    fail "the $MULTI_FRONT container of instance $MULTI_INST is not bound to host port $mport, the port cmgrd handed the platform"
  note "instance $MULTI_INST on $mpub: $MULTI_FRONT published at :$mport ($front_cpu nanocpus), $MULTI_BACK unpublished ($back_cpu nanocpus), both on cmgr-$MULTI_INST"

  # The front box answers on its published port, as a competitor would find it.
  retry 60 "sshd on $mpub:$mport (the $MULTI_FRONT container of instance $MULTI_INST)" \
    tcp_says "$mpub" "$mport" '' 'SSH-2.0'

  # And the flag really is on the other box, reachable only across the
  # instance's own network with the key this build minted. One call for the
  # whole multi-container contract: DNS by stage name, a second container that
  # was launched and started, and material copied out of the private stage
  # into both runtime images.
  got=""
  retry 90 "the flag to come back from $MULTI_BACK through $MULTI_FRONT on instance $MULTI_INST" multi_flag_over_ssh
  [[ "$got" == "$MULTI_FLAG" ]] ||
    fail "the flag on $MULTI_BACK is '$got', but cmgrd reports '$MULTI_FLAG' for build $MULTI_BUILD"

  # Teardown of a multi-container instance: both containers and the network.
  multi_done=$MULTI_INST
  [[ "$(api_status DELETE "/instances/$MULTI_INST")" == 204 ]] || fail "stopping instance $MULTI_INST failed"
  forget "$MULTI_INST"
  assert_gone "$MULTI_INST" "$MULTI_WORKER" "$minst"
  MULTI_INST=""

  cmgrd-cli remove-schema "$MULTI_SCHEMA" || fail "remove-schema $MULTI_SCHEMA failed"
  MULTI_SCHEMA_ADDED=""
  if has_line "$MULTI_SCHEMA" cmgrd-cli list-schemas; then fail "schema $MULTI_SCHEMA is still listed after remove-schema"; fi
  has_line "$SCHEMA_NAME" cmgrd-cli list-schemas ||
    fail "removing schema $MULTI_SCHEMA took schema $SCHEMA_NAME with it"
  [[ "$(api_status GET "/builds/$MULTI_BUILD")" == 404 ]] || fail "build $MULTI_BUILD survived the removal of its schema"
  [[ "$(api_status GET "/builds/$OD_BUILD")" == 200 ]] ||
    fail "build $OD_BUILD of schema $SCHEMA_NAME did not survive the removal of $MULTI_SCHEMA"
  for host in "$MULTI_FRONT" "$MULTI_BACK"; do
    if has_line "$(image_tag "$MULTI_META" "$host")" registry_tags "$CH_MULTI"; then
      fail "the $host image of build $MULTI_BUILD is still in the registry after its schema was removed"
    fi
  done
  # And on the builder, which is the only place the private stage ever
  # existed: it is never pushed, so the registry check above cannot see it,
  # and it carries this build's flag and Alice's private key in plain text.
  for host in "$MULTI_FRONT" "$MULTI_BACK" "$MULTI_PRIVATE"; do
    if has_line "$E2E_REGISTRY/$CH_MULTI:$(image_tag "$MULTI_META" "$host")" builder_tags; then
      fail "the $host image of build $MULTI_BUILD is still on the builder after its schema was removed; destroyImages left it behind$([[ "$host" == "$MULTI_PRIVATE" ]] && printf ' -- and that stage holds the flag and the ssh key in plain text')"
    fi
  done
  ok "schema $MULTI_SCHEMA converged alongside $SCHEMA_NAME without touching its builds or its persistent instance; $CH_MULTI shipped two runtime images and kept '$MULTI_PRIVATE' out of the registry; instance $multi_done ran $MULTI_FRONT (0.5 cpu, published at $mpub:$mport) and $MULTI_BACK (0.25 cpu, unpublished) on one cmgr network and returned its flag over ssh from $MULTI_BACK through $MULTI_FRONT; the stop removed both containers and the network, and remove-schema left $SCHEMA_NAME whole"
else
  deselect "a second schema and the multi-container delivery shape"
fi

# ------------------------------ 15f. losing the race for the write lock

if (( FULL )); then
  step "database busy: a launch and a stop that lose the race for SQLite's write lock are answered as retryable 503s, and the same requests go through once it is free"
  # The only coverage of ErrDatabaseBusy in either handler
  # (cmd/cmgrd/main.go:449 for a launch, :571 for a stop). cmgrd opens the
  # database with _busy_timeout=100 (cmgr/database.go:246), so a writer
  # holding the lock for longer than that is exactly the burst that timeout is
  # sized against; every write on the launch and stop paths goes through
  # retryableDB (cmgr/database_instances.go:20), and the platform's celery
  # workers retry a 503 and fail a 500.
  #
  # This is the one step that reaches behind the API: nothing cmgrd exposes
  # can hold its write lock, so it is held from outside with sqlite3 against
  # the same file, which is why cork-data is mounted into this container.
  CMGRD_DB=/var/lib/cork/cmgr.db
  # Can another writer take the lock right now? sqlite3's CLI has no busy
  # timeout of its own, so BEGIN IMMEDIATE either takes it at once or says it
  # is locked.
  db_write_free() { quiet sqlite3 "$CMGRD_DB" 'BEGIN IMMEDIATE; ROLLBACK;'; }
  db_write_locked() { ! db_write_free; }
  # Every instance cmgrd records, of every build of every challenge. Counted
  # from the system dump rather than from the workers, so an instance whose
  # worker has left the fleet is still counted.
  instance_rows() { cmgrd-cli system-dump | jq -r '[.[] | .builds[]?.instances[]?] | length'; }
  if command -v sqlite3 >/dev/null 2>&1 && [[ -w "$CMGRD_DB" ]]; then
    dbtmp=$(mktemp -d)
    # ONE process holds the lock, fed through a fifo this shell keeps open.
    #
    # Not `( printf ...; sleep N ) | sqlite3 &`: $! there names sqlite3, but
    # bash's `wait <pid>` waits for the JOB, and the job also holds the
    # sleeping subshell -- which nothing signals and which never writes to the
    # closed pipe again, so it takes no SIGPIPE. Waiting on it idled out the
    # backstop here; NOT waiting on it let it survive to the teardown's bare
    # `wait`, where it was charged to the concurrent-stop timing and tripped
    # the leaked-slot guard. Neither is fixable while a second process exists.
    #
    # With a fifo, sqlite3 is the only process and the lock is released on
    # EOF, which arrives when the descriptor below is closed -- or when this
    # shell dies, which makes the backstop automatic rather than a timer.
    #
    # .timeout so it waits for the lock rather than being refused outright:
    # db_write_locked probes by taking the very same lock, and without this the
    # probe could win the race and leave this BEGIN IMMEDIATE rejected, with
    # nothing holding the lock for the rest of the step.
    lock_fifo="$dbtmp/lock.fifo"
    mkfifo "$lock_fifo" || fail "could not create the fifo feeding the stand-in writer"
    sqlite3 "$CMGRD_DB" <"$lock_fifo" >/dev/null 2>&1 &
    DB_LOCK_PID=$!
    exec {DB_LOCK_FD}>"$lock_fifo"
    printf '.timeout 5000\nBEGIN IMMEDIATE;\n' >&"$DB_LOCK_FD"
    retry 10 "the write lock on $CMGRD_DB to be held by the stand-in writer" db_write_locked

    before_inst=$(instance_rows)
    [[ "$before_inst" =~ ^[0-9]+$ ]] || fail "could not count the instance records before the refused launch: '$before_inst'"
    timed_launch "$dbtmp" busy "$OD_BUILD" "$(jq -cn '{user_id: "db-busy", env: {CUSTOM_VAR: "e2e"}}')"
    read -r code _ <"$dbtmp/busy.code"
    [[ "$code" == 503 ]] ||
      fail "a launch that lost the race for the write lock answered HTTP $code, not 503: $(head -c 300 "$dbtmp/busy.body"). ErrDatabaseBusy is retryable, and the platform retries a 503 and fails a 500 (cmd/cmgrd/main.go:449)"
    grep -qi '^retry-after:' "$dbtmp/busy.head" ||
      fail "the 503 for a launch that lost the write lock carried no Retry-After: $(tr -d '\r' <"$dbtmp/busy.head" | head -20)"
    grep -qi 'database busy' "$dbtmp/busy.body" ||
      fail "the 503 body does not name the database: $(head -c 300 "$dbtmp/busy.body"). A launch refused as overloaded or busy is reported the same way, and this step would be asserting nothing"
    # openInstance is the first write on the launch path and it runs before
    # anything reaches a daemon (cmgr/api.go:337), so a launch refused here
    # left no record and no container to reap.
    after_inst=$(instance_rows)
    [[ "$after_inst" == "$before_inst" ]] ||
      fail "the launch refused for a busy database left an instance record behind: $before_inst before, $after_inst after"

    # A stop loses the same race and is answered the same way. One request for
    # status, headers and body: a second DELETE would be a different race.
    busy_id=${OD_IDS[0]}
    busy_meta=${INST_META[$busy_id]}
    curl -sS --connect-timeout 5 --max-time "$API_TIMEOUT" \
      -o "$dbtmp/stop.body" -D "$dbtmp/stop.head" -w '%{http_code}' \
      -X DELETE "$CMGRD_SERVER/instances/$busy_id" >"$dbtmp/stop.code" 2>/dev/null || true
    code=$(cat "$dbtmp/stop.code")
    [[ "$code" == 503 ]] ||
      fail "a stop that lost the race for the write lock answered HTTP $code, not 503: $(head -c 300 "$dbtmp/stop.body") (cmd/cmgrd/main.go:571)"
    grep -qi '^retry-after:' "$dbtmp/stop.head" ||
      fail "the 503 for a stop that lost the write lock carried no Retry-After"

    # Closing the write end is what releases the lock: sqlite3 reads EOF and
    # exits, rolling the open transaction back. wait is safe now -- there is
    # exactly one process in this job and it is on its way out.
    exec {DB_LOCK_FD}>&-
    DB_LOCK_FD=""
    wait "$DB_LOCK_PID" 2>/dev/null || true
    DB_LOCK_PID=""
    retry 15 "the write lock on $CMGRD_DB to be free again" db_write_free

    # A refused stop must leave the instance still RECORDED, whatever it did
    # to the containers: that is what makes it retryable rather than a
    # half-completed teardown the platform can never finish. Checked before
    # the retry, because afterwards there is nothing left to distinguish.
    [[ "$(api_status GET "/instances/$busy_id")" == 200 ]] ||
      fail "the stop refused for a busy database removed instance $busy_id's record anyway: a 503 must leave the platform something to retry"

    # The retry the platform would make, for both verbs.
    inst=$(launch "$OD_BUILD" db-busy e2e) ||
      fail "the launch retried after the write lock was released still failed: a retryable 503 must mean the same request goes through moments later"
    track "$inst" db-busy
    assert_on_worker "$(jq -r .worker <<<"$inst")" "$inst"
    [[ "$(api_status DELETE "/instances/$busy_id")" == 204 ]] ||
      fail "the stop retried after the write lock was released still failed"
    # Records only. The containers may already have gone with the refused
    # attempt, so their absence is not evidence about this request.
    [[ "$(api_status GET "/instances/$busy_id")" == 404 ]] ||
      fail "instance $busy_id is still known to cmgrd after its stop was retried"
    assert_gone_from_worker "${INST_WORKER[$busy_id]}" "$busy_id" "$busy_meta"
    forget "$busy_id"
    rm -rf "$dbtmp"
    ok "a launch and a stop that lost the race for the write lock were both 503 + Retry-After naming the database, the refused launch left no record and reached no daemon, and both requests went through unchanged once the lock was released"
  else
    skip "database busy: needs sqlite3 in this image and cmgrd's database mounted here (see e2e/cork.Dockerfile and the cork-data mount in e2e/compose.yaml)"
  fi
else
  deselect "database busy: a launch and a stop that lose the write lock are retryable 503s"
fi


# ------------------------------- 15g. base image pins (full mode only)

if (( FULL )); then
step "base image pins: every base the corpus names resolves to a digest once, FROM is rewritten in the build context and never on disk, and a rebuild through the pins still builds and pushes"
# Pinning is the only reason cork can build on BuildKit without asking Docker
# Hub what ubuntu:24.04 means on essentially every build: it rewrites FROM to
# the digest in the tar it hands the daemon, leaving several hundred challenge
# Dockerfiles untouched (cmgr/basepins.go). The rewriting itself is unit
# tested; what only a fleet can show is that a real build through a rewritten
# context still produces an image the registry accepts. The step after this
# one takes it further and has a worker serve it.
#
# Placed late on purpose. A refresh changes the pin fingerprint, and that is
# folded into every build's content identity (contentChecksum, cmgr/docker.go),
# so a generation built after this step is tagged differently from the same
# source built before it. The steps that compare generations against each other
# must not straddle that boundary.
[[ "$(api GET /pins | jq -r '.pins | length')" == 0 ]] ||
  fail "the pin map is already populated before pin-refresh: this step's before-and-after reading of CMGR_BASE_PINS proves nothing"
rc=0
pins_out=$(cmgrd-cli pin-refresh 2>&1) || rc=$?
sed 's/^/       /' <<<"$pins_out"
# Partial resolution is the expected outcome here, and it is the half worth
# pinning: examples/disks builds on cmgr/examples-guestfish-base, which the
# example's own tooling builds locally and no registry serves, so
# DistributionInspect cannot answer for it. What must hold is that the refusal
# is reported AND the references that did resolve are still written and in
# force. A refresh that discarded the lot over one unresolvable base would be
# unusable on any corpus that carries a locally built base.
#
# Conditional on the refusal actually happening, rather than asserting it: the
# name is one Docker Hub could start serving, and a step that breaks because
# somebody registered cmgr/examples-guestfish-base would be pinning another
# registry's contents. Whichever way it goes, the map has to agree with it.
pins=$(api GET /pins)
if (( rc != 0 )); then
  grep -q 'cmgr/examples-guestfish-base' <<<"$pins_out" ||
    fail "pin-refresh reported a failure without naming the base it could not resolve: an operator cannot act on that"
  jq -e '[.pins[] | select(.ref == "cmgr/examples-guestfish-base")] | length == 0' >/dev/null <<<"$pins" ||
    fail "cmgr/examples-guestfish-base reached the pin map although it could not be resolved: an unresolved base must not be written"
  note "cmgr/examples-guestfish-base is served by no registry: reported, left unpinned, and the bases that did resolve were written anyway"
else
  note "every base resolved, including cmgr/examples-guestfish-base: the partial-resolution path was not exercised this run"
fi
ubuntu_digest=$(jq -r '.pins[] | select(.ref == "ubuntu:24.04") | .digest' <<<"$pins")
[[ "$ubuntu_digest" == sha256:* ]] ||
  fail "ubuntu:24.04 is not pinned to a sha256 digest after a refresh (got '${ubuntu_digest:-nothing}'), so the examples still build on a mutable tag"
(( $(jq -r '.pins[] | select(.ref == "ubuntu:24.04") | .in_use' <<<"$pins") >= 1 )) ||
  fail "ubuntu:24.04 is pinned but reported as used by no challenge: the corpus scan and the pin map disagree"
jq -e '[.pins[] | select(.digest | startswith("sha256:") | not)] | length == 0' >/dev/null <<<"$pins" ||
  fail "the pin map holds an entry that is not a sha256 digest; cmgrd refuses to start on such a file, so this one would not survive a restart"
note "pinned ubuntu:24.04 to $ubuntu_digest, $(jq -r '.pins | length' <<<"$pins") reference(s) in the map"

# A rebuild with the pins in force. The build context every challenge is built
# from now carries a rewritten FROM, so this is the first build in the run whose
# Dockerfile the daemon sees is not the one on disk.
#
# Two challenges come back rebuilt, not one, and that is correct: the failed-
# rebuild step above restores the persistent source to the generation it found
# without running an update, so this update absorbs that restore as well as the
# edit below. Hence no "the persistent challenge must not have been rebuilt"
# assertion here, unlike the earlier update steps.
OD_BUILD_UNPINNED=$(api GET "/builds/$OD_BUILD")
set_generation 6 ondemand
rc=0
out=$(cmgrd-cli update --prune-old) || rc=$?
sed 's/^/       /' <<<"$out"
(( rc == 0 )) ||
  fail "cmgrd-cli update exited $rc rebuilding $CH_ONDEMAND with base image pins in force: a rewritten build context must build exactly as the file on disk did"
grep -q "  $CH_ONDEMAND$" <<<"$out" || fail "$CH_ONDEMAND was not rebuilt with pins in force"
OD_BUILD_PINNED=$(api GET "/builds/$OD_BUILD")
[[ "$(jq -r .checksum <<<"$OD_BUILD_PINNED")" != "$(jq -r .checksum <<<"$OD_BUILD_UNPINNED")" ]] ||
  fail "build $OD_BUILD kept its content checksum: generation 6 was not built, so nothing below is a statement about pinning"
OD_TAG_PINNED=$(image_tag "$OD_BUILD_PINNED" challenge)
has_line "$OD_TAG_PINNED" registry_tags "$CH_ONDEMAND" ||
  fail "$OD_TAG_PINNED is not in the registry: the image built from a pinned context was not pushed"

# The Dockerfile on disk must still read ubuntu:24.04. Rewriting the source
# would be a different feature with a different cost -- several hundred
# challenges to edit and re-review -- and the whole design rests on not doing
# it, so it is worth a line rather than an assumption.
grep -q "^FROM ubuntu:24.04 AS base" "$CHALLENGES/runtime_env_vars/Dockerfile" ||
  fail "the challenge Dockerfile on disk no longer reads 'FROM ubuntu:24.04 AS base': pinning must rewrite the build context only"

# Inline cache. cork stamps the image it pushes with BuildKit's cache metadata
# and offers that same tag back as a cache source, which is how a builder whose
# local cache was reclaimed recovers the apt and pip layers every challenge
# shares instead of re-running them once per challenge (cmgr/docker.go). It is
# invisible to everything else in this run: the key is in the pushed config or
# the recovery path is simply gone.
man=$(registry_api "/v2/$CH_ONDEMAND/manifests/$OD_TAG_PINNED" \
  -H 'Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json')
cfg=$(jq -r '.config.digest // empty' <<<"$man")
[[ "$cfg" == sha256:* ]] ||
  fail "could not read the config digest of $CH_ONDEMAND:$OD_TAG_PINNED from the registry"
cfg_blob=$(registry_api "/v2/$CH_ONDEMAND/blobs/$cfg")
jq -e 'has("moby.buildkit.cache.v0")' >/dev/null <<<"$cfg_blob" ||
  fail "the image cork pushed carries no moby.buildkit.cache.v0 key: BUILDKIT_INLINE_CACHE did not reach the build, so a builder whose cache was reclaimed would rebuild every shared layer from scratch instead of importing them"

ok "$(jq -r '.pins | length' <<<"$pins") base(s) pinned by digest and one unresolvable base reported without discarding them; generation 6 built from a rewritten context and pushed as $OD_TAG_PINNED carrying inline cache metadata; the challenge Dockerfile on disk is untouched"

# ------------------------------------ 15h. the inline cache is IMPORTED

step "inline cache import: with the builder's layer cache destroyed, a rebuild recovers the shared apt layer from the registry instead of re-running it"
# The step above proves only the publish half -- that the pushed config carries
# moby.buildkit.cache.v0. Nothing proved the import half, and the import half is
# where this was quietly broken: CacheFrom named only the tag the build was
# about to produce, and tags are content-addressed, so that tag cannot exist yet
# whenever the source changed -- which is the only reason `update` rebuilds
# anything. Every rebuild therefore asked the registry for something that could
# not be there and imported nothing. cacheRefsFor now offers the generation
# being displaced first (cmgr/docker.go).
#
# The proof is layer identity rather than timing, which would be flaky, or log
# scraping, which cork does not surface. runtime_env_vars/Dockerfile is:
#
#     1  FROM ubuntu:24.04 AS base        -> layer 1, from the registry either way
#     2  RUN apt-get install python3-pip socat  -> layer 2, THE ONE THAT MATTERS
#     3  RUN mkdir /challenge             -> layer 3
#     4  RUN echo ... metadata.json       -> layer 4
#     5  RUN echo ... flag.txt            -> layer 5
#     6  COPY server.py                   -> layer 6, changes every generation
#
# Layer 2 depends on the pinned base digest and its own RUN line, neither of
# which differs between generations. Re-running it would still produce a
# DIFFERENT digest -- an apt layer's tar carries the mtimes of the files that
# run wrote -- so an identical digest across a build that started from an empty
# cache can only have come from the registry. Layer 1 is identical either way
# and proves nothing, which is exactly why the assertion names layer 2.
OD_LAYERS_BEFORE=$(jq -r '.layers[].digest' <<<"$man")
# The comparison below is only meaningful if the layers it compares include
# ones cork BUILT. Base layers come from the registry whether or not the import
# works, so a guard of "at least two layers" would be satisfied by one base
# layer plus the COPY -- and the step would pass with the import completely
# broken. ubuntu:24.04 is a single-layer image and runtime_env_vars/Dockerfile
# adds five (two RUN in the base stage, two RUN and one COPY in the challenge
# stage). Asserted rather than assumed, so that a base image gaining a layer or
# the Dockerfile gaining an instruction fails here and is re-derived, instead of
# quietly hollowing this step out.
OD_EXPECTED_LAYERS=6
(( $(wc -l <<<"$OD_LAYERS_BEFORE") == OD_EXPECTED_LAYERS )) ||
  fail "$CH_ONDEMAND:$OD_TAG_PINNED has $(wc -l <<<"$OD_LAYERS_BEFORE") layers, expected $OD_EXPECTED_LAYERS (one for ubuntu:24.04 plus five from runtime_env_vars/Dockerfile). Re-derive this step: if the extra layers are base layers, the comparison below may be comparing nothing cork built"

# Destroy the cache. This is the condition the whole step rests on: with it
# non-empty a local hit is indistinguishable from a registry import.
prune_out=$(curl -sS --fail-with-body --connect-timeout 5 --max-time 300   -X POST "$E2E_BUILDER/build/prune?all=true") ||
  fail "could not prune the builder's build cache: $prune_out"
# --fail-with-body, and a count rather than a sum. Without the former a 404 or
# a renamed field yields an empty jq result; without the latter a cache RECORD
# whose Size is 0 still produces a cache hit, so the sum can read zero with
# usable cache present. Either way the precondition this step rests on -- that
# a hit can only have come from the registry -- would be silently unmet.
cache_left=$(curl -sS --fail-with-body --connect-timeout 5 --max-time 60 "$E2E_BUILDER/system/df" |
  jq -r '.BuildCache | length') ||
  fail "could not read the builder's build cache state from /system/df"
[[ "$cache_left" == "0" ]] ||
  fail "the builder still holds $cache_left build cache record(s) after 'build prune --all': this step cannot tell a registry import from a local cache hit unless the cache is genuinely empty"
pruned_records=$(jq -r '.CachesDeleted | length? // 0' <<<"$prune_out")
(( pruned_records > 0 )) ||
  fail "'build prune --all' deleted no cache records, so the four challenges built above left no cache to destroy and the premise of this step does not hold"
note "build cache pruned: $(jq -r '.SpaceReclaimed // 0' <<<"$prune_out") bytes reclaimed, $pruned_records records deleted"

# Rebuild. Only server.py changes, so layer 6 must move and layers 1-5 must not.
set_generation 7 ondemand
rc=0
out=$(cmgrd-cli update --prune-old) || rc=$?
sed 's/^/       /' <<<"$out"
(( rc == 0 )) ||
  fail "cmgrd-cli update exited $rc rebuilding $CH_ONDEMAND from an empty build cache"
grep -q "  $CH_ONDEMAND$" <<<"$out" || fail "$CH_ONDEMAND was not rebuilt for generation 7"
OD_BUILD_IMPORTED=$(api GET "/builds/$OD_BUILD")
OD_TAG_IMPORTED=$(image_tag "$OD_BUILD_IMPORTED" challenge)
[[ "$OD_TAG_IMPORTED" != "$OD_TAG_PINNED" ]] ||
  fail "build $OD_BUILD kept tag $OD_TAG_PINNED: generation 7 was not built, so nothing below is a statement about the cache"

man_after=$(registry_api "/v2/$CH_ONDEMAND/manifests/$OD_TAG_IMPORTED"   -H 'Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json')
OD_LAYERS_AFTER=$(jq -r '.layers[].digest' <<<"$man_after")

# Every layer but the last, rather than a hand-picked index. Picking one (the
# apt layer is instruction 2, so layer 2) quietly assumed ubuntu:24.04 ships
# exactly one layer and that nothing is ever inserted above the apt line; if
# either changed, the index would land on a BASE layer, which is identical
# whether or not the import worked, and the step would pass while cacheRefsFor
# was broken. The index-free form has neither failure and is strictly stronger:
# it covers the flag layers too, which the flag being deterministic
# (sha256(challenge:format:seed), cmgr/docker.go) makes reproducible content --
# so they can only match if they were imported rather than re-run.
n_before=$(wc -l <<<"$OD_LAYERS_BEFORE")
n_after=$(wc -l <<<"$OD_LAYERS_AFTER")
(( n_before == n_after )) ||
  fail "the image gained or lost layers over the rebuild ($n_before -> $n_after): editing server.py must not change the shape of the image, so the comparison below is not between like and like"
shared_before=$(head -n -1 <<<"$OD_LAYERS_BEFORE")
shared_after=$(head -n -1 <<<"$OD_LAYERS_AFTER")
top_before=$(tail -n1 <<<"$OD_LAYERS_BEFORE")
top_after=$(tail -n1 <<<"$OD_LAYERS_AFTER")

# The top layer must have moved, or the "rebuild" produced the same image and
# there is nothing to conclude. Note this is a weaker statement than the tag
# check above and is here for the diagnosis it gives, not for coverage.
[[ "$top_after" != "$top_before" ]] ||
  fail "the top layer is unchanged at $top_before after editing server.py: generation 7 reproduced generation 6's image, so nothing here shows a cache import"
[[ "$shared_after" == "$shared_before" ]] ||
  fail "$(printf '%s
' "the layers below the top were rebuilt rather than imported."     "  before: $(tr '
' ' ' <<<"$shared_before")"     "  after:  $(tr '
' ' ' <<<"$shared_after")"     "The build cache was pruned to zero above, so the only other source is the registry:"     "CacheFrom offered nothing that could hit. See cacheRefsFor in cmgr/docker.go, which"     "must offer the generation being displaced and not only the tag this build creates.")"

# And the image is still an image: served by a worker, with the build's flag.
inst=$(launch "$OD_BUILD" "e2e-user-40" "e2e-value-40")
track "$inst" 40
check_ondemand "$inst" 40 "E2E_GENERATION=7"
ok "from an empty build cache, generation 7 reproduced all $(( n_before - 1 )) of generation 6's lower layers byte for byte -- $(( n_before - 2 )) of them layers cork built, which only an import can explain -- and rebuilt only the one server.py changed; the image is served by $(jq -r .worker <<<"$inst")"
else
  # Every other full-mode block names what it skipped; the README promises a
  # regular run lists them, and these two were not counted.
  deselect "base image pins"
  deselect "the inline cache is imported, not just published"
fi
# -------------------------------------------------------------- 16. teardown

step "teardown: stop the remaining on-demand instances, remove the schema, check the workers, builder, and registry are clean"
# All at once rather than one at a time: the teardown semaphore is only ever
# contended here, and a bounded wait, a double release or a release that never
# happens all pass when the stops are sequential.
stop_tmp=$(mktemp -d)
t=$(date +%s)
for id in "${OD_IDS[@]}"; do
  api_status DELETE "/instances/$id" >"$stop_tmp/$id" &
done
wait
took=$(( $(date +%s) - t ))
(( took < 60 )) || fail "${#OD_IDS[@]} concurrent stops took ${took}s: a teardown slot was leaked or never released"
for id in "${OD_IDS[@]}"; do
  code=$(cat "$stop_tmp/$id")
  [[ "$code" == 204 ]] ||
    fail "the concurrent stop of instance $id answered HTTP $code: a teardown slot was refused rather than waited for"
done
rm -rf "$stop_tmp"
for id in "${OD_IDS[@]}"; do
  assert_gone "$id" "${INST_WORKER[$id]}" "${INST_META[$id]}"
done
note "${#OD_IDS[@]} instances stopped concurrently in ${took}s, none refused"
PERSIST_META=$(api GET "/instances/$PERSIST_INST")
PERSIST_WORKER=$(jq -r .worker <<<"$PERSIST_META")
cmgrd-cli remove-schema "$SCHEMA_NAME"
if has_line "$SCHEMA_NAME" cmgrd-cli list-schemas; then fail "schema $SCHEMA_NAME is still listed"; fi
[[ "$(api_status GET "/builds/$OD_BUILD")" == 404 ]] || fail "build $OD_BUILD survived the schema removal"
assert_gone "$PERSIST_INST" "$PERSIST_WORKER" "$PERSIST_META"
# Current generations of every challenge, plus the retained rollback
# generations (destroyImages untags those too).
for id in "$CH_ONDEMAND" "$CH_PERSISTENT" "$CH_MAKE" "$CH_FLAGONLY"; do
  if has_line "${IMAGE_TAG[$id]}" registry_tags "$id"; then
    fail "$E2E_REGISTRY/$id:${IMAGE_TAG[$id]} is still in the registry"
  fi
done
if has_line "$OD_TAG_G2" registry_tags "$CH_ONDEMAND"; then fail "rollback tag $OD_TAG_G2 of $CH_ONDEMAND is still in the registry"; fi
if has_line "$PERSIST_TAG_G1" registry_tags "$CH_PERSISTENT"; then fail "rollback tag $PERSIST_TAG_G1 of $CH_PERSISTENT is still in the registry"; fi
btags=$(builder_tags)
for id in "$CH_ONDEMAND" "$CH_PERSISTENT" "$CH_MAKE" "$CH_FLAGONLY"; do
  if grep -q "^$E2E_REGISTRY/$id:" <<<"$btags"; then fail "the builder still holds $id images"; fi
done
restore_sources
cmgrd-cli worker-list | sed 's/^/       /'
ok "instances gone from the workers, builds gone from cmgrd and the builder, tags gone from the registry, sources restored"

if (( SKIPPED )); then
  printf '\nPASSED WITH %d STEP(S) SKIPPED in %ds\n' "$SKIPPED" "$(( $(date +%s) - T0 ))"
  printf '  not run: %s\n' "${SKIPPED_NAMES[@]}"
  if (( DESELECTED )); then printf '  and %d full-mode step(s) this run did not ask for\n' "$DESELECTED"; fi
elif (( DESELECTED )); then
  printf '\nALL REGULAR-MODE STEPS PASSED in %ds\n' "$(( $(date +%s) - T0 ))"
  printf '  %d full-mode step(s) not run; E2E_FULL=1 (run.sh --full) adds them\n' "$DESELECTED"
else
  printf '\nALL STEPS PASSED in %ds\n' "$(( $(date +%s) - T0 ))"
fi
