#!/usr/bin/env bash
#
# Generate the PKI for the cmgr fleet: TWO private CAs and their leaf certs.
#
# Naming convention — every file is  <domain>-<role>-<kind>.pem :
#   domain = docker | zot                 (the two trust domains / CAs)
#   role   = ca | server | client | worker
#   kind   = cert | key                   (so a CA is <domain>-ca-cert / <domain>-ca-key)
#
#   docker-ca-cert / docker-ca-key      docker CA (cmgr <-> worker dockerd mTLS)
#   docker-server-cert / -key           worker dockerd server (CN/SAN academy-docker-worker, shared)
#   docker-client-cert / -key           cmgr -> worker dockerd (CN cmgr)
#
#   zot-ca-cert / zot-ca-key            zot CA (registry mTLS)
#   zot-server-cert / -key              zot server            (CN/SAN = registry address)
#   zot-client-cert / -key              cmgr -> zot, READ-WRITE (CN cmgr; orchestrator push + delete)
#   zot-worker-cert / -key              worker -> zot, READ-ONLY (CN worker; image pull, shared)
#
# "client" is the orchestrator (cmgr) in both domains. zot has a second client — the
# workers' read-only pull identity — which gets the "worker" role since it is the one
# principal that does not fit server/client.
#
# The two CAs split the trust domains so a leak in one cannot forge into the other:
# a stolen zot-worker cert grants zot read but cannot control any worker's dockerd
# (dockerd trusts only the docker CA). cmgr's identity is TWO certs (docker-client +
# zot-client): same CN=cmgr, but each signed by its own CA and deployed to a different
# directory (DOCKER_CERT_PATH vs certs.d), which cmgrd reads independently
# (workers.go vs registry.go) — so no code change is needed.
#
# These are the BUNDLE names. At each destination the consuming software fixes the
# on-disk names: DOCKER_CERT_PATH wants ca.pem/cert.pem/key.pem (Docker SDK) and
# certs.d wants ca.crt/client.cert/client.key (dockerd registry convention); those
# cannot be renamed. dockerd's own --tls* paths and zot's http.tls.* paths are free,
# so the worker and registry keep the standardized names at rest. See the deployment
# map printed at the end.
#
# Re-running reuses existing CAs in $OUT, so leaves can be re-issued (or new ones
# added) without re-deploying either CA across the fleet.
#
# SECURITY: docker-ca-key.pem and zot-ca-key.pem never leave this machine — they are
# deployed to NO box. zot must keep anonymousPolicy/defaultPolicy empty; the CNs above
# are the only identities (users "cmgr" and "worker" in zot's config.json).
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

# make_ca <domain> <CN> — create <domain>-ca-cert.pem/<domain>-ca-key.pem if absent, else reuse.
make_ca() {
  local domain="$1" cn="$2"
  if [[ -f "$domain-ca-cert.pem" && -f "$domain-ca-key.pem" ]]; then
    echo "== CA $domain (reusing existing) =="
  else
    echo "== CA $domain =="
    openssl genrsa -out "$domain-ca-key.pem" 4096
    openssl req -new -x509 -days "$DAYS" -key "$domain-ca-key.pem" -sha256 \
      -out "$domain-ca-cert.pem" -subj "/CN=$cn"
  fi
}

# issue <domain> <role> <subject-CN> <ext-lines...>
# writes <domain>-<role>-cert.pem / <domain>-<role>-key.pem, signed by the <domain> CA.
issue() {
  local domain="$1" role="$2" cn="$3"; shift 3
  local key="$domain-$role-key.pem" crt="$domain-$role-cert.pem"
  rm -f "$key" "$crt"
  openssl genrsa -out "$key" 4096
  openssl req -new -key "$key" -out leaf.csr -subj "/CN=$cn"
  printf '%s\n' "$@" > leaf-ext.cnf
  openssl x509 -req -days "$DAYS" -sha256 \
    -in leaf.csr -CA "$domain-ca-cert.pem" -CAkey "$domain-ca-key.pem" -CAcreateserial \
    -out "$crt" -extfile leaf-ext.cnf
  rm -f leaf.csr leaf-ext.cnf
}

