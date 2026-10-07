# Tiny policy toolbox for the netlab driver: iptables in the docker VM's
# host network namespace. Docker 28/29 removed the inter-bridge
# DOCKER-ISOLATION-STAGE rules (the new DOCKER-FORWARD chain accepts
# traffic entering from any bridge), so cross-network container traffic
# flows by default — including into `internal` networks from outside.
# The netlab driver therefore enforces its zone matrix explicitly in
# DOCKER-USER, the chain docker documents for exactly this purpose (it
# is consulted before DOCKER-FORWARD and never rewritten by the daemon).
# On engines that still isolate bridges by default the rules are inert
# no-ops; on 28+/29 they restore the semantics the topologies assume.
FROM alpine:3.22
RUN apk add --no-cache iptables
ENTRYPOINT ["iptables"]
