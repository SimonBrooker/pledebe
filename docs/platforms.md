# Platform support

pledebe monitors a Plex database. It does not need to *be* Plex, and it does not
need Plex to be in a container — but what it can *do* about a problem depends on
whether it can reach the process manager.

## Capability tiers

| Tier | What pledebe does | Requires |
|---|---|---|
| **Monitor** | metrics, integrity, FTS, bloat, log analysis, alerting, history | read access to the PMS config dir |
| **Guided repair** | diagnose, then emit the exact DBRepair command for *your* platform, with pre-flight checks already done | the above |
| **Automated repair** | stop PMS, run DBRepair, verify, restart | PMS in Docker + socket access |

**Monitor is the universal tier and the reason the project exists.** Automated
repair is a Docker-only convenience. Everywhere else we stop at guided repair —
which is honest: on a Synology package install we are not going to invent a
reliable way to stop PMS from inside a container, and pretending otherwise is how
you corrupt someone's library.

Guided repair covers the platforms `DBRepair.sh` itself runs on. **Windows PMS is
monitor-only** — see the Windows section below for why.

| Platform | Monitor | Guided repair | Automated repair |
|---|---|---|---|
| PMS in Docker (any host) | yes | yes | yes |
| Linux / macOS native | yes | yes | no |
| Synology SPK, QNAP QPKG, TrueNAS | yes | yes | no |
| Windows native | yes | not yet | no |

## Finding the database

Do not hardcode paths. pledebe scans each mounted root to a bounded depth for:

```
**/Plug-in Support/Databases/com.plexapp.plugins.library.db
```

That single marker identifies a PMS config dir on every platform, including
migrated, relocated and non-standard installs. The table below is *hints* for
documentation and for defaulting the compose file — not detection logic.

**Container images do not agree on layout.** Confirmed on a real Unraid host
2026-07-28: `hotio/plex` puts the database at `/config/Plug-in Support/Databases/`
— `/config` *is* the "Plex Media Server" directory, with no
`Library/Application Support/Plex Media Server` intermediate. lsio and plexinc
do have that intermediate. binhex uses `/config/Plex Media Server`. This is
exactly why detection scans for the marker and derives the config root by
stripping three levels from the hit, rather than joining known path fragments.

| Platform | PMS config root (mount this) |
|---|---|
| Unraid + hotio | `/mnt/user/appdata/<plex>` (flat — verified) |
| Docker, lsio / plexinc | `<config volume>/Library/Application Support/Plex Media Server` |
| Docker, binhex | `<config volume>/Plex Media Server` |
| Windows (native) | `%LOCALAPPDATA%\Plex Media Server` |
| Synology (native SPK) | `/volume1/PlexMediaServer/AppData/Plex Media Server` |
| Synology (Docker) | as Docker above |
| QNAP (native QPKG) | `/share/<datavol>/.qpkg/PlexMediaServer/Library/Plex Media Server` |
| TrueNAS SCALE (app) | under the app's `ix-applications` / config dataset |
| TrueNAS CORE (jail/plugin) | `/usr/local/plexdata/Plex Media Server` inside the jail |
| macOS | `~/Library/Application Support/Plex Media Server` |
| Linux (native pkg) | `/var/lib/plexmediaserver/Library/Application Support/Plex Media Server` |

Verify each of these against a real install before publishing them as
documentation — they are from memory and vendors move things between versions.
The scanner is what we actually rely on.

## Getting Plex SQLite

We need Plex's SQLite build, not stock sqlite3, because PMS installs custom
collations and `integrity_check` compares index entries using them. Three
acquisition paths, in preference order:

1. **`docker cp` from the PMS container** — exact version match. `docker cp` is a
   `GET /containers/{id}/archive`, so a **read-only** socket proxy is enough;
   monitor mode still never needs `POST`.

   Images disagree on where Plex is installed, so scan rather than assume.
   Verified locations:

   | Image | Plex SQLite |
   |---|---|
   | plexinc, lsio | `/usr/lib/plexmediaserver/Plex SQLite` |
   | hotio | `/app/bin/usr/lib/plexmediaserver/Plex SQLite` (verified 2026-07-28) |

   Fallback is `find / -xdev -type f -name "Plex SQLite"` — `-xdev` is essential,
   it keeps the search on the image filesystem and out of mounted media.

   **The binary is not self-contained.** Copying `Plex SQLite` alone fails: it
   looks for siblings in its own directory. You must copy the whole
   `plexmediaserver` directory. Verified 2026-07-28 on hotio/plex — the full
   directory is **218MB**, and with it the binary runs `PRAGMA integrity_check`
   correctly inside a stock `debian:bookworm-slim` container with no other
   setup, no `LD_LIBRARY_PATH`, and no Plex present.

   **Consequence for the image:** do not bake it in — 218MB, and it is Plex's to
   distribute. Extract once at first run into the `/data` volume, cache it, and
   re-extract when the detected PMS version changes. No attempt has been made to
   find a smaller working subset of that directory; 218MB on a persistent volume
   is acceptable and the pruning is not worth the fragility.
2. **Bind-mount the Plex install dir** — for native Linux installs, mount
   `/usr/lib/plexmediaserver` read-only.
3. **Download from Plex's package repo** — fetch the Linux PMS `.deb`/`.rpm` for
   the detected version and extract the binary on first run. This is the
   **required** path on Windows, Synology SPK and QNAP QPKG, where the host's
   Plex SQLite is either a Windows `.exe` or sits outside anything we can mount.

The database file format is portable, so a Linux Plex SQLite reads a database
written by Windows PMS without issue. Match the *version* as closely as you can
anyway — schema migrations are the risk window.

Do not bake Plex's binary into the published image. It is Plex's to distribute.

## Windows PMS specifically

The user runs PMS natively on Windows; pledebe runs in Docker Desktop on the same
box with a bind mount of `%LOCALAPPDATA%\Plex Media Server`. Notes:

- Plex SQLite on the host is `Plex SQLite.exe` and is unusable from a Linux
  container — acquisition path 3 is mandatory.
- Docker Desktop bind mounts go through a filesystem translation layer. **Never
  let SQLite write to the mount.** `VACUUM INTO` must target container-local
  scratch, and the mount should be `:ro`.
- **Windows is monitor-only.** Decided 2026-07-28. Guided repair would need
  ChuckPa's PowerShell variant vendored as a second script, with its own lock
  entry and its own contract tests — a maintained code path we cannot exercise
  on the dev Plex (Unraid). Windows users get full monitoring, alerting and
  history, and a link to DBRepair's own Windows instructions. Revisit if a
  Windows PMS install becomes available to test against.
- Stopping PMS is a tray-app/service action; we would instruct, never automate.

## Never monitor over SMB/NFS

It is tempting to point pledebe at a network share and monitor a remote Plex.
SQLite locking over SMB/NFS is unreliable, and a half-taken read lock against a
live database is exactly the failure we exist to prevent. Run pledebe on the same
host as PMS. If that is impossible, the answer is a small read-only agent on the
Plex host shipping metrics out — not a remote mount.

## Scratch space

`VACUUM INTO` needs roughly the size of the database, and DBRepair needs 2–3x.
Scratch must be on real disk, never tmpfs — **Unraid's `/tmp` is RAM**, and a
20GB snapshot there can take the host down. pledebe hard-gates on
`statvfs` free space and refuses to start a deep check that could fill the volume.
