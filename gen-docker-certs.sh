#!/usr/bin/env bash
#
# Generate the PKI for the cmgr fleet: one private CA and four leaf certs.
#
#   1. Worker dockerd server cert  CN/SAN DNS:academy-docker-worker (shared by
#      every worker; cmgrd pins this ServerName, so no per-worker certs needed)
#   2. cmgr client cert            CN=cmgr   — dockerd mTLS client (DOCKER_CERT_PATH)
#                                              AND zot read-write identity (orchestrator only)
#   3. worker registry client cert CN=worker — zot read-only identity (shared by workers)
#   4. zot server cert             CN/SAN = registry address
#
# The cmgr client files are named ca.pem/cert.pem/key.pem exactly because
# Docker's SDK (client.FromEnv) looks for those names in DOCKER_CERT_PATH,
# and cmgrd reuses the same directory for worker TLS material (workers.go).
#
# Re-running reuses an existing CA in $OUT, so leafs can be re-issued (or new
# ones added) without re-deploying ca.pem across the fleet.
#
# SECURITY: ca-key.pem never leaves this machine — it is deployed to NO box.
# zot must keep anonymousPolicy/defaultPolicy empty; the CNs above are the
# only identities (users "cmgr" and "worker" in zot's config.json).
#
# Usage: ./gen-docker-certs.sh [registry-address]   (default 10.12.34.121)
#   env: DAYS=3650  OUT=./docker-certs  WORKER_SERVERNAME=academy-docker-worker
set -euo pipefail

REGISTRY="${1:-10.12.34.121}"                              # zot's address (IP or DNS name)
WORKER_NAME="${WORKER_SERVERNAME:-academy-docker-worker}"  # must match WORKER_SERVERNAME in cmgr/workers.go
DAYS="${DAYS:-3650}"                                       # cert validity (~10 years; private CA)
OUT="${OUT:-./docker-certs}"

mkdir -p "$OUT"
cd "$OUT"

if [[ -f ca.pem && -f ca-key.pem ]]; then
  echo "== CA (reusing existing) =="
else
  echo "== CA =="
  openssl genrsa -out ca-key.pem 4096
  openssl req -new -x509 -days "$DAYS" -key ca-key.pem -sha256 -out ca.pem \
    -subj "/CN=cmgr-docker-ca"
fi

# issue <key-file> <cert-file> <subject-CN> <ext-lines...>
issue() {
  local key="$1" crt="$2" cn="$3"; shift 3
  rm -f "$key" "$crt"
  openssl genrsa -out "$key" 4096
  openssl req -new -key "$key" -out leaf.csr -subj "/CN=$cn"
  printf '%s\n' "$@" > leaf-ext.cnf
  openssl x509 -req -days "$DAYS" -sha256 \
    -in leaf.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
    -out "$crt" -extfile leaf-ext.cnf
  rm -f leaf.csr leaf-ext.cnf
}

# SAN type for the registry leaf: IP for a v4 address, DNS otherwise
if [[ "$REGISTRY" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  REGISTRY_SAN="IP:$REGISTRY,IP:127.0.0.1"
else
  REGISTRY_SAN="DNS:$REGISTRY,IP:127.0.0.1"
fi

echo "== worker dockerd server cert (shared, ServerName $WORKER_NAME) =="
issue server-key.pem server-cert.pem "$WORKER_NAME" \
  "subjectAltName = DNS:$WORKER_NAME" \
  "extendedKeyUsage = serverAuth"

echo "== zot server cert (registry @ $REGISTRY) =="
issue zot-key.pem zot-cert.pem "$REGISTRY" \
  "subjectAltName = $REGISTRY_SAN" \
  "extendedKeyUsage = serverAuth"

echo "== cmgr client cert (dockerd client + zot read-write) =="
issue key.pem cert.pem "cmgr" \
  "extendedKeyUsage = clientAuth"

echo "== worker registry client cert (zot read-only) =="
issue worker-key.pem worker-cert.pem "worker" \
  "extendedKeyUsage = clientAuth"

echo "== lock down + clean up =="
chmod 0400 ca-key.pem server-key.pem zot-key.pem key.pem worker-key.pem
chmod 0444 ca.pem server-cert.pem zot-cert.pem cert.pem worker-cert.pem
rm -f ca.srl

echo "done -> $(pwd)"
ls -l

cat <<EOF

Deployment map (CERTS.D = /etc/docker/certs.d/$REGISTRY:5000)
----------------------------------------------------------------------
ca-key.pem        -> NOWHERE. Stays on this machine, offline.

ORCHESTRATOR
  ca.pem          -> \$DOCKER_CERT_PATH/ca.pem  and  CERTS.D/ca.crt
  cert.pem        -> \$DOCKER_CERT_PATH/cert.pem and  CERTS.D/client.cert
  key.pem         -> \$DOCKER_CERT_PATH/key.pem  and  CERTS.D/client.key

EACH WORKER
  ca.pem          -> dockerd --tlscacert  and  CERTS.D/ca.crt
  server-cert.pem -> dockerd --tlscert   (listen tcp://0.0.0.0:2376, --tlsverify)
  server-key.pem  -> dockerd --tlskey
  worker-cert.pem -> CERTS.D/client.cert
  worker-key.pem  -> CERTS.D/client.key

REGISTRY BOX (zot)
  ca.pem          -> http.tls.cacert   (client-cert verification)
  zot-cert.pem    -> http.tls.cert
  zot-key.pem     -> http.tls.key
----------------------------------------------------------------------
EOF
