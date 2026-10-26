package bot

// Playlist screens and keyboards.
//
// All callback data stays well below Telegram's 64-byte limit: ids are SQLite
// integers, verbs are short, and no user text is ever placed in a button's
// callback payload (it would be forgeable and could overflow the limit).

import (
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

const (
	// cbPlaylist is the callback namespace for the playlist library.
	cbPlaylist = "pl:"
	// cbPlaylistMeet selects a saved meeting for "play playlist".
	cbPlaylistMeet = "plpm:"
)

// playlistPageSize is the number of cards/items shown per Telegram screen.
// Eight is the largest list that still fits comfortably on a phone without
// pushing the important actions off-screen.
const maxPlaylistPageSize = 8

// playlistPage slices list to one page and returns the clamped page number
// plus the total page count.
func playlistPage[T any](list []T, page int, size int) ([]T, int, int) {
	if size <= 0 {
		size = maxPlaylistPageSize
	}
	total := (len(list) + size - 1) / size
	if total == 0 {
		total = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= total {
		page = total - 1
	}
	start := page * size
	end := start + size
	if end > len(list) {
		end = len(list)
	}
	return list[start:end], page, total
}

// playlistDuration renders guessed/known total duration.
func playlistDuration(seconds float64) string {
	if seconds <= 0 {
		return "unknown"
	}
	return fmtClock(seconds)
}

// playlistKindLabel renders the stored media kind.
func playlistKindLabel(kind string) string {
	switch kind {
	case string(engine.MediaYouTube):
		return "YouTube"
	case string(engine.MediaDirect):
		return "direct media"
	case string(engine.MediaPage):
		return "web page"
	case "local":
		return "local file"
	default:
		if strings.TrimSpace(kind) == "" {
			return "media"
		}
		return kind
	}
}

// playlistListText renders the user's library landing page.
func playlistListText(list []store.Playlist, page, total int) string {
	var sb strings.Builder
	sb.WriteString("📚 <b>Your playlists</b>\n\n")
	if len(list) == 0 {
		sb.WriteString("No playlists yet.\n\n")
		sb.WriteString("A playlist is a named, persistent list of media links. Build one now and replay it into any meeting with one tap — it survives restarts.\n\n")
		sb.WriteString("Tap <b>➕ New playlist</b> to start, or forward media to the bot and save a live queue (💾 Save queue).")
		return sb.String()
	}
	sb.WriteString(fmt.Sprintf("<i>%d of %d shown</i>\n\n", len(list), total))
	for _, p := range list {
		sb.WriteString(fmt.Sprintf("• <b>%s</b>\n", esc(p.Name)))
		if strings.TrimSpace(p.Description) != "" {
			sb.WriteString("   " + esc(truncate(p.Description, 90)) + "\n")
		}
		sb.WriteString(fmt.Sprintf("   %d item(s) · %s · updated %s\n",
			p.ItemCount, playlistDuration(p.TotalDuration), p.UpdatedAt.Format("2006-01-02 15:04")))
	}
	sb.WriteString("\nTap a playlist to open it.")
	return sb.String()
}

// playlistListKB renders the library keyboard with pagination.
func playlistListKB(list []store.Playlist, page int) *tgbotapi.InlineKeyboardMarkup {
	visible, page, total := playlistPage(list, page, maxPlaylistPageSize)
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, p := range visible {
		label := fmt.Sprintf("📚 %s (%d)", truncate(p.Name, 28), p.ItemCount)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("pl:open:%d", p.ID)),
		})
	}
	if total > 1 {
		var nav []tgbotapi.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("pl:list:%d", page-1)))
		}
		nav = append(nav, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("Page %d/%d", page+1, total), "pl:noop"))
		if page+1 < total {
			nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("pl:list:%d", page+1)))
		}
		rows = append(rows, nav)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("➕ New playlist", "pl:new"),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// playlistDetailText renders one playlist and its visible page of items.
