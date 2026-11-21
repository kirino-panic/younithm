#!/bin/sh
set -e

PUID="${PUID:-99}"
PGID="${PGID:-100}"

if [ "$(id -u)" = "0" ]; then
  mkdir -p "${YOUNITHM_CONFIG_DIR:-/config}"
  chown -R "$PUID:$PGID" "${YOUNITHM_CONFIG_DIR:-/config}"
  exec su-exec "$PUID:$PGID" "$@"
fi

exec "$@"
