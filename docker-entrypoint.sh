#!/bin/sh
# Seed the runtime config from the mounted seed file on first start,
# then run the proxy as the unprivileged muse user.
set -eu

state_dir=${STATE_DIR:-/var/lib/muse-proxy}
config_path=${CONFIG_PATH:-$state_dir/config.json}
config_seed_path=${CONFIG_SEED_PATH:-}
listen_address=${LISTEN_ADDRESS:-}

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$state_dir"
    chown muse:muse "$state_dir"
    exec su-exec muse:muse "$0" "$@"
fi

if [ "$#" -gt 0 ]; then
    exec "$@"
fi

if [ ! -f "$config_path" ]; then
    mkdir -p "$(dirname "$config_path")"
    if [ -n "$config_seed_path" ] && [ -f "$config_seed_path" ]; then
        cp "$config_seed_path" "$config_path"
        printf '%s\n' "Initialized $config_path from $config_seed_path."
    else
        printf '%s\n' "No config at $config_path and no seed; refusing to start." >&2
        exit 1
    fi
fi

if [ -n "$listen_address" ]; then
    export LISTEN="$listen_address"
fi
exec /usr/local/bin/muse-proxy -config "$config_path"