func playlistDetailText(p store.Playlist, items []store.PlaylistItem, page, total int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📚 <b>%s</b>\n\n", esc(p.Name)))
	if strings.TrimSpace(p.Description) != "" {
		sb.WriteString(esc(p.Description) + "\n\n")
	}
	sb.WriteString(fmt.Sprintf("%d item(s) · total %s\n", p.ItemCount, playlistDuration(p.TotalDuration)))
	sb.WriteString(fmt.Sprintf("Created %s · updated %s\n\n", p.CreatedAt.Format("2006-01-02"), p.UpdatedAt.Format("2006-01-02 15:04")))

	if len(items) == 0 {
		sb.WriteString("<i>This playlist has no media yet.</i>\n\n")
		sb.WriteString("Add links one by one, import a list, or save a running session queue into it.")
		return sb.String()
	}
	sb.WriteString(fmt.Sprintf("🎞 <b>Items</b> (page %d/%d)\n", page+1, total))
	for offset, it := range items {
		position := page*maxPlaylistPageSize + offset
		title := it.Title
		if strings.TrimSpace(title) == "" {
			title = store.DefaultPlaylistItemTitle(it.URL)
		}
		sb.WriteString(fmt.Sprintf("%d. %s", position+1, esc(truncate(title, 54))))
		if it.Duration > 0 {
			sb.WriteString(" · " + fmtClock(it.Duration))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\nTap an item to reorder or remove it.")
	return sb.String()
}

// playlistDetailKB renders playlist actions plus the visible item buttons.
func playlistDetailKB(p store.Playlist, items []store.PlaylistItem, page, total int) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton

	playRow := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("▶️ Play all", fmt.Sprintf("pl:play:%d", p.ID)),
		tgbotapi.NewInlineKeyboardButtonData("➕ Add links", fmt.Sprintf("pl:add:%d", p.ID)),
	}
	rows = append(rows, playRow)
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("📥 Import list", fmt.Sprintf("pl:imp:%d", p.ID)),
		tgbotapi.NewInlineKeyboardButtonData("📤 Export", fmt.Sprintf("pl:exp:%d", p.ID)),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("✏️ Rename", fmt.Sprintf("pl:ren:%d", p.ID)),
		tgbotapi.NewInlineKeyboardButtonData("📝 Description", fmt.Sprintf("pl:desc:%d", p.ID)),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("📄 Duplicate", fmt.Sprintf("pl:dup:%d", p.ID)),
		tgbotapi.NewInlineKeyboardButtonData("📋 Add to queue", fmt.Sprintf("pl:enq:%d", p.ID)),
	})

	if len(items) > 0 {
		visible, page, total := playlistPage(items, page, maxPlaylistPageSize)
		for offset, it := range visible {
			position := page*maxPlaylistPageSize + offset
			title := it.Title
			if strings.TrimSpace(title) == "" {
				title = store.DefaultPlaylistItemTitle(it.URL)
			}
			label := fmt.Sprintf("%d. %s", position+1, truncate(title, 34))
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("pl:i:%d:%d", p.ID, it.ID)),
			})
		}
		if total > 1 {
			var nav []tgbotapi.InlineKeyboardButton
			if page > 0 {
				nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("⬅️", fmt.Sprintf("pl:open:%d:%d", p.ID, page-1)))
			}
			nav = append(nav, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d/%d", page+1, total), "pl:noop"))
			if page+1 < total {
				nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("➡️", fmt.Sprintf("pl:open:%d:%d", p.ID, page+1)))
			}
			rows = append(rows, nav)
		}
	}

	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("🗑 Delete playlist", fmt.Sprintf("pl:del:%d", p.ID)),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Playlists", "pl:home"),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// playlistItemText renders one item with its position and metadata.
