#!/usr/bin/env bash
# Bring the fleet up, building cork from this checkout, and run the scenario.
#
# --full adds the steps that are only worth their seconds when nobody is
# waiting for the answer: the registry's failure paths, artifact delivery,
# multi-container challenges, cold pulls, docker-reaper and resource limits.
set -euo pipefail
cd "$(dirname "$0")"
if [[ "${1:-}" == "--full" ]]; then
  export E2E_FULL=1
  shift
fi
export CORK_VERSION="${CORK_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo e2e)}"
docker compose up -d --build
docker compose run --rm e2e
