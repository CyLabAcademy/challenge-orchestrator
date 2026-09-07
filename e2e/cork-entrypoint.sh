#!/bin/sh
# cmgrd creates CMGR_ARTIFACT_DIR and the database's directory itself, so
# nothing here does it for it: a fresh box needs only the challenge tree, and
# the fleet is the only place that claim is tested. The tree is copied from
# the read-only seed mount into the CMGR_DIR volume once, so the scenario can
# edit challenges and run update.
set -eu
if [ -d /challenges-seed ] && [ -d "${CMGR_DIR:-/challenges}" ] && [ -z "$(ls -A "${CMGR_DIR:-/challenges}")" ]; then
  echo "seeding ${CMGR_DIR:-/challenges} from /challenges-seed"
  cp -a /challenges-seed/. "${CMGR_DIR:-/challenges}/"
fi
exec "$@"
