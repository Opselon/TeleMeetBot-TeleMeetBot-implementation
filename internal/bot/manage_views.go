package bot

// Management console screens.
//
// The bot has no separate "operator" role: every authorized user is an admin,
// so this console is reachable from the dashboard for the same people who can
// already start and stop sessions. It is nevertheless written defensively:
// every destructive action goes through a confirmation screen, and nothing in
// a button payload contains user text.

import (
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/backup"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// manageUser is one row in the operator's user list.
type manageUser struct {
	ID        int64
	Admin     bool
	HasPrefs  bool
	AudioLang string
	MediaMode string
	Playlists int
	Records   int
}

// manageUserDetail carries the data one user screen needs.
type manageUserDetail struct {
	manageUser
	Pref      store.UserPreference
	History   []store.Record
	AdminList []int64
}

// manageFileInfo is one row in the storage screen.
type manageFileInfo struct {
	Label  string
	Path   string
	Bytes  int64
	Exists bool
}

// manageHomeText renders the console landing page.
func manageHomeText(userID int64, st store.AdminStats, pl store.PlaylistStats, admins int) string {
	var sb strings.Builder
	sb.WriteString("🎛 <b>Management console</b>\n\n")
	sb.WriteString(fmt.Sprintf("Signed in as <code>%d</code>\n\n", userID))
	sb.WriteString("<b>Runtime</b>\n")
	sb.WriteString(fmt.Sprintf("• Active sessions: <b>%d</b>\n", st.Active))
	sb.WriteString(fmt.Sprintf("• Known users: <b>%d</b>\n", st.Users))
	sb.WriteString(fmt.Sprintf("• Admins: <b>%d</b>\n", admins))
	sb.WriteString("\n<b>Library</b>\n")
	sb.WriteString(fmt.Sprintf("• Playlists: <b>%d</b> across <b>%d</b> owner(s)\n", pl.Playlists, pl.Owners))
	sb.WriteString(fmt.Sprintf("• Stored media items: <b>%d</b>\n", pl.Items))
	sb.WriteString("\n<b>History</b>\n")
	sb.WriteString(fmt.Sprintf("• Session records: <b>%d</b>\n", st.Records))
	sb.WriteString(fmt.Sprintf("• Saved preferences: <b>%d</b>\n", st.Preferences))
	if !st.Oldest.IsZero() {
		sb.WriteString(fmt.Sprintf("• Oldest record: %s\n", st.Oldest.Format("2006-01-02 15:04")))
	}
	sb.WriteString("\nPick a section. Destructive actions always ask for confirmation.")
	return sb.String()
}

// manageHomeKB renders the console navigation.
func manageHomeKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("👥 Users", cbManage+":users"),
			tgbotapi.NewInlineKeyboardButtonData("📚 Playlists", cbManage+":playlists"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🛡 Admins", cbManage+":admins"),
			tgbotapi.NewInlineKeyboardButtonData("💾 Storage", cbManage+":storage"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🧰 Maintenance", cbManage+":maint"),
			tgbotapi.NewInlineKeyboardButtonData("🗄 Backups", cbManage+":backups"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("📣 Broadcast", cbManage+":bc"),
			tgbotapi.NewInlineKeyboardButtonData("🩺 Diagnostics", cbManage+":diag"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
		},
	}}
}

// manageBackKB returns to the console.
func manageBackKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
		},
	}}
}

// mgHome is the callback for the console landing page.
func mgHome() string { return cbManage + ":home" }

// manageAdminsText lists the allow-list.
func manageAdminsText(ids []int64, self int64) string {
	var sb strings.Builder
	sb.WriteString("🛡 <b>Administrators</b>\n\n")
	sb.WriteString("Every listed Telegram id can control the bot, including sessions and this console. Access is fail-closed: an id that is not listed cannot use the bot.\n\n")
	if len(ids) == 0 {
		sb.WriteString("<i>No admins configured. The first /start will claim the bot.</i>")
		return sb.String()
	}
	for i, id := range ids {
		marker := ""
		if id == self {
			marker = " <i>(you)</i>"
		}
		sb.WriteString(fmt.Sprintf("%d. <code>%d</code>%s\n", i+1, id, marker))
	}
	sb.WriteString(fmt.Sprintf("\n<b>%d</b> admin(s). The last admin cannot be removed.", len(ids)))
	return sb.String()
}

