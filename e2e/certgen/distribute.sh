#!/usr/bin/env bash
#
# Mint the fleet PKI with the real config-examples/gen-docker-certs.sh (bind
# mounted in by compose) and lay the files out per its deployment map, one
# volume per box and trust domain, under the exact names each consumer reads:
#
#   /dist/cork-docker      orchestrator DOCKER_CERT_PATH   ca.pem cert.pem key.pem
#   /dist/cork-registry    orchestrator + builder certs.d  ca.crt client.cert client.key (CN=cmgr)
#   /dist/worker-docker    worker dockerd --tls*           docker-{ca-cert,server-cert,server-key}.pem
#   /dist/worker-registry  worker certs.d                  ca.crt client.cert client.key (CN=worker)
#   /dist/zot              registry server identity        zot-{ca-cert,server-cert,server-key}.pem
#
# The CA keys stay in /pki, a volume mounted nowhere else. Runs once per
# volume set; `docker compose down -v` starts over.
set -euo pipefail

PKI=/pki
DIST=/dist
REGISTRY_HOST="${REGISTRY_HOST:-zot}"
STAMP="$PKI/.distributed"

if [[ -f "$STAMP" ]]; then
  echo "certgen: PKI already distributed on $(cat "$STAMP"); nothing to do"
  exit 0
fi

echo "certgen: minting the fleet PKI for registry '$REGISTRY_HOST'"
OUT="$PKI" DAYS="${DAYS:-3650}" bash /usr/local/bin/gen-docker-certs.sh "$REGISTRY_HOST"

put() { # put <bundle file> <dest dir> <dest name> <mode>
  install -m "$4" "$PKI/$1" "$2/$3"
}

# ORCHESTRATOR
put docker-ca-cert.pem     "$DIST/cork-docker"   ca.pem      0444
put docker-client-cert.pem "$DIST/cork-docker"   cert.pem    0444
put docker-client-key.pem  "$DIST/cork-docker"   key.pem     0400
put zot-ca-cert.pem        "$DIST/cork-registry" ca.crt      0444
put zot-client-cert.pem    "$DIST/cork-registry" client.cert 0444
put zot-client-key.pem     "$DIST/cork-registry" client.key  0400

# EACH WORKER
put docker-ca-cert.pem     "$DIST/worker-docker"   docker-ca-cert.pem     0444
put docker-server-cert.pem "$DIST/worker-docker"   docker-server-cert.pem 0444
put docker-server-key.pem  "$DIST/worker-docker"   docker-server-key.pem  0400
put zot-ca-cert.pem        "$DIST/worker-registry" ca.crt                 0444
put zot-worker-cert.pem    "$DIST/worker-registry" client.cert            0444
put zot-worker-key.pem     "$DIST/worker-registry" client.key             0400

# REGISTRY BOX
put zot-ca-cert.pem        "$DIST/zot" zot-ca-cert.pem     0444
put zot-server-cert.pem    "$DIST/zot" zot-server-cert.pem 0444
put zot-server-key.pem     "$DIST/zot" zot-server-key.pem  0400

date -u +%Y-%m-%dT%H:%M:%SZ > "$STAMP"
echo "certgen: distributed"
