# TeleMeetBot v1.3.3 Release Notes

TeleMeetBot v1.3.3 fixes switch media reverts from canvas security errors, adds expanded queue clear options, and introduces comprehensive action logging.

## Highlights

### 1. Robust Switch Playback Verification & CORS Hardening
- **Tainted Canvas Handling**: Fixed an issue where media playback verification failed during `SwitchMedia` due to cross-origin HTML5 canvas tainting (`SecurityError: Failed to execute 'getImageData' on 'CanvasRenderingContext2D': The canvas has been tainted by cross-origin data.`), which erroneously caused the engine to revert to the previous media track.
- **Cross-Origin Headers**: Enabled permissive CORS headers (`Access-Control-Allow-Origin: *`, `Access-Control-Allow-Methods`, `Access-Control-Allow-Headers`) on internal file servers.
- **Playback Progress Verification**: Playback verification now accepts verifiable timeline advancement (`advance >= minAdvance && !paused && ready >= 2`) when frame pixel readouts are restricted by canvas security policies.

### 2. Strengthened Queue Management & Clear Options
- **Clear Options Expansion**: Added `ClearAllExceptCurrent()` to allow operators to wipe both past and upcoming playlist items in one click while preserving the currently playing track.
- **Queue State Safety**: Strengthened `ClearUpcoming()` to safely clear upcoming items when queue playback has not yet begun (`queueIdx < 0`).
- **Interactive Clear Confirmation**: Inline keyboard now provides explicit granular choices: "🗑 Clear Upcoming Only", "🧹 Clear All Except Current", and "❌ Cancel".

### 3. Telegram Large File Streaming & Ingest Clarification
- Supported custom Bot API endpoint (`telegram.api_endpoint`) and configurable size ceiling (`telegram.max_file_size_mb`) for local Bot API setups up to 2000 MiB.
- Clarified error messaging when a forwarded file exceeds cloud Bot API limits, explaining that media must be retrieved to present in Chromium and suggesting direct URL streaming.

### 4. Comprehensive Structured Logging (`[PIPELINE:...]`)
- Added comprehensive logging across all Telegram inline keyboard buttons, callback queries, navigation events, and command dispatches.
- Added structured logs for Google Meet guest name entry, join button detection/clicks, and present actions.
- Added session control execution logs with status toasts and detailed error tracking.
