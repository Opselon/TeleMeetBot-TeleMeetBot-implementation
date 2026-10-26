# Playlists and the management console

This document describes the two large operator-facing features added on top of
the original dashboard:

1. **A persistent playlist library** that belongs to a Telegram user and
   survives bot restarts.
2. **A management console** that gathers users, admins, storage, maintenance,
   backups and broadcasts in one place.

Both features are reachable from the inline dashboard and from commands. They
are designed for the same trust model as the rest of the bot: every authorized
Telegram id is an operator, and every unauthorized id is rejected before a
callback or a text message is parsed.

---

## Playlists

### What a playlist is

A playlist is a named, ordered, persistent list of media links. It is the
long-lived counterpart of a session queue:

| | Session queue | Playlist |
|---|---|---|
| Lifetime | dies with the session | lives in SQLite |
| Owner | the running session | a Telegram user |
| Ordering | editable while playing | editable at any time |
| Launch | starts one session | can launch a session or feed a live queue |
| Limit | bounded by the engine | 500 items, 100 playlists per owner |

A playlist does not copy media bytes. It stores links, optional titles,
optional media kinds and optional durations. A local file produced by
Download & Stream can become invalid when its TTL expires; the launch path
checks local entries and skips the ones whose download is gone instead of
failing the whole playlist.

### Storage

Playlists live in their own SQLite database:

```
<data_dir>/playlists.db
```

It is deliberately separate from `meetings.db`:

- a damaged playlist library can be reset without losing saved meeting links;
- a damaged meeting database can be reset without losing playlists;
- the two databases can be backed up independently;
- a future schema change in one cannot block startup of the other.

The schema is small and explicit:

```sql
playlists(
    id, owner_id, name, description, created_at, updated_at
)

playlist_items(
    id, playlist_id, position, url, title, kind, duration, created_at
)
```

`position` is always contiguous from zero after every mutation. That makes the
dashboard numbering stable and makes "move up" a simple neighbour swap rather
than a renumbering pass.

Every lookup includes `owner_id`. A forged callback id can therefore never
read or mutate another user's playlist, even though callback data itself is
client-controlled.

### Limits and validation

| Limit | Value | Enforced in |
|---|---|---|
| Playlists per owner | 100 | `store.PlaylistStore.Create` |
| Items per playlist | 500 | `store.PlaylistStore.AddItems` |
| Name length | 60 runes | `ValidatePlaylistName` |
| Description length | 240 runes | `ValidatePlaylistDescription` |
| Item title length | 120 runes | `AddItems` |
| URL length | 2048 runes | `ValidatePlaylistURL` |

Names are trimmed, internal whitespace is collapsed, and control characters
are rejected. Names are unique per owner, case-insensitively, through a SQLite
unique index. Descriptions are optional; a single `-` in the Telegram prompt
clears one.

Only absolute `http://` and `https://` links are accepted. This is a storage
fence, not a replacement for the engine's classification: the engine still
decides whether a link is YouTube, a direct media file or a web page.

### Import format

The import and "add links" prompts accept one item per line:

```
https://youtu.be/dQw4w9WgXcQ
Opening talk | https://example.com/opening.mp4
```

A line may contain several bare links; each becomes its own item. The first
title on a line applies to the first link. Blank lines are ignored.

Imports are all-or-nothing:

1. every line is parsed;
2. every URL is validated;
3. the batch size is checked;
4. only then does one transaction insert the items.

If any line is invalid, the whole batch is rejected and the first bad line is
quoted back to the user. Nothing is partially imported.

### Screens and buttons

The library entry point is **📚 Playlists** on the dashboard or `/playlists`.

Landing page:

- one button per playlist, with its item count;
- pagination when the library has more than eight playlists;
- **➕ New playlist**;
- **⬅️ Dashboard**.

Playlist detail:

- **▶️ Play all** — starts a session whose queue is the whole playlist;
- **➕ Add links** — appends typed links one at a time;
- **📥 Import list** — pastes a batch;
- **📤 Export** — renders the list as `title | url` lines;
- **✏️ Rename**;
- **📝 Description**;
- **📄 Duplicate** — copies the playlist and all items;
- **📋 Add to queue** — appends the playlist to a running session;
- one button per visible item;
- **🗑 Delete playlist**;
- **⬅️ Playlists** and **🏠 Dashboard**.

Item detail:

- **⬆️ Move up** and **⬇️ Move down**;
- **❌ Remove item**;
- **⬅️ Back to playlist**, returning to the correct page.

Every destructive action has a confirmation screen. Deleting a playlist never
stops a session that is already playing from it: the session owns its own copy
of the queue.

