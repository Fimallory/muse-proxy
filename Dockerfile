FROM alpine:3.22

RUN apk add --no-cache ca-certificates su-exec tzdata \
    && addgroup -S muse \
    && adduser -S -G muse -h /var/lib/muse-proxy muse \
    && mkdir -p /app /var/lib/muse-proxy \
    && chown -R muse:muse /app /var/lib/muse-proxy

COPY --chmod=0755 muse-proxy /usr/local/bin/muse-proxy
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint

ENV CONFIG_PATH=/var/lib/muse-proxy/config.json \
    CONFIG_SEED_PATH= \
    LISTEN_ADDRESS= \
    STATE_DIR=/var/lib/muse-proxy

WORKDIR /app

EXPOSE 3334

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:3334/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint"]
