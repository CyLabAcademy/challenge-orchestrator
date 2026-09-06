# The workers program their firewall with nftables, like the production
# hosts: docker's iptables backend takes a lock per rule batch and network
# setup grows with the number of challenge networks on the box, which nftables
# does not. Docker 29.6.0 execs the nft binary rather than linking libnftables
# (moby#52886, which crashed on a netlink fd past 1024), and the docker:*-dind
# images carry iptables but not nft, so it is added here.
#
# DIND_VERSION is passed by compose.yaml from the same variable as the x-dind
# anchor, so the workers and the builder never drift apart.
ARG DIND_VERSION=29.6.0
FROM docker:${DIND_VERSION}-dind

RUN apk add --no-cache nftables