### Playing a playlist

Pressing **▶️ Play all** asks for a Google Meet link and offers saved meetings
from the meetings library. The first playlist item becomes the opening media;
the rest become the initial queue.

The launch path checks every local media URL against the Download & Stream
manager. Expired local files are skipped with a visible count instead of
failing the launch. Remote links are always kept: a dead remote link is a
network problem that belongs on the session card and may recover.

Launching respects `limits.max_sessions` and uses the caller's saved audio
language and media mode, exactly like `/new`.

### Adding a playlist to a live session

Pressing **📋 Add to queue** lists the active sessions. Choosing one appends
every playable item to that session's queue without interrupting playback.

The confirmation screen shows how many items were added and re-binds the
message to the session card, so later refresh ticks keep it accurate.

### Saving a queue as a playlist

Every session card has **💾 Save queue**, and the queue view has
**💾 Save as playlist**. Both ask for a name and copy the current queue into a
new playlist. The live session is not changed.

If the queue is empty or the session has ended, the command is refused with a
clear reason. If inserting the items fails, the partially created playlist is
deleted rather than left behind empty.

### Advanced queue tools

The session card's **🎛 Tools** button and the queue view expose operations
that were previously missing:

- **🔀 Shuffle upcoming** — randomizes only items after the current one;
- **🔤 Sort upcoming** — orders the tail by title, then URL;
- **↕️ Reorder items** — moves one upcoming item up or down;
- **💾 Save as playlist** — copies the queue;
- **🗑 Clear options** — clears upcoming items or everything except the
  current one;
- **📋 View queue** — opens the full queue.

Shuffling, sorting and reordering never switch media. The current item keeps
playing; only the order of what comes next changes. The current index follows
the playing item if it is one of the moved rows.

`/queue [session-id]` opens the queue directly. With no argument it uses the
first active session.

---

## Management console

Open it with **🎛 Manage** on the dashboard, `/manage`, `/admin` or
`/dashboard`.

The console is guarded by the same allow-list as every other handler. There is
no separate operator role; a user who can start a session can also inspect the
bot. That is intentional for a single-operator deployment, and the
confirmation screens keep accidents unlikely.

### Overview

The landing page shows:

- active sessions;
- known users and admin count;
- playlists, stored items and distinct owners;
- session records, saved preferences and the oldest record.

Navigation:

- 👥 Users
- 📚 Playlists
- 🛡 Admins
- 💾 Storage
- 🧰 Maintenance
- 🗄 Backups
- 📣 Broadcast
- 🩺 Diagnostics

### Users

The user list is the union of:

- users with saved preferences;
- chat ids from session history;
- owners of at least one playlist.

It shows role, playlist count and session count. A user screen shows the saved
audio language, media mode, latest sessions and two actions:

- **♻️ Reset preferences** — removes that user's saved choices only;
- **🕘 History** — the newest ten session records for that user.

Resetting preferences never touches playlists or history. A user who has never
saved a preference is shown as using defaults rather than as an error.

### Playlists

The console shows aggregate library statistics and the first ten owners. It
does **not** expose playlist contents: the aggregate is enough to see whether
the feature is used, and per-user libraries stay private even from the console.
Operators can open their own library through the normal 📚 Playlists button.

### Admins

The admin screen lists every id in `telegram.admins`, marks the current user,
and offers:

- **➕ Add admin** — asks for a numeric Telegram id, appends it in memory,
  writes the config immediately and reports a save failure;
- **➖ Remove** per admin — goes through a confirmation screen.

The last admin cannot be removed. Losing the final operator would make the bot
unrecoverable without editing the file by hand, so the store-level guard wins
over the button.

Config mutation is protected by a read/write mutex. `AdminIDs()` returns a copy,
so callbacks can iterate the allow-list while another handler adds an id.

### Storage

The storage screen reports:

- free and total disk space from the health snapshot;
- sizes of `config.yml`, `state.json`, `meetings.db` and `playlists.db`;
- Download & Stream status: directory, managed files and bytes, leased files,
  TTL, and whether yt-dlp and ffmpeg were found.

Missing files are shown as "not created yet" rather than as zero bytes.
Refreshing re-reads everything; nothing is cached.

### Maintenance

Three operations, each with a confirmation or an explicit description:

- **🧹 Vacuum playlists** — runs SQLite `VACUUM` on `playlists.db`. Safe while
  the bot runs, may take a moment on a large library.
- **🕘 Clear history** — deletes saved session records. Preferences and
  playlists stay. Interrupted-session recovery will have nothing to report
  afterwards.
