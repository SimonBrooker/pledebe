#!/usr/bin/env bash
# pledebe — FTS triage (all four tables, including the ICU variants).
#
# Round 1 established that FTS4 `integrity-check` reports SQLITE_CORRUPT on a
# perfectly healthy database, and that count parity + MATCH probes are a sound
# replacement — but it only tested fts4_metadata_titles and fts4_tag_titles.
#
# The schema also carries ICU-tokenized variants. Two open risks:
#
#   FALSE NEGATIVE - if Plex's live search uses the _icu tables, a detector
#                    watching only the plain ones reports healthy while search
#                    is broken.
#   FALSE POSITIVE - the leading explanation for the integrity-check misfire is
#                    a tokenizer the CLI does not have registered the same way.
#                    That argument is *stronger* for ICU. If MATCH errors on
#                    _icu tables in a healthy database, our replacement detector
#                    fails the same way the first one did.
#
# The decisive test is the unicode round trip at the end: which table can find
# an accented/non-Latin title? That is the table Plex searches, and the table
# our detector must watch.
#
# Read-only. Writes only a snapshot, and removes it.
#
# Usage: ./fts-triage.sh [-c plex-container]

set -euo pipefail

PLEX_CONTAINER="${PLEX_CONTAINER:-}"
while getopts "c:h" opt; do
  case $opt in
    c) PLEX_CONTAINER=$OPTARG ;;
    h) sed -n '2,26p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
warn() { printf '\033[33m   ! %s\033[0m\n' "$*"; }
ok()   { printf '\033[32m   + %s\033[0m\n' "$*"; }
die()  { printf '\033[31m   x %s\033[0m\n' "$*" >&2; exit 1; }

[ -n "$PLEX_CONTAINER" ] || PLEX_CONTAINER=$(docker ps --format '{{.Names}}' \
  | grep -iE '^(plex|binhex-plex|plex-media-server)$' | head -n1 || true)
[ -n "$PLEX_CONTAINER" ] || die "No Plex container found. Pass -c <name>."

DB_IN=$(docker exec "$PLEX_CONTAINER" \
        find /config -maxdepth 6 -type f -name com.plexapp.plugins.library.db 2>/dev/null | head -n1)
[ -n "$DB_IN" ] || die "Database not found."

PSQL=""
for cand in "/usr/lib/plexmediaserver/Plex SQLite" \
            "/app/bin/usr/lib/plexmediaserver/Plex SQLite"; do
  docker exec "$PLEX_CONTAINER" test -x "$cand" 2>/dev/null && { PSQL=$cand; break; }
done
[ -n "$PSQL" ] || PSQL=$(docker exec "$PLEX_CONTAINER" \
                  find / -xdev -type f -name "Plex SQLite" 2>/dev/null | head -n1)
[ -n "$PSQL" ] || die "Plex SQLite not found."

MOUNT_DEST=$(docker inspect "$PLEX_CONTAINER" \
  --format '{{range .Mounts}}{{.Destination}}{{"\n"}}{{end}}' \
  | awk -v db="$DB_IN" 'index(db, $0"/")==1 { if (length($0)>length(m)) m=$0 } END{print m}')
PROBE="$MOUNT_DEST/pledebe-fts-probe.db"

info "container: $PLEX_CONTAINER"
info "database:  $DB_IN"

pq() { docker exec "$PLEX_CONTAINER" "$PSQL" "$DB_IN" "$1" 2>&1; }

# Deliberately never fails: integrity-check returns SQLITE_CORRUPT, and under
# `set -e` a non-zero command substitution in an assignment kills the script.
# We classify by parsing the text, so swallow the exit status here.
pp() { docker exec "$PLEX_CONTAINER" "$PSQL" "$PROBE" "$1" 2>&1 || true; }

say "Snapshot"
docker exec "$PLEX_CONTAINER" rm -f "$PROBE" || true
pq "VACUUM INTO '$PROBE';" >/dev/null
info "taken"
cleanup() { docker exec "$PLEX_CONTAINER" rm -f "$PROBE" 2>/dev/null || true; }
trap cleanup EXIT