// manageAdminsKB offers add/remove controls.
func manageAdminsKB(ids []int64, self int64) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	if len(ids) > 1 {
		for _, id := range ids {
			label := fmt.Sprintf("➖ Remove %d", id)
			if id == self {
				label = fmt.Sprintf("➖ Remove me (%d)", id)
			}
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("%s:adminrm:%d", cbManage, id)),
			})
		}
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("➕ Add admin", cbManage+":adminadd"),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// manageAdminAddText asks for the Telegram id to add.
func manageAdminAddText() string {
	return "➕ <b>Add administrator</b>\n\nSend the numeric Telegram user id to allow.\n\nAsk the person for their id with /id, then send it here. The id is appended to <code>telegram.admins</code> and saved immediately.\n\nSend /cancel to abort."
}

// manageAdminRemoveText is the removal confirmation.
func manageAdminRemoveText(id int64) string {
	return fmt.Sprintf("⚠️ <b>Remove administrator?</b>\n\n<code>%d</code> will immediately lose access to every command, session and this console.\n\nIf this is the last remaining admin the bot refuses the removal, because nobody could recover access afterwards.", id)
}

// manageAdminRemoveKB confirms an admin removal.
func manageAdminRemoveKB(id int64) *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🗑 Remove", fmt.Sprintf("%s:adminrmok:%d", cbManage, id)),
			tgbotapi.NewInlineKeyboardButtonData("✖️ Keep", cbManage+":admins"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		},
	}}
}

// manageUsersText lists known users.
func manageUsersText(users []manageUser, page, total int) string {
	var sb strings.Builder
	sb.WriteString("👥 <b>Users</b>\n\n")
	if len(users) == 0 {
		sb.WriteString("No known users yet. Use the bot (or create a playlist) and the user appears here.")
		return sb.String()
	}
	sb.WriteString(fmt.Sprintf("<i>Users %d–%d of %d</i>\n\n", page*manageUsersPageSize+1, page*manageUsersPageSize+len(users), total))
	for _, u := range users {
		role := "user"
		if u.Admin {
			role = "admin"
		}
		sb.WriteString(fmt.Sprintf("• <code>%d</code> · %s · %d playlist(s) · %d session(s)\n", u.ID, role, u.Playlists, u.Records))
	}
	sb.WriteString("\nTap a user to see preferences and reset them.")
	return sb.String()
}

// manageUsersKB lists visible users with pagination.
func manageUsersKB(users []manageUser, page, total int) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, u := range users {
		label := fmt.Sprintf("👤 %d", u.ID)
		if u.Admin {
			label = "🛡 " + label
		}
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("%s:user:%d", cbManage, u.ID)),
		})
	}
	pages := (total + manageUsersPageSize - 1) / manageUsersPageSize
	if pages < 1 {
		pages = 1
	}
	if pages > 1 {
		var nav []tgbotapi.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("%s:users:%d", cbManage, page-1)))
		}
		nav = append(nav, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d/%d", page+1, pages), cbManage+":noop"))
		if page+1 < pages {
			nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("%s:users:%d", cbManage, page+1)))
		}
		rows = append(rows, nav)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// manageUserText renders one user's profile.
func manageUserText(u manageUserDetail) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👤 <b>User %d</b>\n\n", u.ID))
	role := "user"
	if u.Admin {
		role = "administrator"
	}
	sb.WriteString(fmt.Sprintf("Role: <b>%s</b>\n", role))
	sb.WriteString(fmt.Sprintf("Playlists: <b>%d</b>\n", u.Playlists))
	sb.WriteString(fmt.Sprintf("Session records: <b>%d</b>\n\n", u.Records))
	sb.WriteString("<b>Preferences</b>\n")
	if !u.HasPrefs {
		sb.WriteString("<i>No saved preferences; the defaults apply.</i>\n")
	} else {
		lang := u.Pref.AudioLang
		if strings.TrimSpace(lang) == "" {
			lang = store.DefaultAudioLang
		}
		mode := u.Pref.MediaMode
		if strings.TrimSpace(mode) == "" {
			mode = store.DefaultMediaMode
		}
		sb.WriteString(fmt.Sprintf("• Audio language: <b>%s</b> (%s)\n", esc(lang), esc(engine.AudioLangDisplayName(lang))))
		sb.WriteString(fmt.Sprintf("• Media mode: <b>%s</b>\n", esc(engine.PlaybackModeDisplay(engine.NormalizePlaybackMode(mode)))))
	}
	if len(u.History) > 0 {
		sb.WriteString("\n<b>Latest sessions</b>\n")
		limit := len(u.History)
		if limit > 5 {
			limit = 5
		}
		for _, r := range u.History[:limit] {
			title := r.Title
			if strings.TrimSpace(title) == "" {
				title = r.MediaURL
			}
			sb.WriteString(fmt.Sprintf("• %s <code>%s</code> %s\n", stateEmoji(r.State), esc(truncate(r.ID, 18)), esc(truncate(title, 38))))
		}
	}
	sb.WriteString("\n<i>Reset preferences to make this user fall back to the configured defaults. The history and playlists are kept.</i>")
	return sb.String()
}

