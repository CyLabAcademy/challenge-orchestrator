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
#   E2E_CHAOS            1 to run the outer-socket chaos steps     1
#   E2E_FULL             1 to add the full-mode steps (run.sh --full) 0
#   E2E_COMPOSE_PROJECT  compose project name, for those steps     cork-e2e
#   E2E_IMAGE            image to run the fake telemetry from      cork-e2e/cork
#   DOCKER_CERT_PATH     cork's worker client certificates         /root/.docker_certs
set -euo pipefail

CMGRD_SERVER="${CMGRD_SERVER:-http://cork:4200}"
E2E_WORKERS="${E2E_WORKERS:-172.28.0.11=worker-a 172.28.0.12=worker-b}"
E2E_SCHEMA="${E2E_SCHEMA:-/opt/e2e/schema.yaml}"
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
PORT_LOW=20000                             # CMGR_PORTS on cork
PORT_HIGH=20029

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
cleanup() {
  local status=$? fake
  if [[ -n "$TRAFFIC_STOP" ]]; then touch "$TRAFFIC_STOP" 2>/dev/null || true; fi
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
  if [[ "$(worker_status "$1" "/networks/cmgr-$2")" == 200 ]]; then
    worker_api "$1" "/networks/cmgr-$2" -X DELETE -o /dev/null
  fi
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

# Edit the sources the update steps rebuild. The generation marker shows up
# in what each service answers, so a rebuilt image is distinguishable from
# the old one at the wire.
restore_sources() {
  cp "$CHALLENGES_SEED/$ONDEMAND_SRC" "$CHALLENGES/$ONDEMAND_SRC"
  cp "$CHALLENGES_SEED/$PERSISTENT_SRC" "$CHALLENGES/$PERSISTENT_SRC"
}
set_generation() { # set_generation <n> both|ondemand: mark the source(s) as generation n
  cp "$CHALLENGES_SEED/$ONDEMAND_SRC" "$CHALLENGES/$ONDEMAND_SRC"
  sed -i "s/response = f\"CMGR_USER_ID/response = f\"E2E_GENERATION=$1\\\\nCMGR_USER_ID/" "$CHALLENGES/$ONDEMAND_SRC"
  grep -q "E2E_GENERATION=$1" "$CHALLENGES/$ONDEMAND_SRC" || fail "could not mark $ONDEMAND_SRC"
  if [[ "$2" == both ]]; then
    cp "$CHALLENGES_SEED/$PERSISTENT_SRC" "$CHALLENGES/$PERSISTENT_SRC"
    sed -i "s/-in secret.enc'/-in secret.enc e2e-generation-$1'/" "$CHALLENGES/$PERSISTENT_SRC"
    grep -q "e2e-generation-$1" "$CHALLENGES/$PERSISTENT_SRC" || fail "could not mark $PERSISTENT_SRC"
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
  backend=$(worker_api "$ip" /info | jq -r '.FirewallBackend.Driver // "unknown"')
  [[ "$backend" == "nftables" ]] || fail "worker $ip programs its firewall with $backend, not nftables"
  sweep_worker "$ip"
done
ok "cmgrd $(api GET /version | jq -r .version), zot, builder, and $NWORKERS workers (dockerd on nftables + telemetry) answer"

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
t=$(date +%s)
for (( i = 30; i < 30 + BURST_N; i++ )); do
  # One subshell per request, each timed and captured whole. It ends on a
  # printf, so a curl that never answered cannot escape into `wait` and abort
  # the run under set -e: it is reported as the missing status it is.
  (
    s=$(date +%s)
    out=$(try_launch "$OD_BUILD" "e2e-user-$i" "e2e-value-$i") || out=$'000\ncurl did not answer'
    printf '%s\n%s\n' "$(( $(date +%s) - s ))" "$out"
  ) >"$burst_dir/$i" &
done
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
      if grep -qE "no launch slot on worker $WB for instance [0-9]+ within 2s" <<<"$body"; then
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
  fail "all $BURST_N launches on $WB were accepted, so nothing here exercised the refusal: either its two launch slots emptied faster than the burst filled them (raise BURST_N; $WB has $(( PORT_HIGH - PORT_LOW + 1 )) ports), or cork is not running with CMGR_WORKER_LAUNCH_WAIT=2s"
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
# $BURST_N-way contention) on top of the 2s wait.
(( burst_worst <= 6 )) ||
  fail "a burst launch was refused only after ${burst_worst}s: a refusal must arrive within the 2s launch wait plus overhead, never after queueing behind the daemon (cmgr/launch.go acquireSlot)"
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
LAUNCH_WAIT=2      # CMGR_WORKER_LAUNCH_WAIT on cork, set in compose.yaml
BURST_N=32         # wave one: round robin puts 16 on each of the two workers
PROBE_N=8          # wave two, fired while those queues are at their deepest
ADMIT_MAX_MS=1500  # admission decides before any I/O; the wait is 2000ms
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
note "$(( BURST_N + PROBE_N )) burst launches: $n2xx started, $nadmit refused by admission, $nslot refused after the full ${LAUNCH_WAIT}s wait, $nother other"

# The substitution is the regression, and a slot refusal is what proves the
# queue really was deeper than the wait: if one launch sat there for the whole
# wait, the launches that arrived behind it should have been turned away on
# sight. A burst that produced neither refusal never saturated anything and
# has tested nothing, which is a broken test rather than a broken cork -- so
# it says so separately, and names the knob.
if (( nadmit == 0 && nslot > 0 )); then
  fail "$nslot burst launches waited the full ${LAUNCH_WAIT}s for a slot and not one was refused ahead of it: admit (cmgr/launch.go, a53e8d6) no longer runs before openInstance, so every refusal fell back to acquireSlot's 'no launch slot on ... within' form"
fi
if (( nadmit == 0 )); then
  fail "the burst of $(( BURST_N + PROBE_N )) launches never queued deeply enough to refuse anything ($n2xx started, $nother other): the fleet worked them off faster than the estimate admit reads, so nothing was proved here. Raise BURST_N (CMGR_PORTS caps it near 25 per worker) or lower CMGR_WORKER_LAUNCH_WAIT"
fi
# Half the wait would do; ${ADMIT_MAX_MS}ms because these are 40 concurrent
# curls in one container on a box that is also starting containers, and the
# message already said which refusal each of these was. A refusal that really
# waited for a slot cannot come back under ${LAUNCH_WAIT}s.
slowest=$(printf '%s\n' "${admit_times[@]}" | jq -s -r '(. + [0] | max) * 1000 | floor' || echo "")
[[ "$slowest" =~ ^[0-9]+$ ]] ||
  fail "could not read the response times of the $nadmit admission refusals from curl: '$(printf '%s ' "${admit_times[@]}")'"
(( slowest < ADMIT_MAX_MS )) ||
  fail "the slowest of $nadmit admission refusals took ${slowest}ms against a ${LAUNCH_WAIT}s launch wait: it waited for a slot instead of being refused on the estimate"

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
ok "$nadmit of $(( BURST_N + PROBE_N )) burst launches were refused by admission in under ${ADMIT_MAX_MS}ms against a ${LAUNCH_WAIT}s wait, each naming the worker whose queue was full and carrying Retry-After; the burst consumed $consumed ids for the $(( BURST_N + PROBE_N - nadmit )) launches it let through, so a refusal cost no row, no port and no docker call"

# -------------------------------------------------------- 6. remote-make

step "remote-make challenge: an on-demand instance answers over TCP, then stops cleanly"
inst=$(api POST "/builds/$MK_BUILD")
id=$(jq -r .id <<<"$inst")
w=$(jq -r .worker <<<"$inst")
pub=$(jq -r .worker_public <<<"$inst")
port=$(jq -r .ports.socat <<<"$inst")
assert_on_worker "$w" "$inst"
retry 30 "the BinEx101 prompt at $pub:$port" tcp_says "$pub" "$port" '1\n1\n' 'Give me a number'
note "instance $id on $w: $pub:$port prompts for input"
cmgrd-cli stop "$id"
assert_gone "$id" "$w" "$inst"
ok "remote-make instance launched, answered, and was torn down on the worker"

# ------------------------------------------------------------ 7. stop path

step "stop path: DELETE /instances/<id> tears the instance down on its worker and is idempotent"
id=${OD_IDS[0]}
w=${INST_WORKER[$id]}
cmgrd-cli stop "$id"
assert_gone "$id" "$w" "${INST_META[$id]}"
[[ "$(api_status DELETE "/instances/$id")" == 204 ]] || fail "a second DELETE of instance $id was not a 204"
forget "$id"
ok "instance $id: containers and network gone from $w, second delete is a no-op"

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
btags=$(builder_tags)
if grep -qx "$E2E_REGISTRY/$CH_ONDEMAND:$OD_TAG_G1" <<<"$btags"; then fail "generation 1 image is still tagged on the builder after --prune-old"; fi
grep -qx "$E2E_REGISTRY/$CH_ONDEMAND:$OD_TAG_G3" <<<"$btags" || fail "generation 3 image is not on the builder"
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