TABLES="fts4_metadata_titles fts4_metadata_titles_icu fts4_tag_titles fts4_tag_titles_icu"

# ------------------------------------------------------------------ schema --

say "Definitions of every FTS virtual table (not just the ones we assumed)"
pp "SELECT sql FROM sqlite_master WHERE type='table' AND sql LIKE 'CREATE VIRTUAL TABLE%';" \
  | sed 's/^/     /'

# ---------------------------------------------------------- per-table check --

say "Per-table: integrity-check, counts, MATCH"
for t in $TABLES; do
  exists=$(pp "SELECT count(*) FROM sqlite_master WHERE name='$t';")
  if [ "$exists" != "1" ]; then
    warn "$t: not present"
    continue
  fi

  case $t in
    fts4_metadata_titles*) src=metadata_items ;;
    fts4_tag_titles*)      src=tags ;;
  esac

  ic=$(pp "INSERT INTO $t($t) VALUES('integrity-check');")
  cnt=$(pp "SELECT count(*) FROM $t;")
  scnt=$(pp "SELECT count(*) FROM $src;")
  m=$(pp "SELECT count(*) FROM $t WHERE $t MATCH 'the';")
  # The real index-side count. `SELECT count(*) FROM <fts>` reads the CONTENT
  # table for external-content FTS4, so it always equals the source — it is
  # tautological and detects nothing. _docsize has one row per INDEXED document.
  dcnt=$(pp "SELECT count(*) FROM ${t}_docsize;")

  printf '\n'
  info "$t"
  info "  integrity-check : ${ic:-ok}"
  info "  rows (content)  : $cnt   (source $src: $scnt)  <- tautological"
  info "  docs (indexed)  : $dcnt"
  if [ "$dcnt" = "$scnt" ]; then ok "  index parity    : exact"
  else warn "  index parity    : MISMATCH ($dcnt indexed vs $scnt rows)"; fi
  if echo "$m" | grep -qi "error\|malformed"; then
    warn "  MATCH 'the'     : $m"
  else
    info "  MATCH 'the'     : $m rows"
  fi
done

# ------------------------------------------------- the decisive unicode test --

# Round 2 found fts4_metadata_titles returning 0 rows for a title that exists,
# while its "parity" looked perfect. One sample is an anecdote — this measures
# a hit rate over many titles, which is the only check that has actually caught
# anything so far.
#
# Phrase match ('"some title"') avoids fragile token extraction and works the
# same for ASCII and non-ASCII. `tr '[:punct:]'` is byte-oriented and therefore
# UTF-8 safe: it only touches ASCII punctuation, leaving accented letters alone.
say "How many rows are legitimately unindexable?"
# A doc with no title produces no tokens and may never enter the index. Until we
# know this number, a _docsize gap means nothing — 3% missing and 3% NULL titles
# would be a perfectly healthy index.
MI=$(pp "SELECT count(*) FROM metadata_items;")
MI_NULL=$(pp "SELECT count(*) FROM metadata_items WHERE title IS NULL OR trim(title)='';")
TG=$(pp "SELECT count(*) FROM tags;")
TG_NULL=$(pp "SELECT count(*) FROM tags WHERE tag IS NULL OR trim(tag)='';")
info "metadata_items: $MI total, $MI_NULL with empty/NULL title"
info "  => expected indexed: $((MI - MI_NULL))"
info "tags:           $TG total, $TG_NULL with empty/NULL tag"
info "  => expected indexed: $((TG - TG_NULL))"
info ""
info "Compare against the 'docs (indexed)' figures above. A gap that matches the"
info "NULL count is healthy; a gap beyond it is missing documents."

