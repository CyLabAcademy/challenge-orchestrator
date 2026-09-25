#!/bin/sh
# corkd creates CORK_ARTIFACT_DIR and the database's directory itself, so
# nothing here does it for it: a fresh box needs only the challenge tree, and
# the fleet is the only place that claim is tested. The tree is copied from
# the read-only seed mount into the CORK_DIR volume once, so the scenario can
# edit challenges and run update.
set -eu
if [ -d /challenges-seed ] && [ -d "${CORK_DIR:-/challenges}" ] && [ -z "$(ls -A "${CORK_DIR:-/challenges}")" ]; then
  echo "seeding ${CORK_DIR:-/challenges} from /challenges-seed"
  cp -a /challenges-seed/. "${CORK_DIR:-/challenges}/"
fi
# The CloudWatch agent stand-in, started before corkd rather than beside it:
# corkd opens its socket at startup, and a first write to a port nothing holds
# yet would back the exporter off for 30 seconds and lose every record until
# it redialled.
if [ -n "${E2E_EMF_CAPTURE:-}" ]; then
  # Only the stop file: a stale one would kill the stub the moment it starts.
  # The framing marker belongs to the run, not the container, so the scenario
  # clears it once at the start -- clearing it here as well would wipe what
  # this run had already recorded every time a step restarts corkd, which is
  # the same trap the capture itself was moved out of this script to avoid.
  rm -f "${E2E_EMF_CAPTURE}.stop"
  python3 /usr/local/bin/emf-stub.py "${E2E_EMF_CAPTURE}" &
  # Cheap insurance that the bind has happened. The socket is up in
  # milliseconds; corkd takes far longer to reach its first launch, so this is
  # belt and braces rather than a race being papered over.
  sleep 1
fi

exec "$@"