// manageUserKB renders the per-user actions.
func manageUserKB(u manageUserDetail) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("♻️ Reset preferences", fmt.Sprintf("%s:user:%d:reset", cbManage, u.ID)),
			tgbotapi.NewInlineKeyboardButtonData("🕘 History", fmt.Sprintf("%s:user:%d:hist", cbManage, u.ID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("👥 All users", cbManage+":users"),
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		},
	}
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// manageUserResetText confirms a preference reset.
func manageUserResetText(id int64) string {
	return fmt.Sprintf("♻️ <b>Reset preferences for %d?</b>\n\nThe audio-language and media-mode choices are removed, so the defaults apply on the next stream. Playlists and session history are not touched.", id)
}

// manageUserResetKB confirms a preference reset.
func manageUserResetKB(id int64) *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("♻️ Reset", fmt.Sprintf("%s:user:%d:resetok", cbManage, id)),
			tgbotapi.NewInlineKeyboardButtonData("✖️ Keep", fmt.Sprintf("%s:user:%d", cbManage, id)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		},
	}}
}

// manageUserHistoryText renders a bounded history for one user.
func manageUserHistoryText(id int64, records []store.Record) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🕘 <b>History for %d</b>\n\n", id))
	if len(records) == 0 {
		sb.WriteString("No session records for this user.")
		return sb.String()
	}
	limit := len(records)
	if limit > 10 {
		limit = 10
	}
	for _, r := range records[:limit] {
		title := r.Title
		if strings.TrimSpace(title) == "" {
			title = r.MediaURL
		}
		sb.WriteString(fmt.Sprintf("%s <code>%s</code>\n", stateEmoji(r.State), esc(truncate(r.ID, 22))))
		sb.WriteString("   " + esc(truncate(title, 60)) + "\n")
		if !r.CreatedAt.IsZero() {
			sb.WriteString("   " + r.CreatedAt.Format("2006-01-02 15:04") + "\n")
		}
		if r.Error != "" {
			sb.WriteString("   🔴 " + esc(truncate(r.Error, 80)) + "\n")
		}
		sb.WriteString("\n")
	}
	if len(records) > limit {
		sb.WriteString(fmt.Sprintf("<i>… and %d older record(s)</i>", len(records)-limit))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// managePlaylistsText renders global library statistics.
func managePlaylistsText(st store.PlaylistStats, owners []int64) string {
	var sb strings.Builder
	sb.WriteString("📚 <b>Playlist library</b>\n\n")
	sb.WriteString(playlistOverviewText(st))
	sb.WriteString("\n\n")
	if len(owners) == 0 {
		sb.WriteString("No stored playlists yet. Users create them from 📚 Playlists on the dashboard.")
		return sb.String()
	}
	sb.WriteString("<b>Owners</b>\n")
	limit := len(owners)
	if limit > 10 {
		limit = 10
	}
	for _, id := range owners[:limit] {
		sb.WriteString(fmt.Sprintf("• <code>%d</code>\n", id))
	}
	if len(owners) > limit {
		sb.WriteString(fmt.Sprintf("<i>… and %d more owner(s)</i>\n", len(owners)-limit))
	}
	sb.WriteString("\n<i>Per-user libraries are private: the console only shows aggregate counts, not playlist contents.</i>")
	return sb.String()
}

// manageStorageText renders disk, database and media-manager state.
func manageStorageText(snap health.Snapshot, mediaStatus media.Status, files []manageFileInfo) string {
	var sb strings.Builder
	sb.WriteString("💾 <b>Storage</b>\n\n")
	sb.WriteString(fmt.Sprintf("Disk: <b>%.1f GB</b> free of %.1f GB\n", snap.DiskFreeGB, snap.DiskTotalGB))
	sb.WriteString(fmt.Sprintf("Data directory: <code>%s</code>\n\n", esc(snap.DataDir)))
	sb.WriteString("<b>State files</b>\n")
	for _, f := range files {
		if !f.Exists {
			sb.WriteString(fmt.Sprintf("• %s: <i>not created yet</i>\n", f.Label))
			continue
		}
		sb.WriteString(fmt.Sprintf("• %s: <b>%s</b>\n", f.Label, manageBytes(f.Bytes)))
	}
	sb.WriteString("\n<b>Download &amp; Stream</b>\n")
	if !mediaStatus.Enabled {
		sb.WriteString("<i>Disabled on this installation.</i>\n")
	} else {
		sb.WriteString(fmt.Sprintf("• Directory: <code>%s</code>\n", esc(mediaStatus.Dir)))
		sb.WriteString(fmt.Sprintf("• Files: <b>%d</b> · %s\n", mediaStatus.ManagedFiles, manageBytes(mediaStatus.ManagedBytes)))
		sb.WriteString(fmt.Sprintf("• Leased: <b>%d</b> · TTL %d min\n", mediaStatus.LeasedFiles, mediaStatus.TTLMinutes))
		sb.WriteString(fmt.Sprintf("• yt-dlp: %s · ffmpeg: %s\n", manageYesNo(mediaStatus.YTDLPAvailable), manageYesNo(mediaStatus.FFmpegAvailable)))
	}
	sb.WriteString("\n<i>Managed downloads expire on their own; the TTL reclaims files no session still needs.</i>")
	return sb.String()
}

// manageStorageKB offers refresh and maintenance.
func manageStorageKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbManage+":storage"),
			tgbotapi.NewInlineKeyboardButtonData("🧰 Maintenance", cbManage+":maint"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
		},
	}}
}

