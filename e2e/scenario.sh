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
step() { printf '\n=== [%4ds] %s\n' "$(( $(date +%s) - T0 ))" "$*"; }
ok() { printf '  ok   %s\n' "$*"; }
note() { printf '  --   %s\n' "$*"; }
skip() { printf '  skip %s\n' "$*"; }
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
declare -A FAKES=()   # fake telemetry container id -> the sidecar it replaced
cleanup() {
  local status=$? fake
  if [[ -n "$PAUSED_WORKER" ]]; then outer_ctl "$PAUSED_WORKER" unpause >/dev/null 2>&1 || true; fi
  for fake in "${!FAKES[@]}"; do
    outer_ctl "$fake" stop >/dev/null 2>&1 || true
    outer_ctl "${FAKES[$fake]}" start >/dev/null 2>&1 || true
  done
  if [[ -n "$STOPPED_SIDECAR" ]]; then outer_ctl "$STOPPED_SIDECAR" start >/dev/null 2>&1 || true; fi
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

# down_worker <worker ip>: cmgrd-cli worker-down, verified. cmgrd's poller
# checks "already down" before it polls and stores the result after, so a
# poll in flight during worker-down can overwrite the sticky down with the
# poll's verdict (seen in cork's log as "ok -> down" then "down -> ok" within
# a second). Re-issue once and say so; with that race fixed in cork the note
# never appears.
down_worker() {
  cmgrd-cli worker-down "$1"
  sleep 1
  if ! health_is "$1" down; then
    note "worker-down on $1 was overwritten by an in-flight telemetry poll (cork race); re-issuing"
    cmgrd-cli worker-down "$1"
    sleep 1
  fi
  health_is "$1" down || fail "worker $1 is not reported down after worker-down"
}

# readd <worker ip>: the recovery path for a down or purged worker.
readd() {
  cmgrd-cli worker-add "$1" "${PUBLIC[$1]}"
  retry 30 "worker $1 to come back ok" health_is "$1" ok
}

worker_health() { api GET /workers | jq -r --arg ip "$1" '.[] | select(.ip==$ip) | .health'; }
health_is() { [[ "$(worker_health "$1")" == "$2" ]]; }

# launch <build id> <user id> <custom var>: POST /builds/<id> as the platform does.
launch() {
  api POST "/builds/$1" "$(jq -cn --arg u "$2" --arg v "$3" '{user_id: $u, env: {CUSTOM_VAR: $v}}')"
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
# instance. cmgrd removes its containers and network on the worker but keeps
# the instance row (no containers, no ports) until the platform stops it; the
# stop is then a plain 204.
assert_torn_down() {
  local meta
  meta=$(api GET "/instances/$1")
  [[ "$(jq -r '.containers | length' <<<"$meta")" == 0 ]] || fail "instance $1 still lists containers after the rebuild"
  [[ "$(jq -r '.ports // {} | length' <<<"$meta")" == 0 ]] || fail "instance $1 still holds ports after the rebuild"
  assert_gone_from_worker "${INST_WORKER[$1]}" "$1" "${INST_META[$1]}"
  [[ "$(api_status DELETE "/instances/$1")" == 204 ]] || fail "stopping the torn-down instance $1 was not a 204"
  [[ "$(api_status GET "/instances/$1")" == 404 ]] || fail "instance $1 is still known after its stop"
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
if have_outer; then note "outer docker socket available: chaos steps enabled"; else note "outer docker socket unavailable or E2E_CHAOS=$E2E_CHAOS: chaos steps will be skipped"; fi

step "waiting for the fleet"
retry 90 "cmgrd" quiet api GET /version
retry 90 "zot" quiet registry_api /v2/
retry 60 "the builder daemon" quiet curl -sSf "$E2E_BUILDER/_ping"
for ip in "${WORKER_IPS[@]}"; do
  retry 120 "dockerd on $ip" quiet worker_api "$ip" /_ping
  retry 60 "telemetry on $ip" quiet curl -sSf --max-time 3 "http://$ip:2136/health"
  sweep_worker "$ip"
done
ok "cmgrd $(api GET /version | jq -r .version), zot, builder, and $NWORKERS workers (dockerd + telemetry) answer"

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

# ------------------------------------------------- 8. cmgrd restart

step "cmgrd restart: workers and instances come back from the database"
if have_outer; then
  cork=$(compose_container cork)
  [[ -n "$cork" ]] || fail "cork container not found via the outer docker API"
  outer_ctl "$cork" restart
  retry 60 "cmgrd after restart" quiet api GET /version
  retry 30 "every worker to report ok again" all_workers_ok
  [[ "$(api_status GET "/instances/$PERSIST_INST")" == 200 ]] || fail "persistent instance $PERSIST_INST forgotten across the restart"
  id=${OD_IDS[0]}
  meta=$(api GET "/instances/$id")
  [[ "$(jq -r .worker <<<"$meta")" == "${INST_WORKER[$id]}" ]] || fail "instance $id lost its worker across the restart"
  check_ondemand "$meta" "${INST_USER[$id]}"
  inst=$(launch "$OD_BUILD" "e2e-user-5" "e2e-value-5")
  track "$inst" 5
  check_ondemand "$inst" 5
  ok "after a restart the workers are re-polled to ok, instance $id is still served and stoppable, new launches work"
else
  skip "needs the outer docker socket"
fi

# ---------------------------------------------- 9. rebuild (update)

step "update, generation 2: edited sources rebuild both changed challenges, push new tags, restart the persistent instance, tear down on-demand ones"
OD_BUILD_G1=$(api GET "/builds/$OD_BUILD")
OD_TAG_G1=${IMAGE_TAG[$CH_ONDEMAND]}
PERSIST_TAG_G1=${IMAGE_TAG[$CH_PERSISTENT]}
PERSIST_META_G1=$(api GET "/instances/$PERSIST_INST")
set_generation 2 both
t=$(date +%s)
out=$(cmgrd-cli update --prune-old)
sed 's/^/       /' <<<"$out"
note "updated in $(( $(date +%s) - t ))s"
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
note "${#OD_IDS[@]} on-demand instances were torn down on their workers and not restarted; their records stayed (no containers, no ports) until the platform's DELETE"
OD_IDS=()
PERSIST_META_G2=$(api GET "/instances/$PERSIST_INST")
[[ "$(jq -c '.containers | sort' <<<"$PERSIST_META_G2")" != "$(jq -c '.containers | sort' <<<"$PERSIST_META_G1")" ]] || fail "persistent instance $PERSIST_INST kept its old containers"
w=$(jq -r .worker <<<"$PERSIST_META_G2")
pub=$(jq -r .worker_public <<<"$PERSIST_META_G2")
port=$(jq -r '.ports.socat // 0' <<<"$PERSIST_META_G2")
[[ "$w" == "$(jq -r .worker <<<"$PERSIST_META_G1")" ]] || fail "persistent instance $PERSIST_INST moved workers on rebuild"
[[ "$port" == "$(jq -r .ports.socat <<<"$PERSIST_META_G1")" ]] || fail "persistent instance $PERSIST_INST changed port on rebuild: $(jq -r .ports.socat <<<"$PERSIST_META_G1") -> $port"
assert_on_worker "$w" "$PERSIST_META_G2"
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
note "instance $victim cleared from cmgrd; its container still runs on $WB (docker-reaper's job in production)"
reap "$WB" "$victim" "$vmeta"
forget "$victim"
readd "$WB"
ok "launches went to $WA only, the stop on $WB was DB-only, worker-add brought $WB back"

# ------------------------------------------------ 11. telemetry silence

step "telemetry silence: a worker whose agent goes quiet is marked down after 30s and stays down until re-added"
if have_outer; then
  sidecar=$(compose_container "${PUBLIC[$WB]}-telemetry")
  [[ -n "$sidecar" ]] || fail "no container for compose service ${PUBLIC[$WB]}-telemetry"
  ensure_instance_on "$WB" 16
  id=$(instance_on "$WB")
  STOPPED_SIDECAR=$sidecar
  outer_ctl "$sidecar" stop
  note "stopped ${PUBLIC[$WB]}-telemetry; cmgrd tolerates 30s of silence (60 misses at 500ms)"
  retry 60 "worker $WB to be marked down" health_is "$WB" down
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
if have_outer; then
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
if have_outer; then
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

step "hung dockerd: a control call that hangs marks the worker down; later stops on it clear records only"
if have_outer; then
  ensure_instance_on "$WB" 18
  id=$(instance_on "$WB")
  wb=$(compose_container "${PUBLIC[$WB]}")
  [[ -n "$wb" ]] || fail "compose container for ${PUBLIC[$WB]} not found"
  PAUSED_WORKER=$wb # the EXIT trap unpauses if anything below fails
  outer_ctl "$wb" pause
  note "paused ${PUBLIC[$WB]}'s dockerd (telemetry keeps answering); stopping instance $id, expect the 30s control timeout"
  t=$(date +%s)
  if cmgrd-cli stop "$id" >/dev/null 2>&1; then
    fail "stop of instance $id succeeded against a paused daemon"
  fi
  note "stop failed after $(( $(date +%s) - t ))s"
  health_is "$WB" down || fail "worker $WB was not marked down after the hung call"
  [[ "$(api_status DELETE "/instances/$id")" == 204 ]] || fail "second stop of instance $id on the down worker was not a 204"
  [[ "$(api_status GET "/instances/$id")" == 404 ]] || fail "instance $id still known"
  outer_ctl "$wb" unpause
  PAUSED_WORKER=""
  retry 30 "dockerd on $WB" quiet worker_api "$WB" /_ping
  reap "$WB" "$id" "${INST_META[$id]}"
  forget "$id"
  readd "$WB"
  ok "hung call -> $WB down (sticky), records cleared on the second stop, worker-add after unpause recovered it"
else
  skip "needs the outer docker socket"
fi

# ------------------------------------------------- 15. worker-remove

step "worker-remove: purges the worker and its instance records, leaving its containers to the reaper"
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
  reap "$WB" "$id" "${INST_META[$id]}"
  forget "$id"
done
readd "$WB"
[[ "$(api GET /workers | jq -r --arg ip "$WB" '.[] | select(.ip == $ip) | .instances')" == 0 ]] || fail "re-added worker $WB still counts instances"
ok "worker $WB purged with ${#victims[@]} instance record(s), containers left running, re-added clean"

# -------------------------------------------------------------- 16. teardown

step "teardown: stop the remaining on-demand instances, remove the schema, check the workers, builder, and registry are clean"
for id in "${OD_IDS[@]}"; do
  cmgrd-cli stop "$id"
  assert_gone "$id" "${INST_WORKER[$id]}" "${INST_META[$id]}"
done
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

printf '\nALL STEPS PASSED in %ds\n' "$(( $(date +%s) - T0 ))"
