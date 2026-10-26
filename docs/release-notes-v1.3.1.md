# TeleMeetBot v1.3.1 — long videos no longer die mid-playback

**A patch release with one headline fix: a downloaded file could be deleted
while a session was still streaming it.** Anything longer than the one-hour
download TTL — a long YouTube download, a paused stream, a talk left running —
died mid-playback with a playback error and, in download mode, an aborted
session.

## The bug

Downloaded media carried a hard one-hour TTL, and that TTL was enforced as a
playback deadline rather than as a cleanup policy:

- The media janitor (`Sweep`) deleted **any** expired entry, without checking
  whether a session was still streaming it. A two-hour video downloaded at
  noon was unlinked at 13:00, mid-playback.
- The session watchdog treated the same deadline as a session-level failure:
  when `ExpiresAt` passed, `watch()` errored the session out instead of
  letting playback continue.
- The file server then served a path that no longer existed, so seeks, range
  requests and resumed playback all failed with a hard error.

The TTL is the right policy for *cleaning up abandoned files*. It was the wrong
policy for *media in use*.

## The fix: active-media leases

Downloaded files are now pinned to the session that owns them for the entire
lifetime of that session. The TTL is unchanged at one hour, but it is now an
**orphan-reclamation policy** instead of a hard cut-off:

- `media.Manager` gains a lease API (`Lease`, `Release`,
  `ReleaseAndDelete`, `LeaseOwners`). A session leases its file the moment it
  adopts it, before the file is recorded as the session's local media — so
  there is no window in which the janitor can delete a file the session is
  about to play.
- The janitor skips any expired entry that a lease holds, and logs that it
  retained it. Playback continues for as long as the owning session is alive,
  regardless of video duration, elapsed time or paused playback.
- Releasing a lease reclaims the file: `Release` deletes an expired, unowned
  entry at once, while a file that is still fresh is left for the janitor and
  its own deadline. `ReleaseAndDelete` is the teardown path (stop, close,
  cleanup, mode change): it deletes immediately unless another session still
  holds the same entry — for example two sessions sharing one Telegram file.
- `Remove()` stays the unconditional admin path and always wins over a lease.
- Leases are **in-memory only**. A process restart drops every pin, so stale
  leases can never protect orphaned files: crash recovery and the startup
  sweep behave exactly as before, and disk protection (download size cap,
  timeout, concurrency, path validation, SSRF defences, minimum free disk) is
  untouched.

The session watchdog no longer fails a session at `ExpiresAt`. It logs once
that the file is past its TTL and held by an active lease, and playback
continues.

## Configuration

No key was added, removed or renamed; every v1.3.0 config loads unchanged.
The semantics of one key are clarified in the docs:

```yaml
download:
  ttl_minutes: 60   # orphan cleanup: reclaims files no session is streaming;
                    # an active session keeps its file for its whole lifetime
```

`Status` gains one read-only field, `leased_files`, reporting how many managed
files are currently held by an active session.

## Regression tests

Existing tests that required an active file to expire were rewritten, and new
deterministic tests added (short TTLs and an injectable clock — no test waits
an actual hour). Proven by the suite:

- an active download remains accessible **after** its original one-hour
  deadline, under repeated janitor sweeps;
- playback requests, including HTTP range requests, do not fail because the
  janitor deleted the file;
- stopping a session or switching media releases the old file promptly, while
  the new media is leased before it is adopted;
- inactive expired files are still reclaimed;
- restart recovery, concurrent cleanup, failed downloads and multi-session
  ownership remain safe.

Full suite, race detector and `go vet` are green.

## Artifacts

Native binaries for Linux (amd64, arm64, arm v6/v7, 386), macOS (Intel, Apple
Silicon) and Windows (amd64, arm64), plus `SHA256SUMS` and the installers:

```bash
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/v1.3.1/install.sh | sh -s -- --version v1.3.1
```

Docker image published as
`ghcr.io/opselon/telemeetbot-telemeetbot-implementation:v1.3.1` (and `:latest`),
multi-arch for `linux/amd64` and `linux/arm64`.
