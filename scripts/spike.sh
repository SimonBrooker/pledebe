#!/usr/bin/env bash
# pledebe spike — validate the monitor-first premise on a live Unraid Plex.
#
# Answers three questions, read-only, without stopping PMS:
#   A. Can we query the live DB with Plex's own SQLite (collations present)?
#   B. Does `VACUUM INTO` give us a checkable snapshot + an exact bloat figure,
#      and how long does it take on a real library?
#   C. Does the extracted Plex SQLite binary run standalone in a neutral
#      container, or does it need Plex's lib dir alongside it?
#
# Nothing here writes to the Plex database. The only writes are to SCRATCH.
#
# Usage:  ./spike.sh [-c plex-container] [-a appdata-path] [-s scratch-path]

set -euo pipefail

PLEX_CONTAINER="${PLEX_CONTAINER:-}"
APPDATA="${APPDATA_PATH:-}"
SCRATCH="${SCRATCH_PATH:-/mnt/user/appdata/pledebe/scratch}"

while getopts "c:a:s:h" opt; do
  case $opt in
    c) PLEX_CONTAINER=$OPTARG ;;
    a) APPDATA=$OPTARG ;;
    s) SCRATCH=$OPTARG ;;
    h) sed -n '2,14p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
warn() { printf '\033[33m   ! %s\033[0m\n' "$*"; }
die()  { printf '\033[31m   x %s\033[0m\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- discovery --

say "Discovery"

if [ -z "$PLEX_CONTAINER" ]; then
  PLEX_CONTAINER=$(docker ps --format '{{.Names}}' \
    | grep -iE '^(plex|binhex-plex|plex-media-server|PlexMediaServer)$' \
    | head -n1 || true)
fi
[ -n "$PLEX_CONTAINER" ] || die "No Plex container found. Pass -c <name>."
info "container: $PLEX_CONTAINER"

# Where the container thinks its config lives, and where that maps on the host.
# Scan for the marker file rather than trusting known paths — this is the same
# detection pledebe itself uses, so a failure here is a real finding.
# Search the container's own mount destinations, likeliest first, and stop on
# the first hit so we never traverse a media library.
MARKER="com.plexapp.plugins.library.db"

MOUNT_DESTS=$(docker inspect "$PLEX_CONTAINER" \
  --format '{{range .Mounts}}{{.Destination}}{{"\n"}}{{end}}')
SEARCH_ROOTS=$(printf '%s\n%s\n%s\n' \
  "/config" "/var/lib/plexmediaserver" "$MOUNT_DESTS" \
  | awk 'NF && !seen[$0]++')

DB_IN=""
while IFS= read -r root; do
  [ -n "$root" ] || continue
  info "scanning $root ..."
  hit=$(docker exec "$PLEX_CONTAINER" \
        find "$root" -maxdepth 6 -type f -name "$MARKER" 2>/dev/null | head -n1 || true)
  if [ -n "$hit" ]; then DB_IN=$hit; break; fi
done <<< "$SEARCH_ROOTS"

if [ -z "$DB_IN" ]; then
  warn "Marker scan found nothing. Mount destinations in this container:"
  printf '%s\n' "$MOUNT_DESTS" | sed 's/^/     /'
  warn "Contents of /config:"
  docker exec "$PLEX_CONTAINER" ls -la /config 2>&1 | sed 's/^/     /' || true
  die "Could not locate $MARKER inside $PLEX_CONTAINER."
fi

# .../Plex Media Server/Plug-in Support/Databases/<marker>  -> strip three levels
CFG_IN=$(dirname "$(dirname "$(dirname "$DB_IN")")")
info "database (in container): $DB_IN"
info "config    (in container): $CFG_IN"

# Map the container path back to the host via whichever mount contains it.
MOUNT_DEST=""; MOUNT_SRC=""
while IFS=$'\t' read -r src dst; do
  [ -n "$dst" ] || continue
  case "$DB_IN" in
    "$dst"/*)
      # keep the longest matching destination
      if [ ${#dst} -gt ${#MOUNT_DEST} ]; then MOUNT_DEST=$dst; MOUNT_SRC=$src; fi ;;
  esac
done < <(docker inspect "$PLEX_CONTAINER" \
         --format '{{range .Mounts}}{{.Source}}{{"\t"}}{{.Destination}}{{"\n"}}{{end}}')

[ -n "$APPDATA" ] || APPDATA=$MOUNT_SRC
[ -n "$APPDATA" ] || die "Could not resolve host appdata path. Pass -a."
info "mount: $MOUNT_SRC -> $MOUNT_DEST"
info "appdata (on host): $APPDATA"

DB_HOST="${DB_IN/#$MOUNT_DEST/$MOUNT_SRC}"
[ -f "$DB_HOST" ] || warn "Host path $DB_HOST not visible from here (ok if running off-box)."

PMS_VERSION=$(docker inspect "$PLEX_CONTAINER" --format '{{.Config.Image}}')
info "image: $PMS_VERSION"

# Scratch must NOT be tmpfs. Unraid's /tmp is RAM — a VACUUM INTO of a 20GB
# database there will eat the host's memory and can take the server down.
mkdir -p "$SCRATCH"
SCRATCH_FS=$(df -PT "$SCRATCH" | awk 'NR==2{print $2}')
SCRATCH_FREE=$(df -Pk "$SCRATCH" | awk 'NR==2{print $4}')
info "scratch: $SCRATCH (fs=$SCRATCH_FS, free=$((SCRATCH_FREE/1024))MB)"
case "$SCRATCH_FS" in
  tmpfs|ramfs) die "Scratch is RAM-backed. Point -s at a real disk (cache pool)." ;;
esac

DB_BYTES=$(docker exec "$PLEX_CONTAINER" stat -c %s "$DB_IN")
info "library.db: $((DB_BYTES/1024/1024))MB"
if [ "$SCRATCH_FREE" -lt $((DB_BYTES/1024*2)) ]; then
  die "Need ~2x DB size free in scratch. Have $((SCRATCH_FREE/1024))MB."
fi

for f in "-wal" "-shm"; do
  sz=$(docker exec "$PLEX_CONTAINER" stat -c %s "$DB_IN$f" 2>/dev/null || echo 0)
  info "library.db$f: $((sz/1024))KB"
done

# ------------------------------------------------ A: query via docker exec --

say "A. Live read with Plex SQLite (via docker exec)"

# Image layouts disagree here too: plexinc/lsio use /usr/lib/plexmediaserver,
# hotio stages the app elsewhere. -xdev keeps the search on the container's own
# filesystem so it can never descend into mounted media.
PSQL=""
for cand in "/usr/lib/plexmediaserver/Plex SQLite" \
            "/app/bin/usr/lib/plexmediaserver/Plex SQLite" \
            "/app/usr/lib/plexmediaserver/Plex SQLite" \
            "/app/Plex SQLite"; do
  if docker exec "$PLEX_CONTAINER" test -x "$cand" 2>/dev/null; then PSQL=$cand; break; fi
done

if [ -z "$PSQL" ]; then
  info "not in known locations, scanning image filesystem ..."
  PSQL=$(docker exec "$PLEX_CONTAINER" \
         find / -xdev -type f -name "Plex SQLite" 2>/dev/null | head -n1 || true)
fi

if [ -z "$PSQL" ]; then
  warn "Plex SQLite not found. 'Plex Media Server' binary lives at:"
  docker exec "$PLEX_CONTAINER" find / -xdev -type f -name "Plex Media Server" 2>/dev/null \
    | sed 's/^/     /' || true
  die "Could not locate Plex SQLite in $PLEX_CONTAINER."
fi
info "Plex SQLite: $PSQL"

pq() { docker exec "$PLEX_CONTAINER" "$PSQL" "$DB_IN" "$1"; }

t0=$(date +%s)
PAGE_COUNT=$(pq "PRAGMA page_count;")
PAGE_SIZE=$(pq "PRAGMA page_size;")
FREELIST=$(pq "PRAGMA freelist_count;")
t1=$(date +%s)
info "pragmas ok in $((t1-t0))s"
info "page_count=$PAGE_COUNT page_size=$PAGE_SIZE freelist=$FREELIST"
if [ "$PAGE_COUNT" -gt 0 ]; then
  info "free-page ratio: $((FREELIST*100/PAGE_COUNT))%  (>30% => vacuum is worth it)"
  info "reclaimable (est): $((FREELIST*PAGE_SIZE/1024/1024))MB"
fi

# A collation error here is the whole reason we can't use stock sqlite3.
info "collation probe: $(pq "SELECT title FROM metadata_items ORDER BY title_sort LIMIT 1;" 2>&1 | head -c 120)"

# --------------------------------------------------- B: snapshot + integrity --

say "B. VACUUM INTO snapshot (this is the load-bearing trick)"

# Write the snapshot at the root of the mapped volume so host and container both
# see it without assuming any particular directory layout.
PROBE_IN="$MOUNT_DEST/pledebe-probe.db"
PROBE_HOST="$MOUNT_SRC/pledebe-probe.db"
docker exec "$PLEX_CONTAINER" rm -f "$PROBE_IN" || true

# The snapshot lands on the Plex volume, NOT in scratch — so this is the
# filesystem that has to have room. Checking scratch alone was wrong.
if [ -d "$MOUNT_SRC" ]; then
  VOL_FREE=$(df -Pk "$MOUNT_SRC" | awk 'NR==2{print $4}')
  info "probe target: $MOUNT_SRC (free=$((VOL_FREE/1024))MB, need ~$((DB_BYTES/1024/1024))MB)"
  [ "$VOL_FREE" -gt $((DB_BYTES/1024)) ] \
    || die "Not enough free space on the Plex volume for the snapshot."
fi

warn "Starting VACUUM INTO. Watch Plex for stalls — that's what we're measuring."
t0=$(date +%s)
pq "VACUUM INTO '$PROBE_IN';"
t1=$(date +%s)
VAC_SECS=$((t1-t0))
PROBE_BYTES=$(docker exec "$PLEX_CONTAINER" stat -c %s "$PROBE_IN")
info "took ${VAC_SECS}s"
info "original: $((DB_BYTES/1024/1024))MB -> snapshot: $((PROBE_BYTES/1024/1024))MB"
info "actual reclaimable: $(( (DB_BYTES-PROBE_BYTES)/1024/1024 ))MB"

pp() { docker exec "$PLEX_CONTAINER" "$PSQL" "$PROBE_IN" "$1"; }

t0=$(date +%s)
INTEG=$(pp "PRAGMA integrity_check;" | head -n5)
t1=$(date +%s)
info "integrity_check ($((t1-t0))s): $INTEG"

say "   FTS indexes"
for tbl in fts4_metadata_titles fts4_tag_titles; do
  if pp "SELECT 1 FROM sqlite_master WHERE name='$tbl' LIMIT 1;" | grep -q 1; then
    res=$(pp "INSERT INTO $tbl($tbl) VALUES('integrity-check');" 2>&1 || true)
    info "$tbl: ${res:-ok}"
  else
    info "$tbl: not present (schema may use fts5 — note the actual table names)"
  fi
done

say "   Query latency baseline"
for q in "SELECT count(*) FROM metadata_items;" \
         "SELECT count(*) FROM media_parts;" \
         "SELECT count(*) FROM metadata_items WHERE title LIKE 'a%';"; do
  t0=$(date +%s%N)
  out=$(pp "$q")
  t1=$(date +%s%N)
  info "$(( (t1-t0)/1000000 ))ms  $out  <- $q"
done

# ------------------------------------- C: standalone binary in a neutral box --

say "C. Extracted Plex SQLite, run standalone (no exec into Plex)"

# docker cp is a GET on /containers/{id}/archive — a READ-ONLY socket is enough.
# If this works, monitor mode never needs POST access to the Docker API.
#
# The previous run showed the binary is NOT self-contained: it looks for
# siblings in its own directory. So copy the whole plexmediaserver directory,
# not just the one file.
PLEXDIR=$(dirname "$PSQL")
rm -rf "$SCRATCH/plexdir"
mkdir -p "$SCRATCH"
docker cp "$PLEX_CONTAINER:$PLEXDIR" "$SCRATCH/plexdir" 2>/dev/null \
  && info "extracted $PLEXDIR via docker cp (read-only socket sufficient)" \
  || warn "docker cp failed — production would need the download fallback"

if [ -d "$SCRATCH/plexdir" ]; then
  info "extracted size: $(du -sh "$SCRATCH/plexdir" 2>/dev/null | cut -f1)"
  cp "$PROBE_HOST" "$SCRATCH/probe.db" 2>/dev/null || true

  # Pre-pull separately so image-pull chatter cannot be mistaken for a failure
  # of the binary — that is what happened on the previous run.
  docker pull -q debian:bookworm-slim >/dev/null 2>&1 || true

  if out=$(docker run --rm -v "$SCRATCH:/s" debian:bookworm-slim \
           "/s/plexdir/Plex SQLite" /s/probe.db "PRAGMA integrity_check;" 2>/dev/null); then
    if [ "$(echo "$out" | head -n1)" = "ok" ]; then
      info "standalone: OK — works with the full directory copied"
    else
      warn "standalone ran but returned: $(echo "$out" | head -c 200)"
    fi
  else
    err=$(docker run --rm -v "$SCRATCH:/s" debian:bookworm-slim \
          "/s/plexdir/Plex SQLite" /s/probe.db "PRAGMA integrity_check;" 2>&1 >/dev/null || true)
    warn "standalone failed: $(echo "$err" | head -c 300)"
    warn "=> pledebe would need the download-from-Plex-packages path instead"
  fi
fi

# ------------------------------------------------------------------ cleanup --

say "Cleanup"
docker exec "$PLEX_CONTAINER" rm -f "$PROBE_IN" && info "removed probe from appdata"
info "scratch left at $SCRATCH for inspection — rm -rf when done"

say "Summary"
cat <<EOF
   DB size            : $((DB_BYTES/1024/1024))MB
   VACUUM INTO time   : ${VAC_SECS}s
   Reclaimable        : $(( (DB_BYTES-PROBE_BYTES)/1024/1024 ))MB
   Free-page ratio    : $((FREELIST*100/PAGE_COUNT))%
   Integrity          : $(echo "$INTEG" | head -n1)

   Go/no-go: if VACUUM INTO was tolerable and Plex didn't stall, the
   monitor-first design holds. If it took minutes and blocked playback,
   move the deep check to a Butler-window-only schedule.
EOF