- **♻️ Reset all prefs** — removes every saved audio-language and media-mode
  choice. Running sessions are not changed.

Preferences are stored in `state.json`; clearing history and resetting
preferences each rewrite that file once.

### Backups

A backup is a timestamped directory under:

```
<data_dir>/backups/<YYYYMMDD-HHMMSS>/
```

It contains:

- `config.yml` copied verbatim;
- `state.json` copied verbatim;
- `meetings.db` produced with SQLite `VACUUM INTO`;
- `playlists.db` produced with SQLite `VACUUM INTO`;
- `manifest.json` with version, creation time, file names and total bytes.

Missing source files are skipped, so a fresh installation can still back up
whatever exists. At least one file must be copied; otherwise the empty
directory is removed and an error is returned.

`VACUUM INTO` is used for databases because copying a live WAL database can
miss committed pages that still live in the `-wal` file. `VACUUM INTO` writes a
self-contained copy from a read transaction, so the snapshot is consistent
even while the bot is serving traffic.

The screen shows the newest five snapshots, total bytes and the count. Actions:

- **➕ Create backup** — one snapshot, synchronously;
- **🧹 Prune old** — keeps the newest ten and deletes older snapshot
  directories; unrelated directories are never touched;
- **🔄 Refresh**.

A manifest that cannot be read makes a directory invisible to the list, which
also means it is never pruned. That is deliberate: an incomplete snapshot is
not silently deleted.

### Broadcast

**📣 Broadcast** composes one message for every admin:

1. the bot asks for the text;
2. the text is stored in the user's flow and shown as a preview with the
   recipient count and character count;
3. **📣 Send** delivers it, **✏️ Rewrite** starts over, **✖️ Cancel** clears
   the flow.

The preview escapes the message so Telegram always accepts the preview itself.
The delivered message is sent with the original formatting. Saving the flow
between steps means a second message can replace the draft without losing the
confirmation buttons.

Broadcast is one-shot: failures are logged per recipient and never retried
automatically. A message that contains invalid HTML is rejected by Telegram;
the log names the recipient and the error.

### Diagnostics

**🩺 Diagnostics** reuses the health card: version, Go runtime, host, uptime,
sessions, Chromium path, disk and config path. It is the same data as `/health`
with console navigation around it.

---

## Data and recovery notes

### Separate databases

`meetings.db` and `playlists.db` are independent SQLite files with their own
migrations. Opening either one applies only its own idempotent DDL. A failure
to open `playlists.db` logs a warning and starts the bot with the library
disabled; a failure to open `meetings.db` is fatal because saved meeting links
are part of the normal `/new` flow.

### Startup backup of the database file

Before opening each SQLite database, the bot copies the raw file to
`<name>.bak-<unix-time>` when it exists. This is a last-resort recovery aid, not
a replacement for the backup screen: it is a byte copy and does not understand
WAL.

### What survives a restart

| Data | File | Survives restart |
|---|---|---|
| Playlists and items | `playlists.db` | yes |
| Saved meetings | `meetings.db` | yes |
| Session history | `state.json` | yes |
| User preferences | `state.json` | yes |
| Active sessions | memory | no, marked interrupted |
| Open wizards and drafts | memory | no |
| Broadcast drafts | memory | no |

### What does not

Session queues are in memory. Saving a queue as a playlist is the supported way
to make it permanent. The management console does not edit playlist contents;
that belongs to the owner's library screens.

---

## Callback-data budget

Telegram limits callback data to 64 bytes. Every new button follows the same
rules:

- ids are numeric SQLite ids or session ids, never names or URLs;
- verbs are short (`pl:`, `q:`, `plpm:`, `menu:manage:`);
- no user text is placed in a payload, because it is both forgeable and
  unbounded;
- pagination uses numeric page indexes.

The longest new payloads are `menu:manage:user:<id>:resetok` and
`q:mvdn:<session-id>:<item-id>`, both comfortably below the limit for normal
ids.

---

## Operational checklist

After upgrading:

1. Run `/manage` and confirm the console opens.
2. Open **🗄 Backups** and create one snapshot.
3. Check **💾 Storage** shows the expected data directory.
4. Create a playlist, add two links, press **▶️ Play all** into a test meeting.
5. While it runs, press **🎛 Tools** and try shuffle, sort and reorder.
6. Press **💾 Save queue** and confirm the saved copy appears under
   📚 Playlists.
7. Use **📣 Broadcast** to send a short test message to the admins.

If the playlist library is unavailable, `/playlists` says so and the dashboard
keeps working; check the log for the `open playlist store` warning and the
writability of `paths.data_dir`.
