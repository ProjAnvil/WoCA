# Generic woca role image, baked from a per-role distribution package
# produced by `make <role>` (GOOS=linux). Build context: build/dist, with
# the package passed by name:
#
#   docker build -f build/dist/Dockerfile \
#     --build-arg ROLE_PKG=woca-scheduler.tar.gz -t woca-scheduler:local build/dist
#
# or via `make dist-images`. The packaged start.sh is the entrypoint in
# foreground mode: it exec's the binary as PID 1, so docker stop reaches
# the process with SIGTERM — the graceful webfx shutdown. Role arguments
# (e.g. `serve --addr :8080`) ride the compose `command:`, which start.sh
# passes through to the binary.
ARG ROLE_PKG
FROM alpine:3.22
ARG ROLE_PKG
RUN apk add --no-cache ca-certificates
COPY "${ROLE_PKG}" /tmp/pkg.tar.gz
RUN mkdir -p /opt/woca && \
    tar -xzf /tmp/pkg.tar.gz -C /opt/woca --strip-components=1 && \
    rm /tmp/pkg.tar.gz && \
    chmod +x /opt/woca/start.sh /opt/woca/stop.sh
WORKDIR /opt/woca
ENV WOCA_SVC_MODE=foreground
ENTRYPOINT ["./start.sh"]
