# TeleMeetBot — Production Upgrade Engineering Audit

**Baseline commit:** `ccf7792` (Merge pull request #61 from Opselon/fix/cd-wait-self-match)
**Branch:** `upgrade/production-reliability`
**Scope:** Full-system audit of the Go runtime, streaming engine, media pipeline,
Telegram bot, web dashboard, persistence, security, CI/CD and documentation.
**Method:** Every finding below was verified against the actual code before it was
classified; each accepted defect carries a regression test proven to fail on the
pre-fix code. Unverifiable suspicions were rejected and are listed in
[Rejected / unverified claims](#rejected--unverified-claims).

---

## 1. Baseline

| Property | Value |
| --- | --- |
| Repository | `Opselon/TeleMeetBot-TeleMeetBot-implementation` |
| Primary branch | `main` |
| Go code | 58,156 lines across 15 `internal/` packages + `cmd/telemeetbot` |
| Toolchain | Go 1.26.8, rod v0.116.2, modernc.org/sqlite v1.61.0 |
| Build | `go build ./...` clean |
| `go vet ./...` | clean |
| Unit tests (`go test ./... -count=1`) | all 16 packages pass |
| Race suite (`go test -race ./... -count=1`) | **1 data race found** in `internal/engine` |
| Chromium integration tests | pass (`TELEMEETBOT_BROWSER=/snap/bin/chromium`) |
| Open PRs / issues | none |
| CI | `ci.yml` (gofmt, vet, unit, Chromium integration, cross-compile, installer smoke, upgrade path, website build/SEO/contrast/E2E, docker) |
| CD | `cd.yml` — waits on the exact-SHA CI run by workflow path, fails closed without secrets, `concurrency` cancels stale runs |

**Baseline note (environmental, not a defect):** `TestIntegrationWarmHandOffBeatsColdLaunch`
and `TestIntegrationWarmBrowserIsUsable` fail only when the temp profile directory
is unreachable from a snap-confined Chromium. On this host, `TMPDIR` points under
`/root/.hermes/cache/scratch`, which the snap cannot reach. They pass with
`TELEMEETBOT_PROFILE_DIR=/tmp/tmbprofiles`. No code change was made for them.

The race failure in the baseline `go test -race` run is a genuine defect and is
fixed in [E1](#e1).

---

## 2. Classification legend

- **P0** — Critical production failure or security vulnerability.
- **P1** — Major reliability or functional defect.
- **P2** — Performance, UX or maintainability problem.
- **P3** — Optional improvement.

Every entry states the exact file and code path, the reproduction scenario, the
root cause, the impact, the applied correction, and the regression test.

---

## 3. Accepted findings

### E1 — P1: `Session.Snapshot()` races the queue caption/quality fields

| | |
| --- | --- |
| Path | `internal/engine/session.go:248` — `Snapshot()` |
| Write side | `internal/engine/queue.go:236-237, 274-275, 307-308` — `Next()`, `Previous()`, auto-advance |
| Root cause | `Snapshot()` read `s.captionPolicy` and `s.captionLang` **after** releasing `queueMu.RLock()`, while `Next()`/`Previous()` write those same fields under `queueMu.Lock()` via `applyItemCaptionLocked` (`internal/engine/captions.go:220`). The read was outside the critical section that guards the writes. |
| Reproduction | `go test -race ./internal/engine -run TestSessionQueueConcurrency` — the race detector reports a write in `applyItemCaptionLocked` (via `Next`) racing a read in `Snapshot`, exactly the production pattern of a user pressing ⏭ while the refresh loop renders the card. |
| Impact | Undefined behaviour on the most concurrent path in the product: every dashboard card render, every `/api/state` poll and every bot refresh reads these fields while queue transitions write them. The race detector flagged it in the existing test; in production it manifests as torn policy/language values and, under `-race`, a crash. |
| Correction | Moved the `CaptionPolicy`/`CaptionLang` reads inside the existing `queueMu.RLock()` critical section (next to the `QualityPolicy` read that was already correctly placed there), keeping the nested `queueMu → mu` lock order unchanged. |
| Regression test | `TestSessionQueueConcurrency` under `-race` (pre-existing, now green). Verified: `-race -run TestSessionQueueConcurrency -count=5` passes; the full `-race` suite across all 16 packages passes. |
| Status | **Fixed** — commit `a5d6b46` |

### E2 — P1: `cleanup()` never closed `s.done`, so every stop reported failure

| | |
| --- | --- |
| Path | `internal/engine/session.go:541` — `cleanup()`; waited on by `internal/engine/manager.go:427-431` (`Manager.Stop`), `manager.go:440-445` (`StopAll`), `internal/dashboard/control.go:83`, and `internal/bot/flows.go:875,1002,1099` |
| Root cause | `cleanup()` is the single exit path of `run()` (deferred, so it also runs on a panic) but it never closed `s.done`. The only `close(s.done)` in the package was in the test-only seam `SetStopForTesting` (`session.go:1639`). Every waiter on `Done()` therefore blocked until its bounded deadline. |
| Reproduction | Start a session, press stop, watch the engine log: `Manager.Stop` blocks its full 15 seconds and then returns `session %s did not stop in time` for a session whose goroutine had already finished. Reproduced deterministically in the regression test. |
| Impact | The most common control operation in the product — stopping a stream — always timed out and produced a misleading production error. `StopAll` burned 5 s per session on SIGTERM. Combined with the error being surfaced, operators are told a healthy stop failed. |
| Correction | `cleanup()` now closes `s.done` exactly once, at the end of the function, guarded by a `sync.Once` (`doneOnce`) and nil-safe for hand-built sessions. Waiters now observe a fully released session. |
| Regression test | `internal/engine/done_test.go` — `TestManagerStopWaitsForCleanup` drives the production `cleanup()` through `Manager.Stop`'s wait and asserts `Done()` closes and the state is terminal. **Verified to fail on the pre-fix code** with the exact 15 s `did not stop in time` timeout (fix temporarily reverted). |
| Status | **Fixed** — commit `1249434` |

### E3 — P0: the download HTTP client had no SSRF gate at the dial layer

| | |
| --- | --- |
| Path | `internal/media/manager.go:151` — `NewManager` → `clientWithDialer(opts.HTTPTimeout, nil)`; gate implemented but unused at `internal/media/fetch.go:295` |
| Root cause | `NewManager` passed `nil` for the dial gate. The SSRF protection therefore existed only as the pre-flight DNS check `ValidateResolvedDestination` (`security.go:82`), which validates the host's *current* DNS records. The connection itself dialled whatever the resolver returned at connect time. The engine's own classification probe (`internal/engine/classify.go:160`) already had a `DialContext` gate — the downloader, the part that actually transfers attacker-supplied bytes, did not. |
| Reproduction | A Telegram-supplied media URL whose DNS crosses into a private range between the pre-flight resolve and the connect (DNS rebinding), or a host with mixed public/private records, dials into 127.0.0.1/10.x/172.16.x/192.168.x/169.254.169.254 while every layer above reports the URL as validated. The `ValidateResolvedDestination` verdict is stale the moment it returns. |
| Impact | Full SSRF into the host and its cloud metadata service through the largest attack surface in the product: every media URL any Telegram user sends. |
| Correction | `NewManager` now wires `m.dialGate`, which applies `IsBlockedIP` to the address being dialled, resolves hostnames through the same overridable lookup the pre-flight uses, and refuses unresolvable or all-private hosts (fail-closed). The client keeps `checkRedirect`, so redirect targets are re-validated at both layers. |
| Regression test | `internal/media/dialgate_test.go` — `TestDialGateBlocksPrivateDial` pins the dial-time resolution to the cloud-metadata address and asserts no connection is made and the error wraps `ErrUnsafeURL`; `TestDialGateOnPublicDial` proves the gate still permits a legitimate fixture. **Verified to fail on the pre-fix code** (gate removed → the dial proceeds and the test reports the server was never reached / the error does not wrap `ErrUnsafeURL`). |
| Status | **Fixed** — commit `705ef2d` |

### E4 — P0: `Client.Timeout` made every direct download die at 30 seconds

| | |
| --- | --- |
| Path | `internal/media/manager.go:151` + `internal/media/fetch.go:295` (`Client.Timeout = opts.HTTPTimeout`) and `fetch.go:37` |
| Root cause | The shared HTTP client was built with `Client.Timeout = opts.HTTPTimeout`, and nothing in production ever overrode `HTTPTimeout`, so it was always the 30 s default (`options.go:76`). `Client.Timeout` covers the **entire response body**, not per-read latency. Any direct media download slower than 30 seconds — a large file over a slow link, a throttled CDN — was cancelled as a client timeout, regardless of `download.timeout_seconds`. |
| Reproduction | Request a direct-media download whose body takes > 30 s. The fetch dies with `context deadline exceeded (Client.Timeout or context cancellation while reading body)` while `download.timeout_seconds` (default 600 s) claims the operation is allowed ten minutes. |
| Impact | Download & Stream is silently unusable for any large or slow direct media, and the operator's configured timeout is a lie — the effective limit is an undocumented 30 s. |
| Correction | The client no longer carries a body timeout; `ResponseHeaderTimeout` bounds waiting for headers instead. `fetchDirect` now runs the whole fetch under `opts.Timeout` — the documented per-download bound, the same limit yt-dlp already runs under — falling back to `HTTPTimeout` only when `Timeout` is unset. |
| Regression test | `internal/media/httptimeout_test.go` — `TestDirectDownloadOutlivesHTTPTimeout` serves a dribbled body slower than the pinned `HTTPTimeout` and asserts it still arrives complete; `TestDirectDownloadRespectsDownloadTimeout` proves the new bound is a real deadline (a never-finishing body dies at ~`opts.Timeout`, not at the 5 s `HTTPTimeout` and not ever). **Verified to fail on the pre-fix code** (client timeout restored → `fetchDirect failed … after 202ms: … Client.Timeout … while reading body`). |
| Status | **Fixed** — commit `705ef2d` |

### E5 — P1: the web dashboard login was dead code

| | |
| --- | --- |
| Path | `internal/dashboard/ui.go:134` (rendered page) vs `internal/dashboard/api.go:104` (`handleTelegramAuth`) |
| Root cause | The login page rendered the Telegram Login Widget with `data-onauth="onTelegramAuth(user)"` but the page's JavaScript never defined `onTelegramAuth`. The widget rendered and clicked, then threw `onTelegramAuth is not defined` the instant a user completed Telegram authorization — while a fully implemented HMAC-verifying backend (`VerifyTelegramAuth`, `internal/dashboard/telegram_login.go`) waited for a payload that never arrived. |
| Reproduction | Enable `dashboard.enabled`, open it in a browser, complete the Telegram authorization. Nothing happens; the network tab shows no request to `/api/auth/telegram`. Reproduced statically: `grep onTelegramAuth internal/dashboard/ui.go` shows only the `data-onauth` attribute, never a definition. |
| Impact | The entire opt-in web dashboard is unreachable through its only documented login flow. An operator who enables it concludes the feature is broken. |
| Correction | The page now defines `window.onTelegramAuth` **before** the widget script loads, so the iframe's callback is always present. It POSTs the signed widget fields to `/api/auth/telegram` as form data, reloads on success, and surfaces the backend's rejection reason on failure. The backend was already correct; no auth logic changed. |
| Regression test | `internal/dashboard/login_widget_test.go` — `TestLoginPageDefinesOnTelegramAuth` asserts the handler is defined, precedes the widget script, posts to the endpoint, and forwards the signed fields; `TestLoginPageWithoutBotUsername` keeps the disabled path honest. **Verified to fail on the pre-fix code** (definition stripped → `page references onTelegramAuth but never defines window.onTelegramAuth — web login is dead code`). |
| Status | **Fixed** — commit `17fd47f` |

---

## 4. Rejected / unverified claims

The following agent-raised claims were investigated and **rejected**, because the
code does not support them. Recording them prevents them from being re-litigated
without new evidence.

1. **"Client.Timeout is the effective 30 s download bound" → accepted as E4.** But the
   accompanying claim that `opts.Timeout` bounds the whole HTTP download was also
   checked: `opts.Timeout` was only used for yt-dlp/ffmpeg `runExec` and the janitor
   cutoff (`manager.go:820`), never for the HTTP path — which is exactly why E4's
   fix routes the HTTP fetch through it.
2. **"Shared-browser refcounting can leak Chromium"** — `manager.go:628-641`. The
   release closure decrements its captured copy of `refs`, and the guard
   `refs <= 0 && m.shared != nil` closes the shared browser exactly once. The
   captured-copy pattern is correct because `m.shared.refs` is only ever
   incremented under `sharedMu`. Not a defect.
3. **"Cancelling a session does not interrupt a running download"** —
   `internal/engine/download.go:198` calls `s.downloadMedia(context.Background(), …)`.
   This is deliberate: the download must complete (or hit its own `opts.Timeout`)
   for the session to have a local file to play; the session context is not the
   right cancellation bound for a download that outlives the share. Practical
   impact is bounded by the download deadline and the janitor. Recorded as a
   follow-up candidate, not a defect.
4. **"Telegram callback answers are missing"** — `flows.go:771` answers unknown
   callbacks and every dispatched branch answers; unauthorized callbacks are
   intentionally not answered (`flows.go:638-641`) to avoid a free denial-of-chat
   vector, which is the documented trade-off. Not a defect.
5. **"Unknown callback data reaches a dead default"** — `flows.go:771-773` handles
   it with `AnswerCallback("unknown action")`. Not a defect.
6. **`media/clientWithDialer`'s now-unused first parameter** — kept for signature
   stability across the test seam; renamed to `_`. Not a defect.

---

## 5. Corroborated strengths (no change required)

These were checked specifically because they are the areas a less careful audit
would have flagged. They are correct as implemented.

- **CD cannot deploy a stale image.** `cd.yml` waits for the `ci.yml` run of the
  exact push SHA, filtered by workflow path (the `workflow` query parameter is
  unreliable), and hard-fails if that run fails or the wait times out.
  Production incident 2026-10-27 (stale image) is addressed by this code.
- **SSRF redirect validation.** `checkRedirect` (`fetch.go:313`) re-runs
  `ValidateURL` and `ValidateResolvedDestination` on every redirect target —
  including bare-IP hosts — so redirects cannot be used to sidestep the gate.
- **SQLite stores are uniformly configured.** users/meetings/playlists each open
  with `busy_timeout(5000)`, `journal_mode(WAL)`, `foreign_keys(ON)` and
  `SetMaxOpenConns(1)`; migrations are versioned, idempotent and column-guarded
  against a crash between DDL and bookkeeping.
- **OAuth.** PKCE, HMAC-signed state with expiry, nonce validation, OIDC
  issuer/audience/expiry checks, AES-256-GCM token encryption with AAD, and
  revocation are all implemented (`internal/googleauth/oauth.go`, `crypto.go`).
- **Dashboard auth.** Telegram Login Widget HMAC verification, an
  HMAC-signed cookie, per-IP login rate limiting, and a fail-closed admin
  allow-list re-checked on every request (`requireAuth` also checks `IsAdmin`).
- **Bot authorization.** `authorizeUser` fails closed on a non-empty admin list.
- **Subprocess safety.** yt-dlp/ffmpeg run through `exec.CommandContext` with
  vetted argv (no shell), bounded output, timeouts and cancellation
  (`internal/media/ytdlp.go:489,500`).
- **Queue ownership.** All store lookups filter by `owner_id`, so a forged
  callback id cannot read or mutate another user's data.
- **Path traversal.** `SafeJoin`/`SecureDelete` resolve symlinks on both sides
  and refuse anything escaping the managed dir; `SanitizeFilename` cannot
  produce `..` or an absolute path.

---

## 6. Follow-up candidates (deferred, with evidence)

These are real but lower-severity items found during the audit. They are recorded
here rather than fixed in this phase to keep the diff focused on P0/P1.

| ID | Sev | Path | Note |
| --- | --- | --- | --- |
| F1 | P2 | `internal/engine/session.go:539` `cleanup()` | `run()` has no panic recovery; a panic inside the session goroutine runs `cleanup()` (good — resources are released) but is silently swallowed and the meeting's dedup slot is not freed. `logx.CatchPanicFunc` is the established pattern in this codebase. |
| F2 | P2 | `internal/engine/audiotrack.go:805,889` | `ensureAudioTrack` can spend up to ~106 s in 10 s evals with no session-context check; a stop during that window does not interrupt it. |
| F3 | P2 | `internal/engine/manager.go:687-727` | `WarmUp` holds `warmMu` for the whole launch; `ShutdownWarm` during a launch waits, then closes a warm browser the keep-warm loop may immediately relaunch. One orphan Chromium on a SIGTERM racing a launch. |
| F4 | P2 | `internal/engine/queue.go` | `Next()` advances the cursor before `SwitchMedia` succeeds; a permanently-failing item emits `EventError` and leaves the cursor pointing at the unplayable item rather than skipping or staying put. Present, documented behaviour worth revisiting. |
| F5 | P3 | `internal/web/web.go:210-232` | `/healthz` always reports `ok`; a readiness split (chromium present, DB reachable, disk free, session capacity) would let a watchdog distinguish alive from able-to-serve. |

---

## 7. Verification of the accepted fixes

| Test | Command | Result on pre-fix code | Result post-fix |
| --- | --- | --- | --- |
| E1 | `go test -race ./internal/engine -run TestSessionQueueConcurrency -count=5` | FAIL (data race) | PASS |
| E1 | `go test -race ./... -count=1` (all 16 packages) | FAIL | PASS |
| E2 | `go test ./internal/engine -run TestManagerStopWaitsForCleanup` | FAIL (15 s "did not stop in time") | PASS |
| E3 | `go test ./internal/media -run TestDialGateBlocksPrivateDial` | FAIL (no dial gate) | PASS |
| E4 | `go test ./internal/media -run TestDirectDownloadOutlivesHTTPTimeout` | FAIL ("Client.Timeout … while reading body") | PASS |
| E4 | `go test ./internal/media -run TestDirectDownloadRespectsDownloadTimeout` | FAIL (no effective deadline) | PASS |
| E5 | `go test ./internal/dashboard -run TestLoginPageDefinesOnTelegramAuth` | FAIL ("web login is dead code") | PASS |

Full suites after every commit: `gofmt -l` clean, `go vet ./...` clean,
`go test ./internal/... ./cmd/... -count=1` green, and the full `-race` suite
across all 16 packages green.

---

## 8. Architectural decisions

- **No new components, databases or services were introduced.** Every fix is
  inside an existing file, using the package's existing concurrency primitives.
- **The SSRF fix reuses the package's own `IsBlockedIP` verdict and the same
  overridable resolver** the pre-flight check uses, so test fixtures and the
  loopback exemption continue to work identically. No new dependency, no second
  source of truth for what is blocked.
- **The timeout fix promotes `opts.Timeout` to the documented bound it already
  was for yt-dlp**, rather than introducing a new knob. `HTTPTimeout` keeps its
  documented meaning as a fallback and test seam.
- **The dashboard login fix changes frontend wiring only.** The backend's
  HMAC verification, admin allow-list and rate limiting were correct and are
  untouched.
- **Lock ordering is unchanged** (E1): `queueMu` → `mu`, as the existing
  comment block in `Snapshot` documents.

---

## 9. Commit index

| Commit | Finding | Severity | Files |
| --- | --- | --- | --- |
| `a5d6b46` | E1 — `queueMu` race in `Snapshot` | P1 | `internal/engine/session.go` |
| `1249434` | E2 — `s.done` never closed | P1 | `internal/engine/session.go`, `internal/engine/done_test.go` (new) |
| `705ef2d` | E3 — SSRF dial gate + E4 — 30 s download cliff | P0 ×2 | `internal/media/manager.go`, `internal/media/fetch.go`, `internal/media/dialgate_test.go` (new), `internal/media/httptimeout_test.go` (new) |
| `17fd47f` | E5 — dead web login | P1 | `internal/dashboard/ui.go`, `internal/dashboard/login_widget_test.go` (new) |
