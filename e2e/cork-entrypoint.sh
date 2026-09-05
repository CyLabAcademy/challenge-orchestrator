#!/bin/sh
# cmgrd refuses to start unless CMGR_ARTIFACT_DIR and the database's
# directory exist; the ansible role pre-creates them, this does the same.
# The challenge tree is copied from the read-only seed mount into the
# CMGR_DIR volume once, so the scenario can edit challenges and run update.
set -eu
mkdir -p "${CMGR_ARTIFACT_DIR:-.}" "$(dirname "${CMGR_DB:-cmgr.db}")"
if [ -d /challenges-seed ] && [ -d "${CMGR_DIR:-/challenges}" ] && [ -z "$(ls -A "${CMGR_DIR:-/challenges}")" ]; then
  echo "seeding ${CMGR_DIR:-/challenges} from /challenges-seed"
  cp -a /challenges-seed/. "${CMGR_DIR:-/challenges}/"
fi
exec "$@"
