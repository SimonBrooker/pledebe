#!/bin/sh
# Match the PUID/PGID convention used by linuxserver, hotio and binhex images,
# which is what NAS users expect.
#
# Without this, /data is created by Docker owned by root and a container running
# as a fixed uid cannot write its history database — the failure is an opaque
# "unable to open database file (14)".
#
# We start as root only to fix ownership, then drop privileges. pledebe itself
# never runs as root unless explicitly asked to.
set -e

PUID=${PUID:-1000}
PGID=${PGID:-1000}

if [ "$(id -u)" = "0" ]; then
    mkdir -p /data
    # Only /data. The Plex mounts are read-only and must not be touched.
    # -h changes symlinks themselves rather than following them: a symlink
    # planted in the data volume must not be able to redirect a root chown at
    # something outside it.
    chown -Rh "$PUID:$PGID" /data 2>/dev/null || \
        echo "pledebe: warning: could not chown /data; continuing" >&2

    exec su-exec "$PUID:$PGID" /usr/local/bin/pledebe "$@"
fi

# Already unprivileged (e.g. `docker run --user`). Run as-is and let pledebe
# report a permission problem plainly if /data is not writable.
exec /usr/local/bin/pledebe "$@"
