#!/usr/bin/env bash
# The single-host scenario: cork as a class deployment runs it.
#
# Deliberately small. The multi-host fleet (scenario.sh) covers what cork does
# -- every delivery shape, artifacts, ports, admission, rebuild generations,
# the registry and its failure paths. Repeating any of that here would buy
# nothing and cost minutes. What no other scenario covers is the deployment
# with none of it: one docker daemon, no registry, no workers, no
# certificates. That is the default and the legacy cmgr shape, and the
# multi-host work and the build-plane split both landed on top of it.
#
# It runs on the box itself (run-single.sh execs it there), so every address
# below is the one a class deployment really uses: cork on localhost, docker
# on /var/run/docker.sock, instances on localhost. Nothing here stands in for
# anything.
#
# Every step asserts something that is only true here:
#
#   1. corkd starts with no registry, no DOCKER_HOST, no certs and no workers
#   2. a build is made and kept on the one daemon, with nothing pushed
#   3. the converge places its instance on that daemon -- no worker on the row
#   4. it serves, and its own solver gets the flag cork recorded for the build
#   5. a rebuild works with no registry to untag in
#   6. teardown leaves the daemon as it found it
#
# Step 4 is the one that matters most: it is the whole pipeline of a class
# deployment in one assertion -- build locally, launch locally, serve the flag
# that was baked in, to a solver that knows only what the orchestrator said.
set -uo pipefail

CORK="${E2E_CORK:-http://127.0.0.1:4200}"
export CORK_SERVER="$CORK"
DOCKER_SOCK="${E2E_DOCKER_SOCK:-/var/run/docker.sock}"
CHALLENGES="${CORK_DIR:-/challenges}"
SCHEMA_FILE=/opt/e2e/schema-single.yaml
SCHEMA_NAME=single
CHALLENGE=cmgr/examples/binex101
SRC=remote-make/BinEx101.c   # the file the rebuild step edits, under CORK_DIR
SEED="${E2E_SEED:-/challenges-seed}"   # the read-only pristine tree, to restore from
T0=$(date +%s)