func playlistItemText(p store.Playlist, it store.PlaylistItem, index, count int) string {
	var sb strings.Builder
	sb.WriteString("🎞 <b>Playlist item</b>\n\n")
	sb.WriteString(fmt.Sprintf("Playlist: <b>%s</b>\n", esc(p.Name)))
	sb.WriteString(fmt.Sprintf("Position: <b>%d / %d</b>\n", index+1, count))
	title := it.Title
	if strings.TrimSpace(title) == "" {
		title = store.DefaultPlaylistItemTitle(it.URL)
	}
	sb.WriteString(fmt.Sprintf("Title: <b>%s</b>\n", esc(title)))
	sb.WriteString(fmt.Sprintf("Kind: %s", esc(playlistKindLabel(it.Kind))))
	if it.Duration > 0 {
		sb.WriteString(" · " + fmtClock(it.Duration))
	}
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("Added: %s\n\n", it.CreatedAt.Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("Link:\n<code>%s</code>\n\n", esc(it.URL)))
	sb.WriteString("<i>Move it with the arrows, or remove it from this playlist. Playback is unaffected until you press Play all.</i>")
	return sb.String()
}

// playlistItemKB renders item actions and a context-aware back button.
func playlistItemKB(p store.Playlist, it store.PlaylistItem, index, count int) *tgbotapi.InlineKeyboardMarkup {
	backPage := 0
	if index >= 0 {
		backPage = index / maxPlaylistPageSize
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("⬆️ Move up", fmt.Sprintf("pl:up:%d:%d", p.ID, it.ID)),
			tgbotapi.NewInlineKeyboardButtonData("⬇️ Move down", fmt.Sprintf("pl:dn:%d:%d", p.ID, it.ID)),
		},
	}
	if count > 1 {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("❌ Remove item", fmt.Sprintf("pl:rm:%d:%d", p.ID, it.ID)),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Back to playlist", fmt.Sprintf("pl:open:%d:%d", p.ID, backPage)),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// playlistNameText is the prompt shown while waiting for a playlist name.
func playlistNameText(mode, current string) string {
	switch mode {
	case "rename":
		return fmt.Sprintf("✏️ <b>Rename playlist</b>\n\nCurrent name: <b>%s</b>\n\nSend the new name (max %d characters).", esc(current), store.MaxPlaylistNameRunes)
	case "session":
		return fmt.Sprintf("💾 <b>Save queue as playlist</b>\n\nSend a name for the new playlist (max %d characters).\n\nThe current session queue will be copied into it; the live session is not changed.", store.MaxPlaylistNameRunes)
	default:
		return fmt.Sprintf("➕ <b>New playlist</b>\n\nSend a name (max %d characters).\n\nYou can add media right after creating it.", store.MaxPlaylistNameRunes)
	}
}

// playlistDescText is the description prompt.
func playlistDescText(p store.Playlist) string {
	return fmt.Sprintf("📝 <b>Description for %s</b>\n\nCurrent: %s\n\nSend a short description (max %d characters). Send a single <code>-</code> to clear it.",
		esc(p.Name), esc(orDash(p.Description)), store.MaxPlaylistDescriptionRunes)
}

// orDash renders an empty field readably.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// playlistAddText is the "add links" prompt.
func playlistAddText(p store.Playlist) string {
	return fmt.Sprintf(`➕ <b>Add media to %s</b>

Send one or more links. Each line may be:
• a link: <code>https://youtu.be/…</code>
• <code>Title | https://example.com/video.mp4</code>

Links are validated before anything is stored: if one line is malformed, the whole batch is rejected and nothing is added.

Send /cancel to stop.`, esc(p.Name))
}

// playlistImportText is the bulk import prompt.
func playlistImportText(p store.Playlist) string {
	return fmt.Sprintf(`📥 <b>Import into %s</b>

Paste a list, one item per line. Supported forms:

<code>https://youtu.be/dQw4w9WgXcQ</code>
<code>Opening talk | https://example.com/opening.mp4</code>

Blank lines are ignored. The import is all-or-nothing: a single invalid link rejects the batch. Maximum %d items per playlist.

Send /cancel to abort.`, esc(p.Name), store.MaxPlaylistItems)
}

// playlistDeleteText is the delete confirmation.
func playlistDeleteText(p store.Playlist) string {
	return fmt.Sprintf("⚠️ <b>Delete playlist?</b>\n\n<b>%s</b> (%d item(s))\n\nThe playlist and its stored links are removed. This cannot be undone and does not stop a session that is already playing from it.",
		esc(p.Name), p.ItemCount)
}

// playlistDeleteKB confirms a destructive delete.
func playlistDeleteKB(id int64) *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("🗑 Yes, delete", fmt.Sprintf("pl:delok:%d", id))},
		{tgbotapi.NewInlineKeyboardButtonData("✖️ Keep it", fmt.Sprintf("pl:open:%d", id))},
	}}
}

