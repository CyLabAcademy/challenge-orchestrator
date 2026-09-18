#!/usr/bin/env bash
# Bring up the class box and run its scenario on it.
#
# One container: dockerd and corkd together, no registry, no workers, no PKI.
# See compose-single.yaml for why this exists alongside run.sh -- in short,
# the multi-host fleet covers what cork does, and this covers the deployment
# that has none of what the multi-host fleet is made of.
#
# The scenario runs with `exec` rather than as its own service, because the
# point of this fleet is that there is only one machine: cork, the docker
# daemon, the instances and the operator all share a localhost.
set -euo pipefail
cd "$(dirname "$0")"
# The scenario's path is inside the container, and git-bash on Windows would
# otherwise rewrite it to one on this host ("C:/Program Files/Git/opt/e2e/
# ...") before docker ever sees it. Unset everywhere else, where it does
# nothing. run.sh needs none of this: compose carries its entrypoint, so no
# container path crosses a command line there.
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'
export CORK_VERSION="${CORK_VERSION:-$(sh ../ci/version.sh)}"
P=cork-e2e-single
# No --wait: the scenario's first step waits for corkd and says what it was
# waiting for when it gives up, and two waits with two timeouts would drift.
docker compose -p "$P" -f compose-single.yaml up -d --build
docker compose -p "$P" -f compose-single.yaml exec -T box bash /opt/e2e/scenario-single.sh
