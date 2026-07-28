# Signals: what we alert on, and what we deliberately don't

> **CORRECTION (2026-07-28, later the same day).** This document previously
> concluded that FTS `integrity-check` was a false positive and instructed
> implementers not to build any FTS alert. **That conclusion was wrong and the
> instruction has been reversed.** See "FTS: the correction" below. The
> calibration reasoning that follows is preserved because the *method* was
> sound — the error was in what it tested, and that is worth keeping visible.

## FTS: the correction

Every test used to reject the FTS signal was a **read**: MATCH probes, row
counts, `_docsize` comparisons, and searching the Plex UI. They all passed, so
the failing integrity-check looked like noise.

[DBRepair's documentation](https://github.com/ChuckPa/DBRepair) states the
opposite, and names a symptom none of those tests exercise:

> FTS indexes can become corrupted even when standard integrity checks pass,
> causing operations like adding items to collections to fail with "database
> disk image is malformed" errors.

The failure is on **writes**. A corrupt FTS index reads perfectly and fails on
UPDATE. Testing search and concluding the index is healthy is a category error.

That reframes the evidence gathered during calibration. These were explained
away as tokenizer quirks; they are equally consistent with genuine corruption,
and the tool that specialises in these databases says corruption is what they
are:

- `integrity-check` failing on all four FTS tables
- `fts4_metadata_titles` missing ~4186 documents
- `fts4_tag_titles_icu` missing ~322,000 documents (61%)

**Decision: pledebe warns on FTS integrity failure.** The cost asymmetry is
decisive — staying silent on a real fault means the user hits collection-add
failures with no diagnosis, which is exactly what this product exists to
prevent, while a false alarm costs them a Reindex: fast, safe, and non
destructive. When the recommended action is that cheap, the bar for reporting
is lower than it would be for suggesting a repair.

The finding text says explicitly that searching still works and names the write
symptom, so a user who tests search does not conclude pledebe is broken.

**Method note:** the checks are run against the deep-check snapshot, never the
live database — `integrity-check` is issued as an `INSERT`.

---


Every signal here has to survive one test: does it fire on a healthy database?
The first one we tried did, which is why this document exists.

## FTS integrity-check — REJECTED as an alert (2026-07-28)

`INSERT INTO fts4_metadata_titles(fts4_metadata_titles) VALUES('integrity-check')`
looks like the perfect detector for the classic "search returns nothing but the
library is otherwise fine" failure. It is not usable.

**Calibration run**, hotio/plex on Unraid, 1139MB database, `PRAGMA
integrity_check` = ok:

| Check | Result |
|---|---|
| `integrity-check` on `fts4_metadata_titles` | **SQLITE_CORRUPT (11)** |
| `integrity-check` on `fts4_tag_titles` | **SQLITE_CORRUPT (11)** |
| `MATCH 'the'` | 35752 rows |
| `MATCH 'a*'` | 29096 rows |
| `MATCH 'star'` | 496 rows |
| `metadata_items` vs `fts4_metadata_titles` | 138179 = 138179 |
| `tags` vs `fts4_tag_titles` | 531045 = 531045 |
| PMS log lines mentioning malformed/disk image | 0 |
| Plex UI search | works |

Search is entirely healthy. The check reports corruption anyway.

Both tables are external-content FTS4:

```sql
CREATE VIRTUAL TABLE fts4_metadata_titles USING fts4(content='metadata_items', title, title_sort, original_title);
CREATE VIRTUAL TABLE fts4_tag_titles     USING fts4(content='tags', tag);
```

**Mechanism: unknown.** An earlier draft of this document blamed a missing
custom tokenizer. The full schema dump (2026-07-28) does not support that for
these two tables:

```sql
CREATE VIRTUAL TABLE fts4_metadata_titles     USING fts4(content='metadata_items', title, title_sort, original_title);
CREATE VIRTUAL TABLE fts4_metadata_titles_icu USING fts4(content='metadata_items', title, title_sort, original_title, tokenize=collating 'root@colStrength=primary;colAlternate=shifted');
```

The tables that failed carry **no `tokenize=` clause** — they use the default
`simple` tokenizer, which is always available. So tokenizer unavailability
cannot explain their failure. Something else about `integrity-check` against
external-content FTS4 is responsible, and we do not know what.

The tokenizer argument may still apply to the `_icu` tables, which do name a
custom `collating` tokenizer. That is what the round-2 triage tests.

**This does not change the decision.** The empirical result stands on its own:
the check fires on a database whose search demonstrably works. We reject it
because it is wrong, not because we understand why.

### Other Plex-specific structures in the schema

Worth knowing before any repair path is written — Plex leans on several SQLite
extensions, all of which live only in `Plex SQLite`:

```sql
CREATE VIRTUAL TABLE spellfix_metadata_titles USING spellfix1;
CREATE VIRTUAL TABLE spellfix_tag_titles      USING spellfix1;
CREATE VIRTUAL TABLE 'locations'              USING rtree('id' integer, 'lat_min' float, ...);
```

`spellfix1` backs typo-tolerant search, `rtree` backs location data. Neither is
in stock sqlite3 — another reason the vendored `Plex SQLite` is non-negotiable —
and both are candidates for their own health checks later.

Worth knowing: DBRepair uses the same CLI, so its own check may report this too.
Do not treat a DBRepair FTS complaint as confirmation of ours; they would share
the same root cause, not corroborate each other.

**Consequence:** an FTS integrity-check failure is logged as an observation and
never, on its own, raises an alert or recommends Reindex.

## FTS health — what we use instead

Three cheap checks that were all decisive on the calibration run:

### Count parity — ALSO REJECTED (2026-07-28, round 2)

An earlier version of this document made count parity the primary signal. It is
**tautological and detects nothing.**

For external-content FTS4, a `SELECT count(*)` without MATCH reads the *content
table*, not the index. So `count(fts4_metadata_titles)` returns
`count(metadata_items)` by construction. It cannot ever disagree.

Round 2 proved it empirically. On the same table, in the same run:

| Check | Result |
|---|---|
| `count(fts4_metadata_titles)` vs `count(metadata_items)` | 138181 = 138181, "exact" |
| MATCH for a title known to exist | **0 rows** |

Parity reported perfect health for an index that could not find an existing
title. A signal that says "healthy" during the exact failure we exist to catch
is worse than no signal.

**Replacement:** count the *index* side via the `_docsize` shadow table, which
holds one row per indexed document:

```sql
SELECT count(*) FROM fts4_metadata_titles_docsize;  -- indexed documents
SELECT count(*) FROM metadata_items;                -- rows that should be indexed
```

That comparison is meaningful. It is untested as of writing — round 3 measures
it.

### What we actually rely on

1. **Known-title round trip.** Sample N titles, phrase-MATCH each, measure the
   hit rate. This is the only check that has caught anything real so far. Use
   phrase queries rather than extracted tokens, and sample both ASCII and
   non-ASCII titles — they behave differently.
2. **Index-side document count** via `_docsize` vs the source table.
3. **MATCH probes** on common terms, asserting non-zero and no error. Weakest of
   the three: it passed on the table that could not find a real title.

Alert on 1. Treat 2 and 3 as corroboration until they have been calibrated.

**Do not compare MATCH counts between the plain and ICU tables.** They tokenize
differently and legitimately disagree by large factors — `MATCH 'the'` returned
35752 vs 56702 on the metadata tables, and 10128 vs 289 on the tag tables.

### ICU tables: risk closed

MATCH works on the ICU tables from the CLI, so the custom
`tokenize=collating 'root@colStrength=primary;colAlternate=shifted'` tokenizer
is available outside PMS. The feared "MATCH is untrustworthy on ICU" scenario
did not materialise.

All four FTS tables report the same `integrity-check` corruption, which
reinforces that the check is systemically wrong rather than describing real
damage to any particular index.

**Retracted (round 3):** round 2 concluded from a single sample that Plex
searches via the ICU tables and the plain index was stale. Round 3 does not
support it — the plain table found 25/25 sampled titles and the ICU table missed
one. One sample was an anecdote. Which index Plex actually queries remains
**unknown**, and nothing in pledebe should assume it.

## `_docsize` index parity — promising, not yet calibrated (round 3)

The first check to produce meaningful differentiation between tables:

| Table | Indexed | Source rows | Gap |
|---|---|---|---|
| `fts4_metadata_titles` | 133995 | 138181 | 3.0% |
| `fts4_metadata_titles_icu` | 138180 | 138181 | ~0% |
| `fts4_tag_titles` | 523105 | 531055 | 1.5% |
| `fts4_tag_titles_icu` | **208818** | 531055 | **60.7%** |

`fts4_tag_titles_icu` is a strong outlier, corroborated independently by
`MATCH 'the'` returning 289 against the plain table's 10128.

### Round 4 resolved both unknowns — the gaps are real

**NULL titles do not explain them.** `metadata_items` has 7493 empty/NULL
titles, yet `fts4_metadata_titles_icu` indexed 138180 of 138181 rows. It indexes
titleless rows anyway.

That also settles `_docsize` semantics: it holds one row per document
**inserted**, not per document that produced tokens. So the arithmetic is sound,
and the gaps are genuinely missing documents:

- `fts4_metadata_titles`: ~4186 documents absent
- `fts4_tag_titles_icu`: ~322000 documents absent (61%)

### Round-trip sampling was biased

Round 3 sampled `ORDER BY id LIMIT n` — the **oldest** rows, the ones most
likely to be correctly indexed. The 25/25 hit rate is therefore not evidence of
health. Round 4 samples randomly and additionally samples newest-first, since a
lagging index shows up in recent additions first.

**Standing methodology note:** when sampling a database to test an index, order
by RANDOM() or target the rows most likely to fail. Convenience ordering
silently selects for the healthy case.

### Round 4: the bias fix immediately found something

| Sample | plain | icu |
|---|---|---|
| ASCII random | 14/15 | 14/15 |
| **ASCII newest** | **8/15** | 13/15 |
| non-ASCII random | 10/10 | 10/10 |

`fts4_metadata_titles` finds 93% of randomly sampled titles but only 53% of the
most recently added ones. That is a **lagging index**, and it is invisible to
random sampling. Newest-first sampling is therefore not optional in the
detector — it is the sample that carries the signal.

### Harness bug: apostrophes manufacture false zeros

`tr '[:punct:]'` rewrites `Bernie's` to `Bernie s`, injecting a bogus `s` token
that phrase-match then requires. Plain and ICU tokenizers split apostrophes
differently, so affected titles return zeros that are ours, not the index's.

The tell: the non-ASCII sample scored 10/10 because those titles use U+2019,
which `tr` leaves alone. `He Came They Saw It s Bonkers` returning 0 on *both*
tables is this artifact.

The finding survives — the five newest-sample plain misses
(`My Grandfather Charles Manson`, `Hand Me That Scalpel`, `The Murder Pact`,
`Descendants Wicked Wonderland`, `Jacqueline Johnson Cabrera`) contain no
apostrophes. Round 5 samples only titles matching `[A-Za-z0-9 ]` so every zero
is real.

**Lesson for the detector:** never re-tokenize a title in application code to
build a query. Any transform we apply that the indexer did not apply produces
false failures. Sample values that need no transformation.

### The open question this cannot answer

A lagging *plain* index may be **completely harmless**. If Plex searches via the
ICU tables, users see correct results and there is nothing to fix — and alerting
would be a false alarm, the third in this document.

Only ground truth settles it. Search the Plex UI for titles the plain index
cannot find but ICU can:

- `My Grandfather Charles Manson`
- `The Murder Pact`
- `Descendants Wicked Wonderland`

**RESOLVED 2026-07-28: all three were found in the Plex UI.**

The plain index's lag is cosmetic. Plex does not depend on
`fts4_metadata_titles` for search, so its ~4186 missing documents and 53%
newest-sample hit rate are **not a fault** and must never raise an alert.

`fts4_metadata_titles` is therefore treated as vestigial. pledebe ignores it.

## Net result of the FTS investigation

Four candidate signals, none survived:

| Signal | Outcome |
|---|---|
| FTS `integrity-check` | fires on all four tables of a healthy DB |
| Count parity | tautological — cannot ever disagree |
| `_docsize` index parity | gaps are real but benign; ground truth says search works |
| Round-trip via SQLite MATCH | measures an index Plex may not even use |

Every check that inspects FTS internals failed for the same underlying reason:
**we do not know how Plex queries its own indexes.** It has four FTS tables plus
two `spellfix1` tables, and evidently blends or falls back among them. Measuring
one in isolation cannot predict what a user sees.

## The pivot: probe the real search path

Stop introspecting SQLite. Ask Plex.

PMS exposes `/search?query=...` over HTTP. A functional probe — pick a title
from `metadata_items`, search for it through the API, assert it comes back —
measures exactly what the user experiences, and is immune to every problem in
this document: tokenizers, index internals, which table is authoritative, and
our own term mangling.

- **Cheap.** One HTTP request against localhost.
- **Unambiguous.** A miss is a real, reproducible, user-visible failure.
- **Actionable.** A drop in hit rate is the Reindex case, stated in the user's
  own terms.

Sample newest-first — that is where the lag showed, and the finding carries over
even though the index it came from turned out not to matter.

**Caveat:** this needs the `X-Plex-Token` from `Preferences.xml`. That file is
already on the never-log/never-export list. Read it into memory, use it for
localhost requests only, and keep it out of logs, diagnostics and the UI.

Keep the SQLite checks as **diagnostic context** shown after a functional probe
fails — never as alert triggers on their own.

## The tag gap — also benign (resolved 2026-07-28)

`fts4_tag_titles_icu` indexes 208818 of 531055 rows (61% missing), corroborated
by `MATCH 'the'` returning 289 against the plain table's 10128.

Ground truth: an actor (`Brad Pitt`), a director (`James Cameron`) and a genre
(`Action`) all return results in the Plex UI. Tag search works. The gap does not
correspond to any user-visible failure, and is not alerted on.

**Residual uncertainty, accepted:** all three probes are high-frequency tags,
which are the likeliest to be present if missingness correlates with obscurity
or recency. A rare tag might still be unfindable. We are not pursuing it — five
consecutive database-side anomalies have now turned out benign, and the
functional-probe design makes the question moot: if tag search ever does break,
the API probe catches it directly. Revisit only if a user reports missing tags.

## Backup freshness — the alarm was ours (2026-07-28)

pledebe's first run reported "newest backup 91 days old" on a server whose
backups were running perfectly: every three days at 02:00, newest from
yesterday, both `library.db` and `blobs.db`.

Cause: `ButlerDatabaseBackupPath` had been set to `/backup/Databases`. We read
the directory beside the database, found four leftovers from before that setting
changed, and reported their age with total confidence.

A second bug hid the evidence. The log check globbed
`'Plex Media Server.log'*`, which matches only the current file — rotations are
named `Plex Media Server.1.log`. It reported zero Butler activity on a server
logging 19799 Butler lines in a single file, appearing to corroborate the false
alarm.

**Fixes:** read the configured path from `Preferences.xml` (whitelisted
attributes only — the file holds `PlexOnlineToken`), accept an explicit mount
for it, and glob `Plex Media Server*.log`.

**The rule this produces:** distinguish *absence of data* from *evidence of
failure*. If the configured backup directory is not visible in our mount
namespace, the honest output is "unknown — cannot see it", never "no backups".
Two bugs pointing the same way felt like corroboration and was not.

## Running tally

Things that looked like findings on a healthy server:

| # | Apparent finding | Reality |
|---|---|---|
| 1 | FTS `integrity-check` corruption ×4 tables | check is unreliable |
| 2 | Count parity exact | tautological |
| 3 | `_docsize` gaps up to 61% | benign |
| 4 | Plain index finds 53% of newest titles | benign, index unused |
| 5 | 171 crash reports | 171 PMS *version* directories |
| 6 | Backups 91 days stale | wrong directory |
| 7 | Butler never runs | broken log glob |

**Zero true positives so far.** Every alarming number has been a measurement
error or a benign quirk. The dominant risk in this product is not missing a real
fault — it is confidently reporting one that does not exist. Design accordingly:
prefer "unknown" over "broken", and calibrate every signal against a healthy
server before it can alert.

## Summary for implementers

Do not build any FTS alert on SQLite introspection. Five separate database-side
anomalies were investigated on a healthy library; all five were benign:

1. `integrity-check` reports corruption on all four FTS tables
2. Count parity is tautological
3. `fts4_metadata_titles` is missing ~4186 documents
4. That index finds only 53% of newest titles
5. `fts4_tag_titles_icu` is missing 61% of documents

Every one would have alarmed a user with nothing wrong. Search health is
measured through Plex's own `/search` API or not at all.

### Open item

The database also carries ICU variants — `fts4_metadata_titles_icu`,
`fts4_tag_titles_icu` — which the calibration run did not probe. Modern Plex
search likely uses these. Extend all three checks to cover them before milestone
1 ships, and re-run calibration.

## Signals confirmed useful

| Signal | Evidence |
|---|---|
| `VACUUM INTO` snapshot | 4s for 1139MB — cheap enough to run nightly |
| `PRAGMA integrity_check` on the snapshot | 6s, returned `ok`, exercises collations |
| Size delta original vs snapshot | 1139MB → 1125MB, i.e. 14MB reclaimable — exact, not estimated |
| `freelist_count / page_count` | 363 / 291757 = 0% — see caveat below |

### The cheap bloat signal under-reports

`freelist_count × page_size` estimated **1MB** reclaimable. The snapshot showed
the true figure was **14MB** — 14x higher. Free pages are only part of the story;
the rest is intra-page fragmentation that no pragma exposes.

So the frequent poll must treat the freelist ratio as a **floor, not an
estimate**. Trigger the deep check on a low threshold and let the snapshot
produce the number we actually report. Never show a freelist-derived figure to
the user as "reclaimable" — on this database it would have been wrong by an
order of magnitude, in the direction that hides a problem.

Both numbers are tiny here because the database is healthy. The ratio between
them on a genuinely bloated database is unknown and worth capturing when
milestone 3's bloated fixture exists.
| Query latency baseline | 54/55/68ms on three canonical queries |

## Standing rule

**Calibrate every new signal against a known-healthy database before it can
alert.** The FTS check would have shipped as a headline feature and fired on
every healthy Plex install. Milestone 3's fixtures exist to make this routine.