# SAN type for the registry leaf: IP for a v4 address, DNS otherwise
if [[ "$REGISTRY" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  REGISTRY_SAN="IP:$REGISTRY,IP:127.0.0.1"
else
  REGISTRY_SAN="DNS:$REGISTRY,IP:127.0.0.1"
fi

make_ca docker cmgr-docker-ca
make_ca zot    cmgr-zot-ca

echo "== [docker] worker dockerd server cert (shared, ServerName $WORKER_NAME) =="
issue docker server "$WORKER_NAME" \
  "subjectAltName = DNS:$WORKER_NAME" \
  "extendedKeyUsage = serverAuth"

echo "== [docker] cmgr client cert (cmgrd -> worker dockerd) =="
issue docker client "cmgr" \
  "extendedKeyUsage = clientAuth"

echo "== [zot] zot server cert (registry @ $REGISTRY) =="
issue zot server "$REGISTRY" \
  "subjectAltName = $REGISTRY_SAN" \
  "extendedKeyUsage = serverAuth"

echo "== [zot] cmgr client cert (orchestrator push + cmgrd delete, read-write) =="
issue zot client "cmgr" \
  "extendedKeyUsage = clientAuth"

echo "== [zot] worker client cert (image pull, read-only) =="
issue zot worker "worker" \
  "extendedKeyUsage = clientAuth"

echo "== lock down + clean up =="
chmod 0400 docker-ca-key.pem zot-ca-key.pem \
  docker-server-key.pem docker-client-key.pem \
  zot-server-key.pem zot-client-key.pem zot-worker-key.pem
chmod 0444 docker-ca-cert.pem zot-ca-cert.pem \
  docker-server-cert.pem docker-client-cert.pem \
  zot-server-cert.pem zot-client-cert.pem zot-worker-cert.pem
rm -f docker-ca-cert.srl zot-ca-cert.srl

echo "done -> $(pwd)"
ls -l

cat <<EOF

Deployment map (CERTS.D = /etc/docker/certs.d/$REGISTRY:5000)
----------------------------------------------------------------------
OFFLINE — deployed to NO box:
  docker-ca-key.pem       the docker CA private key
  zot-ca-key.pem          the zot CA private key

ORCHESTRATOR
  docker-ca-cert.pem      -> \$DOCKER_CERT_PATH/ca.pem      (verify worker server cert)
  docker-client-cert.pem  -> \$DOCKER_CERT_PATH/cert.pem
  docker-client-key.pem   -> \$DOCKER_CERT_PATH/key.pem
  zot-ca-cert.pem         -> CERTS.D/ca.crt                (verify zot server cert)
  zot-client-cert.pem     -> CERTS.D/client.cert
  zot-client-key.pem      -> CERTS.D/client.key

EACH WORKER
  docker-ca-cert.pem      -> dockerd --tlscacert           (verify cmgrd client cert)
  docker-server-cert.pem  -> dockerd --tlscert   (listen tcp://0.0.0.0:2376, --tlsverify)
  docker-server-key.pem   -> dockerd --tlskey
  zot-ca-cert.pem         -> CERTS.D/ca.crt                (verify zot server cert)
  zot-worker-cert.pem     -> CERTS.D/client.cert
  zot-worker-key.pem      -> CERTS.D/client.key

REGISTRY BOX (zot)
  zot-ca-cert.pem         -> http.tls.cacert               (verify cmgr/worker client certs)
  zot-server-cert.pem     -> http.tls.cert
  zot-server-key.pem      -> http.tls.key
----------------------------------------------------------------------
Trust split: the worker dockerds trust ONLY the docker CA; zot trusts ONLY the zot CA.
A leaked zot-worker cert therefore grants zot read but cannot control any dockerd.
Config-file PATHS are unchanged; only the bundle filenames are standardized, plus the
orchestrator holds two distinct cmgr client certs (docker-client + zot-client).
EOF
