package bot

// Management console: callbacks, text steps and the data-gathering behind the
// screens in manage_views.go.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/backup"
	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

const (
	// manageUsersPageSize is the number of users per screen. The rows are
	// short, so more than eight would push the navigation off-screen.
	manageUsersPageSize = 8
	// manageBackupKeep is how many snapshots "prune" preserves.
	manageBackupKeep = 10
	// manageBroadcastMaxRunes leaves room under Telegram's 4096-character
	// message ceiling for the preview envelope.
	manageBroadcastMaxRunes = 3600
)

// sendManageHome opens the console as a fresh message (/manage).
func (b *Bot) sendManageHome(chatID, userID int64) {
	b.sendText(chatID, b.manageHomeText(userID), manageHomeKB())
}

// manageHomeText gathers the overview counters.
func (b *Bot) manageHomeText(userID int64) string {
	return manageHomeText(userID, b.st.Stats(), b.managePlaylistStats(), len(b.cfg.AdminIDs()))
}

// manageEdit edits msgID when it is known, otherwise sends a new message.
func (b *Bot) manageEdit(chatID int64, msgID int, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	if msgID > 0 {
		_ = b.api.Edit(chatID, msgID, text, kb)
		return
	}
	b.sendText(chatID, text, kb)
}

// handleManageCallback routes every "menu:manage..." press.
func (b *Bot) handleManageCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	rest := strings.TrimPrefix(data, cbManage)
	rest = strings.TrimPrefix(rest, ":")
	parts := strings.Split(rest, ":")
	verb := parts[0]
	if verb == "" {
		verb = "home"
	}
	b.log.Infof("[PIPELINE:manage] callback verb=%q parts=%v user=%d chat=%d", verb, parts, userID, chatID)
	switch verb {
	case "noop":
		_ = b.api.AnswerCallback(q.ID, "")
	case "home":
		_ = b.api.AnswerCallback(q.ID, "")
		b.manageEdit(chatID, msgID, b.manageHomeText(userID), manageHomeKB())
	case "admins":
		_ = b.api.AnswerCallback(q.ID, "")
		b.showManageAdmins(chatID, userID, msgID)
	case "adminadd":
		_ = b.api.AnswerCallback(q.ID, "send the Telegram id")
		b.setFlow(userID, &flow{step: stepAdminAdd, parent: "manage"})
		b.manageEdit(chatID, msgID, manageAdminAddText(), playlistCancelKB())
	case "adminrm":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad admin id")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad admin id")
			return
		}
		_ = b.api.AnswerCallback(q.ID, "")
		b.manageEdit(chatID, msgID, manageAdminRemoveText(id), manageAdminRemoveKB(id))
	case "adminrmok":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad admin id")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad admin id")
			return
		}
		if !b.cfg.RemoveAdmin(id) {
			if len(b.cfg.AdminIDs()) <= 1 {
				_ = b.api.AnswerCallback(q.ID, "cannot remove the last admin")
			} else {
				_ = b.api.AnswerCallback(q.ID, "admin not found")
			}
			b.showManageAdmins(chatID, userID, msgID)
			return
		}
		if err := b.cfg.Save(b.cfgPath); err != nil {
			b.log.Warnf("[PIPELINE:manage] saving admins after removal: %v", err)
			_ = b.api.AnswerCallback(q.ID, "removed (save failed)")
		} else {
			_ = b.api.AnswerCallback(q.ID, "admin removed")
		}
		b.showManageAdmins(chatID, userID, msgID)
	case "users":
		page := 0
		if len(parts) > 1 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				page = n
			}
		}
		_ = b.api.AnswerCallback(q.ID, "")
		b.showManageUsers(chatID, userID, msgID, page)
	case "user":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad user")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad user")
			return
		}
		action := ""
		if len(parts) > 2 {
			action = parts[2]
		}
		switch action {
		case "":
			_ = b.api.AnswerCallback(q.ID, "")
			b.showManageUser(chatID, userID, msgID, id)
		case "reset":
			_ = b.api.AnswerCallback(q.ID, "")
			b.manageEdit(chatID, msgID, manageUserResetText(id), manageUserResetKB(id))
		case "resetok":
			removed := b.st.ResetUser(id)
			if removed {
				_ = b.api.AnswerCallback(q.ID, "preferences reset")
			} else {
				_ = b.api.AnswerCallback(q.ID, "no preferences were saved")
			}
			b.showManageUser(chatID, userID, msgID, id)
		case "hist":
			_ = b.api.AnswerCallback(q.ID, "")
			records := b.st.HistoryForUser(id, 0)
			kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{
					tgbotapi.NewInlineKeyboardButtonData("⬅️ User", fmt.Sprintf("%s:user:%d", cbManage, id)),
					tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
				},
			}}
			b.manageEdit(chatID, msgID, manageUserHistoryText(id, records), kb)
		default:
			_ = b.api.AnswerCallback(q.ID, "unknown user action")
		}
	case "playlists":
		_ = b.api.AnswerCallback(q.ID, "")
		text := managePlaylistsText(b.managePlaylistStats(), b.managePlaylistOwners())
		b.manageEdit(chatID, msgID, text, manageBackKB())
	case "storage":
		_ = b.api.AnswerCallback(q.ID, "")
		b.showManageStorage(chatID, msgID)
	case "maint":
		_ = b.api.AnswerCallback(q.ID, "")
		b.showManageMaintenance(chatID, msgID)
	case "vacuum":
		_ = b.api.AnswerCallback(q.ID, "vacuuming…")
		if err := b.manageVacuumPlaylists(); err != nil {
			b.log.Warnf("[PIPELINE:manage] vacuum playlists: %v", err)
			_ = b.api.AnswerCallback(q.ID, "vacuum failed: "+truncate(err.Error(), 60))
		} else {
			_ = b.api.AnswerCallback(q.ID, "playlists vacuumed")
		}
		b.showManageMaintenance(chatID, msgID)
	case "hist":
		_ = b.api.AnswerCallback(q.ID, "")
		b.manageEdit(chatID, msgID, manageClearHistoryText(b.st.Stats().Records), manageClearHistoryKB())
	case "histok":
		n := b.st.ClearHistory()
		_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("cleared %d record(s)", n))
		b.showManageMaintenance(chatID, msgID)
	case "prefs":
		_ = b.api.AnswerCallback(q.ID, "")
		b.manageEdit(chatID, msgID, manageResetPrefsText(b.st.Stats().Preferences), manageResetPrefsKB())
	case "prefsok":
		n := b.st.ResetAllPreferences()
		_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("reset %d user(s)", n))
		b.showManageMaintenance(chatID, msgID)
	case "backups":
		_ = b.api.AnswerCallback(q.ID, "")
		b.showManageBackups(chatID, msgID)
	case "bnew":
		snap, err := backup.Create(b.manageBackupRoot(), b.manageBackupSources())
		if err != nil {
			b.log.Warnf("[PIPELINE:manage] create backup: %v", err)
			_ = b.api.AnswerCallback(q.ID, "backup failed: "+truncate(err.Error(), 50))
		} else {
			_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("backup created (%s)", manageBytes(snap.Bytes)))
		}
		b.showManageBackups(chatID, msgID)
	case "bprune":
		n, err := backup.Prune(b.manageBackupRoot(), manageBackupKeep)
		if err != nil {
			b.log.Warnf("[PIPELINE:manage] prune backups: %v", err)
			_ = b.api.AnswerCallback(q.ID, "prune failed: "+truncate(err.Error(), 60))
		} else {
			_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("removed %d old snapshot(s)", n))
		}
		b.showManageBackups(chatID, msgID)
	case "bc":
		_ = b.api.AnswerCallback(q.ID, "send the broadcast text")
		b.setFlow(userID, &flow{step: stepAdminBroadcast, parent: "manage"})
		b.manageEdit(chatID, msgID, manageBroadcastText(len(b.cfg.AdminIDs())), playlistCancelKB())
	case "bcsend":
		f := b.getFlow(userID)
		if f == nil || f.step != stepAdminBroadcast || strings.TrimSpace(f.payload) == "" {
			_ = b.api.AnswerCallback(q.ID, "nothing to send")
			return
		}
		payload := f.payload
		b.clearFlow(userID)
		recipients := len(b.cfg.AdminIDs())
		_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("sending to %d admin(s)", recipients))
		b.Broadcast(payload)
		kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("📣 New broadcast", cbManage+":bc"),
				tgbotapi.NewInlineKeyboardButtonData("⬅️ Management", mgHome()),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
			},
		}}
		b.manageEdit(chatID, msgID, fmt.Sprintf("✅ <b>Broadcast sent</b>\n\nDelivered to <b>%d</b> admin(s). Check the log for per-recipient failures.", recipients), kb)
	case "bccancel":
		b.clearFlow(userID)
		_ = b.api.AnswerCallback(q.ID, "cancelled")
		b.manageEdit(chatID, msgID, b.manageHomeText(userID), manageHomeKB())
	case "diag":
		_ = b.api.AnswerCallback(q.ID, "")
		b.manageEdit(chatID, msgID, b.healthText(), manageDiagnosticsKB())
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown management action")
	}
}

