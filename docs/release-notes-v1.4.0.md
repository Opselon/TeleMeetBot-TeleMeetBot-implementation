# TeleMeetBot v1.4.0 Release Notes

TeleMeetBot v1.4.0 introduces persistent playlists, an administrative management console, interactive queue management tools, and live database backup capabilities.

## Highlights

### 1. Persistent Playlist Library (`playlists.db`)
- **Dedicated SQLite Persistence**: Added a dedicated `playlists.db` database to store named, reusable playlists independently of live sessions and meeting links.
- **Fail-Closed Multi-Tenant Isolation**: Every playlist query, mutation, item reordering, and deletion strictly checks and verifies ownership by Telegram User ID.
- **Full Library Management**: Create, rename, describe, list, duplicate, and delete playlists through an intuitive Telegram inline wizard.
- **Bulk Import & Reordering**: Import raw URLs in bulk, append media, move tracks up or down with automatic contiguous re-indexing, and launch entire playlists directly into Google Meet meetings.

### 2. Comprehensive Management Console (`/manage`)
- **System Overview & Health Metrics**: Real-time monitoring of host memory, active streams, database records, and system telemetry.
- **Runtime Admin Access Control**: Add or remove Telegram admin IDs dynamically without process restarts; prevents lockout by protecting the last admin.
- **Automated SQLite Backups (`VACUUM INTO`)**: Create consistent point-in-time snapshots of `config.yml`, `state.json`, `meetings.db`, and `playlists.db` using SQLite's atomic `VACUUM INTO`.
- **Admin Broadcasts**: Send formatted announcements and system notices to all configured administrators.

### 3. Advanced Session Queue Tools
- **Tail Shuffle & Sort**: Shuffle upcoming queue items (`ShuffleUpcoming`) without interrupting active playback, or sort upcoming tracks alphabetically by title (`SortUpcoming`).
- **Interactive Track Reordering**: Reorder upcoming queue items interactively in Telegram with automatic index tracking.
- **Save Live Queue as Playlist**: Convert any active Google Meet streaming queue directly into a permanent playlist in your library with one click.
- **Play Next Insertion**: Enqueue tracks directly after the current playing item (`EnqueueNext`) for instant priority playback.

### 4. Structured Observability & Comprehensive Test Suite
- Structured telemetry and pipeline logs (`[PIPELINE:manage]`, `[PIPELINE:playlists]`, `[PIPELINE:queue]`) across all management endpoints and callback flows.
- Unit and concurrency test suite added for playlist storage, multi-tenant isolation, admin operations, snapshot backups, and queue tools.
