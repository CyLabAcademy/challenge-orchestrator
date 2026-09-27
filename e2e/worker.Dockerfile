# syntax=docker/dockerfile:1
# The workers run production's firewall backend and its OCI runtime shim.
#
# DIND_VERSION is passed by compose.yaml from the same variable as the x-dind
# anchor, so the workers and the builder never drift apart.
ARG DIND_VERSION=29.6.0

# oci-interceptor is docker's `default-runtime` on a real cork worker, so every
# container corkd creates goes through it. A fleet without it exercises a
# different runtime chain than the one production runs -- and the chain is
# where the interesting failures have been: a wrapper that spawns the runtime
# instead of exec'ing it stays between containerd's shim and runc, and a
# cancelled `runc delete` then orphans the runtime and leaks its shim (~5 MiB
# each, 66 of them measured on a 2 GiB worker).
#
# Built from source rather than taken from the release tarball: the project
# publishes only x86_64-unknown-linux-gnu, with a glibc 2.39 floor, and these
# workers are Alpine. What is under test here -- that the interceptor is in the
# create path at all, and that its read-only networking mounts are applied --
# is the same in either build.
ARG OCI_INTERCEPTOR_REF=v0.3.0
FROM rust:alpine AS oci-interceptor
ARG OCI_INTERCEPTOR_REF
RUN apk add --no-cache git musl-dev
RUN cargo install --locked --git https://github.com/picoCTF/oci-interceptor \
      --tag "$OCI_INTERCEPTOR_REF" --root /out

FROM docker:${DIND_VERSION}-dind

# docker's iptables backend takes a lock per rule batch and network setup grows
# with the number of challenge networks on the box, which nftables does not.
# Docker 29.6.0 execs the nft binary rather than linking libnftables
# (moby#52886, which crashed on a netlink fd past 1024), and the docker:*-dind
# images carry iptables but not nft, so it is added here.
RUN apk add --no-cache nftables

# crun rather than runc under the interceptor, matching the role. Pinned and
# checksummed because this binary creates every container; the release assets are
# statically linked, so the glibc build runs on Alpine unchanged.
#
# Track upstream, not a distro archive. noble carries 1.14.1, which cannot parse
# Docker 29's ociVersion at all, and resolute carries 1.21, which is four CVE
# fixes behind -- including the rootfs /dev symlink family, reachable on a
# challenge whose rootfs is writable and whose instance gets restarted.
ARG CRUN_VERSION=1.30.1
ARG CRUN_SHA256=86d1e6a0e76945975d3aebfab39cbc6a26eea15f1c3fc66b6776d19e5dc346a0
RUN wget -qO /usr/local/bin/crun \
      "https://github.com/containers/crun/releases/download/${CRUN_VERSION}/crun-${CRUN_VERSION}-linux-amd64" \
 && echo "${CRUN_SHA256}  /usr/local/bin/crun" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/crun

# daemon.json names the wrapper, not the binary, and passes no runtimeArgs --
# the shape the multihost_docker ansible role deploys. Both halves matter:
# docker generates its own wrapper for any runtime whose runtimeArgs is
# non-empty, and that generated wrapper does not exec, which puts back exactly
# the process layer oci-interceptor v0.3.0 removed.
#
# --chmod rather than a mode carried on the file: this tree is edited from
# Windows, where git does not preserve the executable bit, and a wrapper that
# is not executable takes dockerd's every container create down with it.
COPY --from=oci-interceptor /out/bin/oci-interceptor /usr/local/bin/oci-interceptor
COPY --chmod=0755 worker/oci-interceptor-runtime.sh /usr/local/bin/oci-interceptor-runtime.sh
