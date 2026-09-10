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
exec "$@"