// showManageAdmins renders the allow-list.
func (b *Bot) showManageAdmins(chatID, userID int64, msgID int) {
	ids := b.cfg.AdminIDs()
	b.manageEdit(chatID, msgID, manageAdminsText(ids, userID), manageAdminsKB(ids, userID))
}

// showManageUsers renders one page of known users.
func (b *Bot) showManageUsers(chatID, userID int64, msgID, page int) {
	_ = userID
	users := b.manageUsers()
	if len(users) == 0 {
		b.manageEdit(chatID, msgID, manageUsersText(nil, 0, 0), manageUsersKB(nil, 0, 0))
		return
	}
	total := len(users)
	if page < 0 {
		page = 0
	}
	pages := (total + manageUsersPageSize - 1) / manageUsersPageSize
	if page >= pages {
		page = pages - 1
	}
	start := page * manageUsersPageSize
	end := start + manageUsersPageSize
	if end > total {
		end = total
	}
	visible := users[start:end]
	b.manageEdit(chatID, msgID, manageUsersText(visible, page, total), manageUsersKB(visible, page, total))
}

// manageUsers builds the complete user list once per screen.
func (b *Bot) manageUsers() []manageUser {
	owners := b.managePlaylistOwners()
	ids := b.st.KnownUserIDs(owners...)
	out := make([]manageUser, 0, len(ids))
	for _, id := range ids {
		pref, has := b.st.UserPreference(id)
		out = append(out, manageUser{
			ID:        id,
			Admin:     b.cfg.IsAdmin(id),
			HasPrefs:  has,
			AudioLang: pref.AudioLang,
			MediaMode: pref.MediaMode,
			Playlists: b.managePlaylistCount(id),
			Records:   len(b.st.HistoryForUser(id, 0)),
		})
	}
	return out
}