// manageMaintenanceText explains the maintenance actions.
func manageMaintenanceText(st store.AdminStats) string {
	var sb strings.Builder
	sb.WriteString("🧰 <b>Maintenance</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session records: <b>%d</b> · preferences: <b>%d</b>\n\n", st.Records, st.Preferences))
	sb.WriteString("• <b>Vacuum playlists</b> rewrites <code>playlists.db</code> without free pages. It is safe while the bot runs; it may take a moment on a large library.\n\n")
	sb.WriteString("• <b>Clear history</b> removes the saved session records (the cards used to recover interrupted sessions). Preferences and playlists stay.\n\n")
	sb.WriteString("• <b>Reset preferences</b> removes every saved audio-language and media-mode choice. Running sessions are not changed.\n\n")
	sb.WriteString("<i>Every action below has its own confirmation.</i>")
	return sb.String()
}

// manageMaintenanceKB offers the maintenance actions.
func manageMaintenanceKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🧹 Vacuum playlists", cbManage+":vacuum"),
			tgbotapi.NewInlineKeyboardButtonData("🕘 Clear history", cbManage+":hist"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("♻️ Reset all prefs", cbManage+":prefs"),
			tgbotapi.NewInlineKeyboardButtonData("🗄 Backups", cbManage+":backups"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
		},
	}}
}

// manageClearHistoryText confirms clearing the session history.
func manageClearHistoryText(records int) string {
	return fmt.Sprintf("⚠️ <b>Clear session history?</b>\n\nThis removes <b>%d</b> saved session record(s). Interrupted-session recovery and the per-user history screens will be empty.\n\nPlaylists and preferences are kept.", records)
}

// manageClearHistoryKB confirms clearing the history.
func manageClearHistoryKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🗑 Clear history", cbManage+":histok"),
			tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", cbManage+":maint"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		},
	}}
}

// manageResetPrefsText confirms resetting every preference.
func manageResetPrefsText(prefs int) string {
	return fmt.Sprintf("⚠️ <b>Reset every preference?</b>\n\nThis removes the saved audio-language and media-mode choices of <b>%d</b> user(s). Everyone falls back to the configured defaults on their next stream.\n\nSession history and playlists are kept.", prefs)
}

