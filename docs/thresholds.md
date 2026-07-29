# Where the thresholds come from

Every number that decides green, orange or red, with its actual provenance.
This document exists because the honest answer is uncomfortable: **two signals
have external grounding, and the rest are my judgement calibrated against one
healthy server.**

## Grounded

| Signal | Source | Confidence |
|---|---|---|
| `PRAGMA integrity_check` ≠ `ok` | SQLite itself. This is the database engine's own definitive verdict on whether its file is damaged. | **High.** Not a heuristic. |
| FTS `integrity-check` failure | [DBRepair's documentation](https://github.com/ChuckPa/DBRepair), which states FTS indexes corrupt while standard checks pass, and names the symptom. **Confirmed empirically**: detected on a live server, repaired with Reindex, verified fixed. | **High.** The only threshold validated end to end. |

## Provisional — my judgement, one server's worth of calibration

| Threshold | Value | Where it came from | Risk |
|---|---|---|---|
| Backup staleness | 8 days | Plex's default schedule is every 3 days; doubled plus slack, because a server busy through its Butler window legitimately skips one. | Too generous for a user on a daily schedule; too tight for one who disabled backups deliberately. |
| WAL "large" | 512 MB | General SQLite folklore. A healthy WAL is tens of MB. **Not measured, not sourced.** | A large library may run a legitimately larger WAL. Likeliest false positive in the codebase. |
| Integrity check staleness | 48 hours | Arbitrary. Long enough that a daily check never trips it. | Low impact — reports Unknown, never a fault. |
| Bloat, freelist ratio | 30% | Arbitrary. Chosen to be well clear of the 0.1% a healthy database showed. | Low impact — **never warns**, only prompts a measurement. |
| Bloat, measured | 25% reclaimable | Arbitrary. | Low impact — never warns. |
| Repair headroom | 3× database size | Stated early in this project's design and repeated since. **I have not verified this against DBRepair's actual requirements.** | If DBRepair needs more, we would greenlight a repair that runs out of space. Needs checking. |
| Snapshot headroom | 1.2× database size | Measured: the snapshot was 1125 MB for a 1139 MB database, so ~1× plus margin. | **Reasonable.** Grounded in observation. |
| Recent crash window | 14 days | Arbitrary but conventional. | Low impact. |

## What this means

The project's stated discipline is that no signal alerts until calibrated
against a healthy database. **Most of these thresholds do not meet that bar.**
They have been calibrated against exactly one server — a 1.1 GB library on
Unraid — and that server was healthy for all of them.

Two things limit the damage:

1. They are deliberately **generous** — 8 days rather than 4, 512 MB rather than
   100 MB. Erring toward silence, which matches the project's finding that false
   positives are the dominant risk.
2. The ones with the weakest grounding (both bloat thresholds) **cannot produce
   a warning at all**. The worst they do is suggest measuring properly.

## The better approach: per-server baselines

Absolute thresholds are the wrong shape for several of these. A 512 MB WAL might
be routine on a 20 GB library and alarming on a 500 MB one. "Large" is relative
to the server, not to a constant.

pledebe now keeps daily history indefinitely, which makes the alternative
available: compare a value to **that server's own trailing distribution**
instead of a fixed number. "The WAL is 6× its 30-day median" is both more
sensitive and less prone to false alarms than any constant, and it needs no
tuning per library size.

That should replace `walLarge` and the freelist ratio once enough history has
accumulated. It cannot replace `integrity_check` or the FTS check, which are
correctness questions, not statistical ones.

## Open items

- **Verify the 3× repair headroom** against what DBRepair actually requires.
  This is the only provisional threshold that could cause harm rather than
  noise, because it gates a destructive operation.
- **Replace `walLarge` with a baseline comparison** once servers have 30 days of
  history.
- **Collect calibration data from more than one server.** Everything here rests
  on a sample of one.
