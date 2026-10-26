# TeleMeetBot v1.3.2 Release Notes

TeleMeetBot v1.3.2 introduces **Persistent Media Playlist Queues with Previous/Next controls** and a comprehensive overhaul of **Google Meet Admission & Session Reliability**.

## 1. Persistent Media Playlist Queue & Telegram Staging

- **50-Item Batch Submission**: Users can forward multiple Telegram audio/video messages in batch, or paste up to 50 URLs (YouTube, MP4, MP3, web streams) separated by newlines in a single message.
- **Playback Controls**: Added interactive **⏮ Previous** and **⏭ Next** inline buttons to the session card alongside Pause/Resume, Restart, and Stop.
- **Queue Management**: Inspect active playlists with track positions (e.g., `📋 Playlist: 3 / 50`), upcoming items, direct track jumping, item removal, or clear queue with confirmation.
- **Auto-Advance with Ended Detection**: Automatically advances to the next playlist item upon completion, checking both HTML5 media `ended` events and YouTube IFrame Player API `getPlayerState() === 0`.
- **Media Lease Protection**: Queued local media files are locked under `queue:<sessionID>` leases, preventing premature reclamation by background janitor sweeps.
- **SQLite Persistence**: Playlist order and current track index persist across session restarts.

## 2. Google Meet Admission & Lifecycle Reliability

- **Explicit Observable State Machine**:
  `QUEUED → STARTING → JOINING → WAITING_FOR_ADMISSION → ADMITTED → ACTIVE → LEAVING → COMPLETED`
  with distinct terminal outcomes: `FAILED`, `CANCELLED`, and `TIMED_OUT`.
- **Single-Flight Meeting Deduplication**: Prevents duplicate sessions or workers targeting the same Google Meet room (`Shortcode(meetURL)` matching), avoiding browser knock conflicts and rate limits.
- **Debounced Join Requests**: Prevents hammering or cancelling in-flight "Ask to join" requests by debouncing join clicks and verifying button enablement.
- **Modern Web Framework Input Synchronization**: Uses native property descriptor setters (`HTMLInputElement.prototype.value`) so Google Meet's Angular/React/Lit form validation recognizes guest names and activates the join button.
- **Jittered Bounded Backoff**: For host-restricted meetings ("No one can join a meeting unless invited or admitted by the host"), re-request entry cycles back off with randomized jitter (20s–30s + jitter) instead of rigid 30s reloads, eliminating poll storms and reducing delays when hosts arrive.
- **Actionable Host Admission Timeouts**: Returns clear guidance when a meeting host fails to admit the guest within `admit_wait_seconds`.

## Verification & Tests

- Full race-detector suite passed clean (`go test -race ./...`).
- Regression tests for 50-item sequential queue, boundaries, lease management, concurrency, admission state transitions, single-flight deduplication, and reload jitter.