// manageResetPrefsKB confirms resetting all preferences.
func manageResetPrefsKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("♻️ Reset all", cbManage+":prefsok"),
			tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", cbManage+":maint"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		},
	}}
}

// manageBackupsText renders the backup list.
func manageBackupsText(root string, list []backup.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("🗄 <b>Backups</b>\n\n")
	sb.WriteString(fmt.Sprintf("Root: <code>%s</code>\n", esc(root)))
	if len(list) == 0 {
		sb.WriteString("\nNo backups yet. Creating one snapshots the config, session history, saved meetings and playlists.")
		return sb.String()
	}
	var total int64
	for _, s := range list {
		total += s.Bytes
	}
	sb.WriteString(fmt.Sprintf("Snapshots: <b>%d</b> · total <b>%s</b>\n\n", len(list), manageBytes(total)))
	limit := len(list)
	if limit > 5 {
		limit = 5
	}
	for _, s := range list[:limit] {
		sb.WriteString(fmt.Sprintf("• %s · %s · %d file(s)\n",
			s.CreatedAt.Format("2006-01-02 15:04:05"), manageBytes(s.Bytes), len(s.Files)))
	}
	if len(list) > limit {
		sb.WriteString(fmt.Sprintf("<i>… and %d older snapshot(s)</i>\n", len(list)-limit))
	}
	sb.WriteString("\n<i>Snapshots are produced with SQLite VACUUM INTO for the databases, so a live WAL cannot leave a half-written copy.</i>")
	return sb.String()
}

// manageBackupsKB offers create/prune/refresh.
func manageBackupsKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("➕ Create backup", cbManage+":bnew"),
			tgbotapi.NewInlineKeyboardButtonData("🧹 Prune old", cbManage+":bprune"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbManage+":backups"),
			tgbotapi.NewInlineKeyboardButtonData("🧰 Maintenance", cbManage+":maint"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
		},
	}}
}

// manageBroadcastText prompts for a broadcast message.
func manageBroadcastText(recipients int) string {
	var sb strings.Builder
	sb.WriteString("📣 <b>Broadcast</b>\n\n")
	sb.WriteString(fmt.Sprintf("Recipients: <b>%d</b> admin(s)\n\n", recipients))
	sb.WriteString("Send the message that should be delivered to every admin. HTML formatting is allowed, exactly as in normal bot messages.\n\n")
	sb.WriteString("<i>The message is sent once you confirm the preview. It is not retried automatically.</i>\n\n")
	sb.WriteString("Send /cancel to abort.")
	return sb.String()
}

// manageBroadcastPreviewText shows the composed message before sending.
func manageBroadcastPreviewText(recipients int, payload string) string {
	var sb strings.Builder
	sb.WriteString("📣 <b>Broadcast preview</b>\n\n")
	sb.WriteString(fmt.Sprintf("To: <b>%d</b> admin(s)\n", recipients))
	sb.WriteString(fmt.Sprintf("Length: %d characters\n\n", len([]rune(payload))))
	sb.WriteString("<b>Message (as text)</b>\n<pre>")
	sb.WriteString(esc(payload))
	sb.WriteString("</pre>\n\n<i>Send it now? Formatting tags are interpreted by Telegram when delivered.</i>")
	return sb.String()
}

// manageBroadcastPreviewKB confirms a broadcast.
func manageBroadcastPreviewKB(recipients int) *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("📣 Send to %d", recipients), cbManage+":bcsend"),
			tgbotapi.NewInlineKeyboardButtonData("✏️ Rewrite", cbManage+":bc"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", callbackCancelBroadcast()),
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		},
	}}
}

// callbackCancelBroadcast clears the composer and returns home.
func callbackCancelBroadcast() string { return cbManage + ":bccancel" }

// manageDiagnosticsKB returns from the diagnostics screen.
func manageDiagnosticsKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbManage+":diag"),
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
		},
	}}
}

// manageBytes renders a byte count with a binary unit.
func manageBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// manageYesNo renders a boolean for the storage screen.
func manageYesNo(v bool) string {
	if v {
		return "✅"
	}
	return "🔴"
}

// manageAge renders how long ago a snapshot was created.
func manageAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