roundtrip() {
  local label=$1 filter=$2 n=$3 order=${4:-RANDOM()}
  say "Round trip: $label (n=$n)"
  local total=0 hp=0 hi=0

  while IFS= read -r title; do
    [ -n "$title" ] || continue
    local term
    term=$(printf '%s' "$title" | tr '[:punct:]' ' ' | tr -s ' ' | sed 's/^ *//;s/ *$//')
    [ -n "$term" ] || continue
    total=$((total+1))

    local rp ri
    rp=$(pp "SELECT count(*) FROM fts4_metadata_titles     WHERE fts4_metadata_titles     MATCH '\"$term\"';")
    ri=$(pp "SELECT count(*) FROM fts4_metadata_titles_icu WHERE fts4_metadata_titles_icu MATCH '\"$term\"';")
    case $rp in ''|*[!0-9]*) rp=0 ;; esac
    case $ri in ''|*[!0-9]*) ri=0 ;; esac
    [ "$rp" -gt 0 ] && hp=$((hp+1))
    [ "$ri" -gt 0 ] && hi=$((hi+1))

    local mark="  "
    [ "$rp" -eq 0 ] && mark="!!"
    printf '   %s plain=%-5s icu=%-5s %.48s\n' "$mark" "$rp" "$ri" "$term"
  done < <(pp "SELECT title FROM metadata_items
                WHERE title IS NOT NULL AND length(title) BETWEEN 5 AND 60
                  AND $filter
                ORDER BY $order LIMIT $n;")

  printf '\n'
  if [ "$total" -eq 0 ]; then
    warn "no sample titles matched this filter"
    return
  fi
  if [ "$hp" -eq "$total" ]; then ok "plain: $hp/$total found"
  else warn "plain: $hp/$total found  <- index cannot find titles that exist"; fi
  if [ "$hi" -eq "$total" ]; then ok "icu:   $hi/$total found"
  else warn "icu:   $hi/$total found  <- index cannot find titles that exist"; fi
}

# GLOB '*[^ -~]*' matches anything outside printable ASCII.
#
# Round 3 sampled ORDER BY id, i.e. the OLDEST rows — the ones most likely to be
# correctly indexed. That 25/25 result was not evidence of a healthy index.
# Sample randomly, and sample the newest rows specifically, since recently added
# items are where a lagging index would show up first.
#
# Round 4 exposed a flaw in the harness: `tr '[:punct:]'` turns "Bernie's" into
# "Bernie s", injecting a bogus 's' token that phrase-match then demands. The
# plain and ICU tokenizers split apostrophes differently, so those titles
# produce zeros that are OUR bug, not index gaps. Tell: the non-ASCII sample
# scored 10/10 because it uses U+2019, which tr leaves untouched.
#
# So sample only titles that survive the harness unchanged: ASCII alphanumeric
# and spaces, no punctuation at all. Fewer candidates, but every zero is real.
ALNUM="title NOT GLOB '*[^A-Za-z0-9 ]*'"
NONASCII="title GLOB '*[^ -~]*' AND title NOT LIKE '%''%'"

roundtrip "clean ASCII, random" "$ALNUM"    20 "RANDOM()"
roundtrip "clean ASCII, newest" "$ALNUM"    20 "id DESC"
roundtrip "clean ASCII, oldest" "$ALNUM"    20 "id ASC"
roundtrip "non-ASCII, random"   "$NONASCII" 10 "RANDOM()"

# ----------------------------------------------------------------- log check --

say "Does PMS itself complain?"
LOGDIR=$(dirname "$(dirname "$(dirname "$DB_IN")")")/Logs
docker exec "$PLEX_CONTAINER" sh -c \
  "grep -ih 'malformed\|disk image is malformed' '$LOGDIR/Plex Media Server.log'* 2>/dev/null | tail -n 10" \
  | sed 's/^/     /' || true
info "(no output above = clean)"

say "How to read this"
cat <<'EOF'
   Which table found the unicode term tells you which one Plex searches, and
   therefore which one the detector must watch:

     only _icu found it        -> detector must watch the ICU tables
     both found it             -> watch both, prefer parity as primary signal
     _icu errored on MATCH     -> MATCH is NOT trustworthy on ICU tables.
                                  Fall back to count parity alone there, and
                                  say so in docs/signals.md.
     neither found it          -> either the term did not tokenize as expected,
                                  or search really is broken. Confirm in the
                                  Plex UI before concluding anything.

   Ground truth is still the Plex UI. Search for the sample title above.
EOF