// playlistClearText is the clear-items confirmation.
func playlistClearText(p store.Playlist) string {
	return fmt.Sprintf("🧹 <b>Clear every item?</b>\n\n<b>%s</b> keeps its name and description, but all %d stored links are removed. A live session is not affected.",
		esc(p.Name), p.ItemCount)
}

// playlistClearKB confirms clearing items.
func playlistClearKB(id int64) *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("🧹 Clear all items", fmt.Sprintf("pl:clrok:%d", id))},
		{tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", fmt.Sprintf("pl:open:%d", id))},
	}}
}

// playlistExportText renders a portable copy of the playlist. Telegram caps a
// message at 4096 characters, so an oversized export is truncated with a
// clear marker instead of being rejected by the API.
func playlistExportText(p store.Playlist, items []store.PlaylistItem) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📤 <b>%s</b>\n", esc(p.Name)))
	if strings.TrimSpace(p.Description) != "" {
		sb.WriteString(esc(p.Description) + "\n")
	}
	sb.WriteString("\n")
	for _, it := range items {
		title := it.Title
		if strings.TrimSpace(title) == "" {
			title = store.DefaultPlaylistItemTitle(it.URL)
		}
		sb.WriteString(fmt.Sprintf("%s | %s\n", title, it.URL))
	}
	out := sb.String()
	const limit = 3600
	if len([]rune(out)) > limit {
		r := []rune(out)
		out = string(r[:limit]) + "\n\n<i>… export truncated; open the playlist in smaller pages to copy the rest.</i>"
	}
	return out
}

// playlistPlayMeetText asks which meeting should receive the playlist.
func playlistPlayMeetText(p store.Playlist) string {
	return fmt.Sprintf("▶️ <b>Play %s</b>\n\n%d item(s) will become the session queue; playback starts with the first one.\n\nSend a Google Meet link, or pick a saved meeting below.", esc(p.Name), p.ItemCount)
}

// playlistPlayMeetKB lists saved meetings for the playlist launch.
func playlistPlayMeetKB(p store.Playlist, saved []store.Meeting) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, m := range saved {
		if i >= 5 {
			break
		}
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📌 "+truncate(m.Name, 30), fmt.Sprintf("plpm:%d:%d", p.ID, m.ID)),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", fmt.Sprintf("pl:open:%d", p.ID)),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// playlistSessionPickText lists running sessions for "add playlist to queue".
func playlistSessionPickText(p store.Playlist, sessions []engine.Status) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 <b>Add %s to a live session</b>\n\n", esc(p.Name)))
	if len(sessions) == 0 {
		sb.WriteString("No active sessions. Start a stream first, then come back.")
		return sb.String()
	}
	sb.WriteString("Pick the session that should receive these links as upcoming items:\n\n")
	for _, st := range sessions {
		title := st.Title
		if strings.TrimSpace(title) == "" {
			title = st.MediaURL
		}
		sb.WriteString(fmt.Sprintf("%s <code>%s</code> · %s\n", stateEmoji(st.State), st.ID, esc(truncate(title, 36))))
	}
	return sb.String()
}

// playlistSessionPickKB lists one button per live session.
func playlistSessionPickKB(p store.Playlist, sessions []engine.Status) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, st := range sessions {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(stateEmoji(st.State)+" "+string(st.ID), fmt.Sprintf("pl:enqto:%d:%s", p.ID, st.ID)),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Back", fmt.Sprintf("pl:open:%d", p.ID)),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// playlistOverviewText is used by the management console for a global count.
func playlistOverviewText(st store.PlaylistStats) string {
	return fmt.Sprintf("📚 Playlists: <b>%d</b>\n🎞 Stored items: <b>%d</b>\n👤 Owners: <b>%d</b>", st.Playlists, st.Items, st.Owners)
}

// playlistDate renders an optional time consistently.
func playlistDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
}