// showManageUser renders one user's profile.
func (b *Bot) showManageUser(chatID, userID int64, msgID int, id int64) {
	_ = userID
	pref, has := b.st.UserPreference(id)
	detail := manageUserDetail{
		manageUser: manageUser{
			ID:        id,
			Admin:     b.cfg.IsAdmin(id),
			HasPrefs:  has,
			AudioLang: pref.AudioLang,
			MediaMode: pref.MediaMode,
			Playlists: b.managePlaylistCount(id),
			Records:   len(b.st.HistoryForUser(id, 0)),
		},
		Pref:    pref,
		History: b.st.HistoryForUser(id, 5),
	}
	b.manageEdit(chatID, msgID, manageUserText(detail), manageUserKB(detail))
}

// showManageStorage renders files, disk and media-manager state.
func (b *Bot) showManageStorage(chatID int64, msgID int) {
	var mediaStatus media.Status
	if mgr := b.mediaMgr(); mgr != nil {
		mediaStatus = mgr.Snapshot()
	}
	b.manageEdit(chatID, msgID, manageStorageText(b.Snapshot(), mediaStatus, b.manageFiles()), manageStorageKB())
}

// showManageMaintenance renders the maintenance landing page.
func (b *Bot) showManageMaintenance(chatID int64, msgID int) {
	b.manageEdit(chatID, msgID, manageMaintenanceText(b.st.Stats()), manageMaintenanceKB())
}

// showManageBackups renders the snapshot list.
func (b *Bot) showManageBackups(chatID int64, msgID int) {
	root := b.manageBackupRoot()
	list, err := backup.List(root)
	if err != nil {
		b.log.Warnf("[PIPELINE:manage] list backups: %v", err)
	}
	b.manageEdit(chatID, msgID, manageBackupsText(root, list), manageBackupsKB())
}

// managePlaylistStats returns global playlist counters, or zeroes when the
// library is unavailable.
func (b *Bot) managePlaylistStats() store.PlaylistStats {
	if b.pl == nil {
		return store.PlaylistStats{}
	}
	st, err := playlistCall(b, func(ctx context.Context) (store.PlaylistStats, error) {
		return b.pl.Stats(ctx)
	})
	if err != nil {
		b.log.Warnf("[PIPELINE:manage] playlist stats: %v", err)
		return store.PlaylistStats{}
	}
	return st
}

// managePlaylistOwners returns the distinct owners, or nil.
func (b *Bot) managePlaylistOwners() []int64 {
	if b.pl == nil {
		return nil
	}
	owners, err := playlistCall(b, func(ctx context.Context) ([]int64, error) {
		return b.pl.Owners(ctx)
	})
	if err != nil {
		b.log.Warnf("[PIPELINE:manage] playlist owners: %v", err)
		return nil
	}
	return owners
}

// managePlaylistCount counts one owner's playlists.
func (b *Bot) managePlaylistCount(ownerID int64) int {
	if b.pl == nil || ownerID <= 0 {
		return 0
	}
	n, err := playlistCall(b, func(ctx context.Context) (int, error) {
		return b.pl.CountForOwner(ctx, ownerID)
	})
	if err != nil {
		b.log.Warnf("[PIPELINE:manage] count playlists for %d: %v", ownerID, err)
		return 0
	}
	return n
}

