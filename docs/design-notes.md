# pledebe

A Plex database health monitor that happens to have a repair button.

Plex databases rot quietly: search stops returning results, the library balloons
to tens of gigabytes, an unclean shutdown corrupts a page. You find out weeks
later. [DBRepair](https://github.com/ChuckPa/DBRepair) fixes all of that well —
but only once you already know something is wrong.

pledebe watches continuously, tells you *when* a repair is warranted and *which*
one, and keeps the history that turns "search feels slow" into "the FTS index
failed its check the night of the 14th, same night as an unclean shutdown".

Status: **pre-alpha, nothing works yet.** See [docs/platforms.md](docs/platforms.md).

## Design commitments

- **Monitor first, repair second.** The default deployment is read-only, has no
  Docker socket, and cannot damage anything.
- **Never touch the live database.** Deep checks run against a `VACUUM INTO`
  snapshot, which costs one read lock and yields an exact bloat figure for free.
- **Restore beats repair.** When a good dated backup exists, we say so before we
  offer to run a repair.
- **Guided, not magic, off Docker.** On Synology/QNAP/TrueNAS we diagnose and
  hand you the exact command. We do not invent ways to stop PMS. Windows PMS is
  monitored but not repaired — full detail, no repair path.
- **DBRepair is vendored and pinned.** Its menu output is our API; upstream bumps
  go through contract tests before they ship.
- **No signal alerts until it is calibrated against a healthy database.** The
  first detector we tried fired on a perfectly good library — see
  [docs/signals.md](docs/signals.md).

## Roadmap

| # | Milestone | Gate |
|---|---|---|
| 0 | ~~Spike — validate the snapshot premise on a real library~~ **done 2026-07-28** | see below |
| 1 | ~~Walking skeleton: metric poll, verdict, history db, one page~~ **done** | read-only |
| 2 | Log tailer, config-driven patterns, rotation handling | |
| 3 | Fixtures + CI, corrupt/bloated test databases | before any write path |
| 4 | Guided repair — per-platform command generation (Linux/NAS; Windows stays monitor-only) | |
| 5 | Automated repair — Docker only, opt-in profile, socket | explicit opt-in |
| 6 | `upstream-sync.yml`, contract tests, GHCR publish | |

## Running it

```bash
cp .env.example .env && docker compose up -d
```

Status page on `http://<host>:8080`. It collects every 15 minutes, keeps 90 days
of history, and never writes to your Plex data.

For a one-off report without starting the service, add `-once`:

```bash
docker run --rm -v "/path/to/plex/config:/plexconfig:ro" -v ./plexbin:/plexbin:ro ghcr.io/simonbrooker/pledebe:edge -once
```

## Step 0: run the spike

Everything above assumes you can snapshot a live Plex database cheaply. Prove it
before writing application code. On the Plex host:

```bash
./scripts/spike.sh -c plex -s /mnt/user/appdata/pledebe/scratch
```

It is read-only against Plex, times the snapshot, reports real vs reclaimable
size, runs integrity and FTS checks on the copy, and tests whether an extracted
Plex SQLite runs standalone.

### Result (hotio/plex on Unraid, 1139MB library, 2026-07-28)

| | |
|---|---|
| `VACUUM INTO` | **4s** — cheap enough to run nightly |
| `integrity_check` on the snapshot | 6s, `ok` |
| Query latency baseline | 57 / 55 / 66 ms |
| Reclaimable | 14MB actual vs 1MB predicted by freelist |
| Plex SQLite extraction | `docker cp` works; **read-only socket sufficient** |
| Standalone binary | works, but needs the **whole 218MB directory** |
| FTS `integrity-check` | fires on a healthy database — **rejected as a signal** |

**Verdict: go.** The monitor-first design holds. Two findings changed it — see
[docs/signals.md](docs/signals.md) for the FTS rejection and the replacement
detector, and [docs/platforms.md](docs/platforms.md) for the binary-acquisition
consequences.

## Licence

MIT — see [LICENSE](LICENSE).
