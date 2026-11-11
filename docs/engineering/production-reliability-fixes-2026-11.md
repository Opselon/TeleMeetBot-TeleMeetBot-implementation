# Production reliability fixes — November 2026

Investigation date: 2026-11-10/11 (live VPS `80.47.236.99`, Ubuntu 26.04, 2 vCPU / 4 GiB, Docker deployment, Chromium 156).

This note records the root-cause investigation behind the admission-resilience
changes and the network findings every operator of a datacenter deployment
should know about.

---

## 1. Root causes found on the production VPS

Three independent defect classes were confirmed with live evidence.

### RC-A — the meet leg's "quiet" guarantee was void (state corruption)

`quietMeet()` was called **before** `joinMeeting()` ran, so the meet leg kept
calling `setState` for the entire join loop after `meetQuiet` had already
closed. Consequences observed live (`--probe` run, session `s-1be393a1`):

1. Media leg failed at [2/6] (`LOGIN_REQUIRED Sign in to confirm you're not a bot`) → the media-fatal branch recorded terminal `error`.
2. The meet leg — still polling the host gate — overwrote the terminal state
   back to `waiting_for_admission`.
3. Teardown relabelled the non-terminal state into `cancelled`.

A session that both failed its media and was waiting on Google's gate
therefore reported **cancelled** — the least honest terminal state possible.
Fix: `quietMeet()` now fires after `joinMeeting()` returns; a reader of
`meetErrCh` can rely on "no more setState from this leg".

### RC-B — a media failure killed the session during the admission wait

YouTube's datacenter bot-check ("Sign in to confirm you're not a bot",
classified `source_blocked`) routinely fails the browser media leg ~35-40 s
into a session — exactly while Google's host gate is still deciding whether
to admit the bot. The old media-fatal path ended the session instantly, so
the host never got to admit it. Fix:

- The media-first-error path now **waits for the join verdict** (bounded —
  `joinMeeting` is deadline-bounded by the join timeout + admit wait, and the
  verdict is sent only after the leg's last setState, so the decision is
  race-free).
- **Admitted wins:** an admitted session survives the media failure. The
  failure is recorded as a playback phase with an actionable message, the
  standard single auto-fallback download runs (Auto mode), and the share/play
  flow continues — the same "meeting stays live, switch media" contract the
  after-share failure path already had.
- **Both failed → media story** stays the terminal verdict (symmetric with
  the meet-first branch); the join error is logged.

### RC-C — join timeouts were reported as generic `error`

`handleMeetLegError` set `StateTimedOut` and then immediately called
`setError`, which unconditionally overwrote the state with `StateError` —
`timed_out` was unreachable. Fixed via `failWithState(StateTimedOut, err)`,
which keeps the full error surface (message, log, EventError) under the
honest terminal state.

### RC-D — restart triage missed interrupted states

`store.Interrupted()` did not recognise `waiting_for_admission`, `admitted`
or `leaving`, so records from a killed process lingered as live sessions.
All non-terminal states are now covered.

---

## 2. Network findings on the deployment host (evidence)

Measured from inside the production container and on the host itself:

| Check | Result |
|---|---|
| TCP/TLS to `meet.google.com:443` | ✅ 200 in ~120 ms |
| TCP to Google main ranges (`142.250.0.5`, `216.58.194.174`, `74.125.250.129`) | ❌ blackholed (timeout) |
| TCP to `stun.l.google.com` (443 **and** 19302) | ❌ blackholed |
| TCP to googlevideo edges (`74.125.197.134`) | ✅ open |
| UDP STUN to Google (`19302`) | ❌ **no reply at all** (host and container) |
| UDP 53 to `8.8.8.8` | ❌ no reply (container DNS uses provider resolvers over UDP — those work) |
| IPv6 | ❌ none (AAAA answers exist on the host resolver; container sees IPv4-only and works) |

Interpretation: the provider applies a **destination-based filter** — some
Google edge ranges are reachable (Meet signalling, googlevideo), others are
blackholed (stun/TURN-adjacent ranges, several main ranges), and outbound UDP
is silently dropped except to the provider's own resolvers.

Consequences for Google Meet:

- WebRTC over UDP is impossible; every meeting's media must ride **TURN over
  TCP/TLS 443** (Meet supplies its own TURN credentials, incl. TCP
  transport). Meetings therefore work, but media is relayed: expect higher
  CPU and slightly higher latency, not sub-100 ms p2p transport.
- The filter is upstream of the host (iptables/ufw are permissive locally).
  Fixing it requires the provider or a route that avoids the blackholed
  ranges — no in-process change can restore UDP.

The new `netcap` UDP probe (see below) now surfaces exactly this fact from
`/healthz` instead of requiring an SSH session to diagnose.

---

## 3. New diagnostics

### UDP/STUN egress probe (`internal/netcap/udpprobe.go`)

A cached, single-flight RFC 5389 STUN binding probe against Google's public
STUN endpoints. Outcomes: `ok` / `blocked` (datagrams leave, nothing answers —
the provider-filter signature) / `error` (local failure). Wired into
`Manager.ObserveNetwork` (runs on the same demand/startup/recovery passes as
the HTTP probe) and reported under `network.udp` in `/healthz`. The STUN
mapped address is deliberately **not** exposed over HTTP (public endpoint).

Example of what it reports on this deployment:

```json
"network": { "udp": { "outcome": "blocked", "attempted": 4, "error": "..." }, ... }
```

### Meet-only probe mode

`--probe <meet-link> --probe-media ""` now skips YouTube entirely: the media
tab loads a tiny local page, so the probe isolates the join/admission signal
from YouTube's datacenter bot-check. `--probe-mode auto` lets an E2E probe
run the download ladder the way real Telegram sessions do.

---

## 4. Operator guidance for UDP-filtered hosts

- Do **not** try to fix joins by raising retry counts; on this network the
  join signalling is healthy and media rides TURN/TCP.
- If presentation video is blank or stalls, check `network.udp` in
  `/healthz` first: `blocked` + slow publishes means the host is relaying
  everything through TURN-over-TCP on a CPU-constrained box.
- Prefer a host whose provider allows outbound UDP on non-53 ports; on such
  a network the same binary needs no changes.
- `admit_wait_seconds` bounds the host-gate patience (default 300 s). The
  engine now survives a media failure while that wait is running; it no
  longer cancels the knock.
