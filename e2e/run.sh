#!/usr/bin/env bash
# Bring the fleet up, building cork from this checkout, and run the scenario.
set -euo pipefail
cd "$(dirname "$0")"
export CORK_VERSION="${CORK_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo e2e)}"
docker compose up -d --build
docker compose run --rm e2e