// manageVacuumPlaylists compacts the playlist database when the concrete
// store supports it. The bot only depends on the Playlists interface, so the
// optional method is discovered with a type assertion.
func (b *Bot) manageVacuumPlaylists() error {
	if b.pl == nil {
		return errPlaylistDisabled
	}
	v, ok := b.pl.(interface {
		Vacuum(context.Context) error
	})
	if !ok {
		return errors.New("the configured playlist store cannot vacuum")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return v.Vacuum(ctx)
}

// manageFiles describes the state files shown on the storage screen.
func (b *Bot) manageFiles() []manageFileInfo {
	specs := []struct {
		label string
		path  string
	}{
		{"config", b.cfgPath},
		{"state.json", b.cfg.StateFile()},
		{"meetings.db", b.cfg.MeetingsFile()},
		{"playlists.db", b.cfg.PlaylistsFile()},
	}
	out := make([]manageFileInfo, 0, len(specs))
	for _, s := range specs {
		if strings.TrimSpace(s.path) == "" {
			continue
		}
		info := manageFileInfo{Label: s.label, Path: s.path}
		if st, err := os.Stat(s.path); err == nil && !st.IsDir() {
			info.Bytes = st.Size()
			info.Exists = true
		}
		out = append(out, info)
	}
	return out
}

// manageBackupRoot is <data_dir>/backups.
func (b *Bot) manageBackupRoot() string {
	return filepath.Join(b.cfg.Paths.DataDir, "backups")
}

// manageBackupSources lists every file worth snapshotting. Missing files are
// skipped by the backup package, so a fresh installation still works.
func (b *Bot) manageBackupSources() []backup.Source {
	return []backup.Source{
		{Name: "config.yml", Path: b.cfgPath},
		{Name: "state.json", Path: b.cfg.StateFile()},
		{Name: "meetings.db", Path: b.cfg.MeetingsFile(), SQLite: true},
		{Name: "playlists.db", Path: b.cfg.PlaylistsFile(), SQLite: true},
	}
}

// tryManageInput consumes the console's text steps.
func (b *Bot) tryManageInput(chatID, userID int64, f *flow, text string) bool {
	switch f.step {
	case stepAdminAdd:
		b.finishAdminAdd(chatID, userID, text)
		return true
	case stepAdminBroadcast:
		b.finishBroadcastCompose(chatID, userID, f, text)
		return true
	default:
		return false
	}
}

// finishAdminAdd validates and persists a new administrator.
func (b *Bot) finishAdminAdd(chatID, userID int64, text string) {
	raw := strings.TrimSpace(text)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		b.sendText(chatID, "❌ That is not a numeric Telegram id.\n\nSend the number only, for example <code>5094837833</code>, or /cancel.", playlistCancelKB())
		return
	}
	if !b.cfg.AddAdmin(id) {
		b.clearFlow(userID)
		b.sendText(chatID, fmt.Sprintf("ℹ️ <code>%d</code> is already an administrator.", id), manageBackKB())
		b.showManageAdmins(chatID, userID, 0)
		return
	}
	if err := b.cfg.Save(b.cfgPath); err != nil {
		b.log.Warnf("[PIPELINE:manage] saving new admin: %v", err)
		b.clearFlow(userID)
		b.sendText(chatID, fmt.Sprintf("⚠️ <code>%d</code> was added in memory but the config could not be saved: %s\n\nThe change will be lost on restart unless the file is writable.", id, esc(truncate(err.Error(), 160))), manageBackKB())
		b.showManageAdmins(chatID, userID, 0)
		return
	}
	b.clearFlow(userID)
	b.sendText(chatID, fmt.Sprintf("✅ <code>%d</code> is now an administrator and the config was saved.", id), manageBackKB())
	b.showManageAdmins(chatID, userID, 0)
}

// finishBroadcastCompose stores the composed message and shows a preview.
func (b *Bot) finishBroadcastCompose(chatID, userID int64, f *flow, text string) {
	payload := strings.TrimSpace(text)
	if payload == "" {
		b.sendText(chatID, "❌ The broadcast is empty. Send the message text, or /cancel.", playlistCancelKB())
		return
	}
	if n := len([]rune(payload)); n > manageBroadcastMaxRunes {
		b.sendText(chatID, fmt.Sprintf("❌ The message is %d characters; the limit is %d. Shorten it and send again, or /cancel.", n, manageBroadcastMaxRunes), playlistCancelKB())
		return
	}
	f.payload = payload
	b.setFlow(userID, f)
	recipients := len(b.cfg.AdminIDs())
	b.sendText(chatID, manageBroadcastPreviewText(recipients, payload), manageBroadcastPreviewKB(recipients))
}