step() { printf '\n=== [%4ds] %s\n' "$(( $(date +%s) - T0 ))" "$*"; }
note() { printf '  --   %s\n' "$*"; }
ok()   { printf '  ok   %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }

retry() { # retry <seconds> <what> <cmd...>
  local timeout=$1 what=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@"; do
    (( $(date +%s) < deadline )) || fail "timed out after ${timeout}s waiting for $what"
    sleep 1
  done
}

api() { # api <method> <path> [body]
  curl -sS --fail-with-body --connect-timeout 5 --max-time 900 \
    -X "$1" -H 'Content-Type: application/json' ${3:+-d "$3"} "$CORK$2"
}
api_status() {
  curl -sS -o /tmp/body -w '%{http_code}' --connect-timeout 5 --max-time 900 \
    -X "$1" -H 'Content-Type: application/json' ${3:+-d "$3"} "$CORK$2"
}
# The box's own docker daemon, over the same socket cork uses: every "is it
# really there" assertion goes through this rather than through cork, so cork
# saying so is never the evidence for cork having done it.
box() { curl -sS --fail-with-body --max-time 30 --unix-socket "$DOCKER_SOCK" "http://localhost$1"; }

# What cork tags an image, worked out the way cork does: the content-addressed
# tag is derived from the build rather than stored on it (dockerId in cmgr),
# and with no registry the repository is the challenge id alone.
image_name() { # image_name <build metadata json> <host>
  printf '%s:s%d-%x-%s' "$CHALLENGE" "$(jq -r .seed <<<"$1")" "$(jq -r .checksum <<<"$1")" "$2"
}

# ---------------------------------------------------------------- 1. startup

step "startup: corkd runs beside dockerd on one box, with no registry, no DOCKER_HOST, no certificates and no workers"
retry 90 "corkd to answer on $CORK" curl -sSf --max-time 3 -o /dev/null "$CORK/version"
version=$(api GET /version)
[[ "$(jq -r .build_plane <<<"$version")" == "local" ]] ||
  fail "build_plane is '$(jq -r .build_plane <<<"$version")', want local: this fleet is the default deployment, not an external build plane"
[[ -z "${DOCKER_HOST:-}" ]] ||
  fail "DOCKER_HOST is set to '$DOCKER_HOST': this box is meant to prove cork reaches docker at its local socket"
box /_ping >/dev/null || fail "the docker daemon is not answering on $DOCKER_SOCK"
# No workers, and none coming: this is what makes every launch below take the
# local-daemon path (selectWorker returns "", cmgr/api.go).
workers=$(api GET /workers)
[[ "$(jq -r 'length' <<<"$workers")" == 0 ]] ||
  fail "corkd came up with workers registered: $workers"
ok "corkd serves with CORK_REGISTRY, DOCKER_HOST, DOCKER_CERT_PATH and CORK_BUILD_PLANE all unset, and no worker registered"

# ------------------------------------------------------- 2. build, no registry

step "build: the challenge tree is scanned and built on the one daemon, with no registry to push to"
# --verbose so Unmodified is printed too: nothing else scans the tree, but
# these volumes outlive a run, so on the second run the challenge is already
# recorded and a quiet update would name nothing.
out=$(cork update --verbose) || fail "update failed: $out"
sed 's/^/       /' <<<"$out"
grep -q "$CHALLENGE" <<<"$out" ||
  fail "update did not report $CHALLENGE at all: $out"
t=$(date +%s)
cork add-schema "$SCHEMA_FILE" || fail "add-schema failed"
note "the schema converged in $(( $(date +%s) - t ))s"

BUILD=$(api GET "/schemas/$SCHEMA_NAME" | jq -r --arg id "$CHALLENGE" '.[] | select(.id == $id) | .builds[0].id')
[[ "$BUILD" =~ ^[0-9]+$ ]] || fail "no build was recorded for $CHALLENGE"
meta=$(api GET "/builds/$BUILD")
FLAG=$(jq -r .flag <<<"$meta")
[[ "$FLAG" == single\{* ]] || fail "build $BUILD carries no flag of this schema's format: '$FLAG'"

# The images are on the box and nowhere else. A build's tag is derived rather
# than stored -- s<seed>-<checksum>-<host>, cmgr's dockerId -- and with no
# registry configured the repository is the challenge id alone, because every
# push path short-circuits on challengeRegistry == "".
for host in $(jq -r '.images[].host' <<<"$meta"); do
  image=$(image_name "$meta" "$host")
  box "/images/$image/json" >/dev/null ||
    fail "image '$image' of build $BUILD is not on the box's daemon: a single-host build must leave its images there"
done
# And nothing carrying this build's content is registry qualified. Asked of
# the daemon rather than of cork, so it is the images themselves that say so:
# a repository whose first segment has a dot or a port in it is a registry,
# and would mean something tried to push where there is nothing to push to.
csum=$(printf '%x' "$(jq -r .checksum <<<"$meta")")
for tag in $(box "/images/json" | jq -r '.[].RepoTags[]? // empty' | grep -- "-$csum-" || true); do
  first=${tag%%/*}
  [[ "$first" != *.* && "$first" != *:* ]] ||
    fail "the daemon holds '$tag' for build $BUILD, which names a registry although none is configured"
done
ok "$CHALLENGE built as build $BUILD with flag $FLAG; its image(s) are on the one daemon and carry no registry"

# ------------------------------------------------- 3. placement on the local daemon

step "placement: the instance the converge started runs on the local daemon, with no worker on its row"
# There is no list of instances to ask for; they hang off their build in the
# state element, which is the same shape a hand-over carries.
PERSIST=$(api GET /state | jq -r --argjson b "$BUILD" '.[].builds[]? | select(.id == $b) | .instances[]?.id' | head -1)
[[ "$PERSIST" =~ ^[0-9]+$ ]] || fail "the converge started no instance for build $BUILD"
pmeta=$(api GET "/instances/$PERSIST")
# The whole point of this fleet. On the multi-host one this field always names
# a worker; here it must be empty, which is the branch instanceClient takes to
# hand back m.cli (cmgr/workers.go) instead of a worker's client.
worker=$(jq -r '.worker // ""' <<<"$pmeta")
[[ -z "$worker" ]] ||
  fail "instance $PERSIST was placed on worker '$worker', although none is registered: this deployment has only the local daemon"
for cid in $(jq -r '.containers[]' <<<"$pmeta"); do
  state=$(box "/containers/$cid/json" | jq -r .State.Status) ||
    fail "container $cid of instance $PERSIST is not on the box's daemon"
  [[ "$state" == running ]] || fail "container $cid of instance $PERSIST is $state, not running"
done
ok "instance $PERSIST carries no worker and its container(s) run on the box's own daemon"

# --------------------------------------------------------------- 4. it solves

step "solve: the instance serves the flag cork baked into it, to the challenge's own solver"
# The instance the converge started, not a second one: a build the schema
# holds at a fixed count is locked against extra launches ("change the schema
# definition to start more instances"), which is the answer a class operator
# gets too. Solving the one that is running costs no second build and tests
# the same thing.
nports=$(jq -r '.ports | length' <<<"$pmeta")
(( nports == 1 )) || fail "instance $PERSIST publishes $nports port(s); this challenge publishes one: $(jq -c .ports <<<"$pmeta")"
PORT=$(jq -r '.ports | to_entries[0].value' <<<"$pmeta")
# Reached where the orchestrator said it is, on the box it said it is on --
# which here is this one. Nothing about the challenge is known but that.
retry 90 "the challenge to answer at 127.0.0.1:$PORT" \
  sh -c "printf '1\n1\n' | nc -w 5 127.0.0.1 '$PORT' 2>/dev/null | grep -q 'Give me a number'"
solved=$(timeout 60 python3 "$CHALLENGES/remote-make/solver/solve.py" \
  --host 127.0.0.1 --port "$PORT" --print 2>&1) ||
  fail "the solve script failed against instance $PERSIST at 127.0.0.1:$PORT: $solved"
got=$(sed -n 's/^flag: //p' <<<"$solved")
[[ "$got" == "$FLAG" ]] ||
  fail "solving instance $PERSIST returned '$got' and build $BUILD carries '$FLAG': the flag cork baked into the image on this box is not the one it serves (solver said: $solved)"
ok "instance $PERSIST on 127.0.0.1:$PORT was solved with the challenge's own script and gave back build $BUILD's flag"

# ------------------------------------------------------- 5. rebuild, no registry

step "rebuild: a source change rebuilds on the one daemon, with no registry tag to retire"
before_tag=$(image_name "$meta" challenge)
# From the seed rather than from whatever is there: a run that died before
# its teardown leaves this file edited, and an edit applied to an edit would
# nest ("please, please") and drift the source generation between runs.
cp "$SEED/$SRC" "$CHALLENGES/$SRC" || fail "could not restore $SRC from the seed"
sed -i 's/Give me a number/Give me a number, please/' "$CHALLENGES/$SRC" ||
  fail "could not edit $CHALLENGES/$SRC"
out=$(cork update) || fail "the update after the source edit failed: $out"
sed 's/^/       /' <<<"$out"
after=$(api GET "/builds/$BUILD")
after_tag=$(image_name "$after" challenge)
[[ "$after_tag" != "$before_tag" ]] ||
  fail "build $BUILD still carries $before_tag after its source changed: the rebuild did not happen"
box "/images/$after_tag/json" >/dev/null ||
  fail "the rebuilt image $after_tag is not on the box's daemon"
# And the generation it displaced is still there, asked of the daemon rather
# than taken on trust: cork keeps one rollback generation, so a single
# rebuild retires nothing at all. That is what makes this step a check of
# the registry-less path and not of retention -- there is nothing here for
# retireRegistryTag to have skipped, and the multi-host fleet is where
# retention itself is proved.
box "/images/$before_tag/json" >/dev/null ||
  fail "the generation $before_tag was untagged by a single rebuild: it is the rollback generation and cork keeps it"
ok "the edit rebuilt build $BUILD to $after_tag on the one daemon, kept $before_tag as the rollback generation, and pushed to no registry"

# ---------------------------------------------------------------- 6. teardown

step "teardown: instances stop and the schema is removed, leaving the daemon as it was"
# Re-read rather than reuse $pmeta: a rebuild restarts the instance in
# place when it can and replaces it when it cannot, so the containers to
# account for are whichever it has now.
pmeta=$(api GET "/instances/$PERSIST") ||
  fail "instance $PERSIST is gone after the rebuild: it was neither restarted in place nor relaunched"
containers=$(jq -r '.containers[]' <<<"$pmeta")
# The schema holds this instance, so removing the schema is what stops it --
# a direct stop of a locked build's instance is refused, as it is anywhere.
cork remove-schema "$SCHEMA_NAME" || fail "remove-schema failed"
[[ "$(api_status GET "/builds/$BUILD")" == 404 ]] || fail "build $BUILD survived the schema removal"
for cid in $containers; do
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 --unix-socket "$DOCKER_SOCK" "http://localhost/containers/$cid/json")
  [[ "$code" == 404 ]] ||
    fail "container $cid of the persistent instance is still on the daemon (HTTP $code) after its schema was removed"
done
# And the images go with them, both generations of them. Only here is that
# worth asserting: on the multi-host fleet the builder purged its copies as
# soon as it had pushed them, so by the time a build was destroyed there was
# nothing local left to remove and the registry untag was the whole of it.
# This box pushed nowhere, so these are the only copies there are -- and a
# destroy that leaves them behind grows the one disk that also holds the
# tree, the database and the artifacts.
for gone in "$after_tag" "$before_tag"; do
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 --unix-socket "$DOCKER_SOCK" "http://localhost/images/$gone/json")
  [[ "$code" == 404 ]] ||
    fail "image $gone is still on the box's daemon (HTTP $code) after its schema was removed: with no registry it is the only copy of that generation, and nothing is coming back for it"
done
left=$(box "/containers/json?all=true" | jq -r '[.[] | select(.Names[]? | test("^/cmgr-"))] | length')
[[ "$left" == 0 ]] ||
  fail "$left cmgr container(s) are still on the box's daemon after teardown"
nets=$(box "/networks" | jq -r '[.[] | select(.Name | test("^cmgr-"))] | length')
[[ "$nets" == 0 ]] || fail "$nets cmgr network(s) are still on the box's daemon after teardown"
# Put the tree back from the seed, not by undoing the edit: these volumes
# outlive a run, and a restore that reverses one substitution only works if
# exactly one was applied. Then record it, so the next run starts with the
# tree and the database agreeing.
cp "$SEED/$SRC" "$CHALLENGES/$SRC" || fail "could not restore $SRC from the seed"
cmp -s "$SEED/$SRC" "$CHALLENGES/$SRC" || fail "$SRC still differs from the seed after the restore"
cork update >/dev/null || true
ok "instances gone, builds gone, both image generations gone from the one daemon, no cmgr containers or networks left, tree restored"

printf '\nALL STEPS PASSED in %ds\n' "$(( $(date +%s) - T0 ))"
