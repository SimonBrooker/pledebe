# Security review — OWASP Top 10 (2021)

Reviewed 2026-07-28 against the code at that date. Findings are ranked by real
risk in pledebe's actual deployment — a container on a home NAS beside Plex —
not by generic severity.

## Threat model

pledebe reads a Plex config directory and its own data directory, executes
Plex's SQLite binary as a subprocess, and serves one HTML page plus a small
JSON API. It has **no Docker socket, no outbound network calls, no user
accounts, and no write access to Plex's data**. That removes most of the
category in advance.

The assets worth protecting are: the `X-Plex-Token` in `Preferences.xml`, the
integrity of the Plex database, and the host's disk and CPU.

---

## A01 — Broken access control

**Finding: the status page had no authentication.** Anyone able to reach the
port could read filesystem paths, database geometry, Plex version and backup
locations, and could POST to `/deepcheck` to make the server read the entire
database.

**Fixed.** Optional HTTP basic auth via `PLEDEBE_USER` / `PLEDEBE_PASSWORD`,
constant-time comparison, applied to the page, the API and the deep-check
endpoint. `/healthz` stays open deliberately so container health checks work; it
reveals only that the process is alive.

Auth is **off by default**. Forcing credentials on a home-LAN NAS user mostly
produces a password taped to the wall, and many will front this with an
authenticating reverse proxy. Instead, pledebe logs a prominent warning at
startup when it binds beyond loopback with no credentials.

**Residual risk, accepted:** a default-open deployment on a trusted LAN. The
mitigation is documentation and the startup warning. **Never port-forward this
service.**

## A02 — Cryptographic failures

`Preferences.xml` contains `PlexOnlineToken`. The parser uses an explicit
attribute **whitelist**, so the token is never read into memory, never stored,
never logged and never rendered. A test asserts a token in the XML does not
survive parsing, and another asserts no secret appears in the HTTP response.

**No TLS.** Traffic, including basic-auth credentials, is plaintext. Acceptable
on a LAN; anything else needs a TLS-terminating reverse proxy. Documented rather
than solved, since terminating TLS in-process would mean certificate management
this project should not own.

## A03 — Injection

**SQL — pledebe's own database:** every statement uses bound parameters. No
string concatenation anywhere in `internal/store`.

**SQL — Plex's database:** several statements are built with `fmt.Sprintf`, but
the only interpolated values are table names from `ftsTables`, a compile-time
constant list. The one runtime value is the snapshot path in `VACUUM INTO`,
which is operator-supplied and passed through `escapeSQLiteString`.

*Invariant to preserve: no user- or content-derived value may ever reach those
format strings.*

**Command execution:** `exec.CommandContext` receives the binary and each
argument separately. **No shell is involved**, so shell metacharacters in a path
cannot be interpreted.

**HTML:** `html/template` with contextual auto-escaping. No `template.HTML`,
`template.JS` or `template.URL` anywhere, so escaping cannot be bypassed. This
matters because `integrity_check` output is rendered, and that string comes from
the database.

## A04 — Insecure design

The deep check reads an entire database and is exposed as an unauthenticated
POST by default — a resource-exhaustion vector. Mitigations: one run at a time,
a 60-second floor between manual runs, a free-space gate that withdraws the
button rather than failing mid-write, and a same-origin check.

**Design strength worth stating:** monitoring is read-only by construction. The
Plex mounts are `:ro`, and there is no repair code path in the binary at all, so
a compromise of pledebe cannot corrupt a Plex database.

## A05 — Security misconfiguration

**Fixed in `docker-compose.yml`:** `read_only: true`, `cap_drop: ALL`,
`no-new-privileges:true`, `tmpfs` for `/tmp`.

The entrypoint starts as root solely to `chown /data`, then drops to
`PUID:PGID` via `su-exec`; pledebe never serves traffic as root. The chown uses
`-h` so a symlink planted in the data volume cannot redirect it outside.

Security headers on every response: `Content-Security-Policy: default-src
'none'` with no `script-src` at all (the page uses no JavaScript),
`frame-ancestors 'none'`, `form-action 'self'`, plus `nosniff`, `no-referrer`,
`DENY` framing and `no-store`.

**Deliberate relaxations, 2026-07-30.** Adding a favicon and a PWA manifest
required `img-src 'self'` and `manifest-src 'self'`; `default-src 'none'` had
forbidden both. Recorded here so the change reads as a decision rather than
drift. Everything else remains denied, and there is still no `script-src` — a
test asserts that, so adding JavaScript has to be an explicit choice.

Icons and the manifest are served **unauthenticated**, even when basic auth is
configured. A browser requests `/favicon.ico` before it has credentials to
offer, and gating it produces an auth prompt for an icon. They are static
images embedded in the binary and reveal nothing about the server.

## A06 — Vulnerable and outdated components

One runtime dependency: `modernc.org/sqlite`, a pure-Go SQLite — no cgo, so no C
memory-safety surface.

`govulncheck ./...` reports **no vulnerabilities**, and now runs in CI on every
push, so a newly disclosed issue fails the build rather than waiting to be
noticed.

## A07 — Identification and authentication failures

No accounts, sessions, cookies, tokens or password reset — nothing to get wrong.
Basic auth, when enabled, is compared in constant time.

## A08 — Software and data integrity failures

Images are built in CI from a pinned Go toolchain and published by digest.

**Gap:** no SBOM, no build provenance attestation, no image signing. Worth
adding before the project is used by anyone other than its author.

**Planned and not yet built:** vendoring DBRepair (milestone 6) will fetch a
third-party script. The design already pins a commit SHA and verifies a SHA-256
at build time. That verification must not be skipped.

## A09 — Logging and monitoring failures

Operational logging goes to stdout for `docker logs`. Deep-check runs, failures
and collection errors are logged.

**Gap:** no audit trail for who triggered a manual deep check. With no user
identity there is little to record, but if auth becomes standard the
authenticated user should be logged with the action.

## A10 — Server-side request forgery

**Not applicable today** — pledebe makes no outbound requests.

**Future exposure, flagged now:** two planned features introduce it. Downloading
Plex SQLite from Plex's package repository must use a hardcoded host, not a
value from config. Probing Plex's `/search` API must be restricted to a
configured, validated PMS address and must never follow redirects to another
host.

---

## Summary

| | |
|---|---|
| Fixed in this pass | Optional auth, security headers, container hardening, CI vulnerability scanning, symlink-safe chown |
| Accepted risk | Auth off by default on a trusted LAN; no TLS in-process |
| Gaps to close | SBOM, build provenance, image signing; audit logging of triggered actions |
| Watch on future work | SSRF when outbound calls are added; checksum verification when DBRepair is vendored |

**The most important control is not in the code:** do not expose this service to
the internet. It is designed for a private network, and the startup warning
exists to make that hard to get wrong by accident.
