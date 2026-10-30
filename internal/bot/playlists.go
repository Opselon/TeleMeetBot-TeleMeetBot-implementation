package bot

// Playlist library: callbacks, text steps and "play playlist" orchestration.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// Playlists is the persistent library surface the bot needs.
// (*store.PlaylistStore implements it.) A nil value disables the feature
// cleanly instead of panicking on an old data directory.
type Playlists interface {
	Create(ctx context.Context, ownerID int64, name, description string) (store.Playlist, error)
	List(ctx context.Context, ownerID int64) ([]store.Playlist, error)
	Get(ctx context.Context, ownerID, id int64) (store.Playlist, error)
	Rename(ctx context.Context, ownerID, id int64, name string) (store.Playlist, error)
	Describe(ctx context.Context, ownerID, id int64, description string) error
	Delete(ctx context.Context, ownerID, id int64) error
	Duplicate(ctx context.Context, ownerID, id int64) (store.Playlist, error)
	Items(ctx context.Context, ownerID, id int64) ([]store.PlaylistItem, error)
	AddItems(ctx context.Context, ownerID, id int64, items []store.PlaylistItemInput) ([]store.PlaylistItem, error)
	RemoveItem(ctx context.Context, ownerID, playlistID, itemID int64) error
	MoveItem(ctx context.Context, ownerID, playlistID, itemID int64, delta int) error
	ClearItems(ctx context.Context, ownerID, playlistID int64) (int, error)
	Stats(ctx context.Context) (store.PlaylistStats, error)
	Owners(ctx context.Context) ([]int64, error)
	CountForOwner(ctx context.Context, ownerID int64) (int, error)
}

// errPlaylistDisabled is returned when the library is not configured.
var errPlaylistDisabled = errors.New("playlist library is not available on this installation")

// playlistCtx bounds one library operation. SQLite is local, so a short
// timeout is enough; a wedged disk must not hold a Telegram handler open.
const playlistOpTimeout = 5 * time.Second

// playlistCall runs a library operation with a bounded context.
func playlistCall[T any](b *Bot, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if b.pl == nil {
		return zero, errPlaylistDisabled
	}
	ctx, cancel := context.WithTimeout(context.Background(), playlistOpTimeout)
	defer cancel()
	return fn(ctx)
}

// plErr maps storage errors to a short, safe toast.
func plErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errPlaylistDisabled):
		return "playlists unavailable"
	case errors.Is(err, store.ErrPlaylistNotFound):
		return "playlist not found"
	case errors.Is(err, store.ErrPlaylistItemNotFound):
		return "item not found"
	case errors.Is(err, store.ErrPlaylistNameTaken):
		return "name already used"
	case errors.Is(err, store.ErrTooManyPlaylists):
		return "playlist limit reached"
	case errors.Is(err, store.ErrTooManyPlaylistItems):
		return "item limit reached"
	case errors.Is(err, store.ErrPlaylistEmpty):
		return "playlist is empty"
	case errors.Is(err, store.ErrPlaylistNameInvalid):
		return err.Error()
	case errors.Is(err, store.ErrPlaylistURLInvalid):
		return err.Error()
	case errors.Is(err, store.ErrPlaylistDescriptionInvalid):
		return err.Error()
	default:
		return truncate(err.Error(), 110)
	}
}

// parseID parses one numeric callback component.
func parseID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// sendPlaylists opens the library as a fresh message (/playlists).
func (b *Bot) sendPlaylists(chatID, userID int64) {
	if b.pl == nil {
		b.sendText(chatID, "📚 The playlist library is unavailable on this installation.", mainMenuKB())
		return
	}
	list, err := playlistCall(b, func(ctx context.Context) ([]store.Playlist, error) {
		return b.pl.List(ctx, userID)
	})
	if err != nil {
		b.log.Warnf("[PIPELINE:playlists] list %d: %v", userID, err)
		b.sendText(chatID, "🔴 Could not load your playlists: "+esc(plErr(err)), mainMenuKB())
		return
	}
	b.sendText(chatID, playlistListText(list, 0, len(list)), playlistListKB(list, 0))
}

// showPlaylistList edits (or sends) the library landing page.
func (b *Bot) showPlaylistList(chatID, userID int64, msgID, page int) {
	if b.pl == nil {
		b.sendText(chatID, "📚 The playlist library is unavailable on this installation.", mainMenuKB())
		return
	}
	list, err := playlistCall(b, func(ctx context.Context) ([]store.Playlist, error) {
		return b.pl.List(ctx, userID)
	})
	if err != nil {
		b.log.Warnf("[PIPELINE:playlists] list %d: %v", userID, err)
		if msgID > 0 {
			_ = b.api.Edit(chatID, msgID, "🔴 Could not load your playlists: "+esc(plErr(err)), backKB())
		} else {
			b.sendText(chatID, "🔴 Could not load your playlists: "+esc(plErr(err)), backKB())
		}
		return
	}
	text := playlistListText(list, page, len(list))
	kb := playlistListKB(list, page)
	if msgID > 0 {
		_ = b.api.Edit(chatID, msgID, text, kb)
		return
	}
	b.sendText(chatID, text, kb)
}

// showPlaylist renders one playlist. msgID == 0 sends a new message.
func (b *Bot) showPlaylist(chatID, userID int64, msgID int, id int64, page int) {
	if b.pl == nil {
		b.sendText(chatID, "📚 The playlist library is unavailable on this installation.", mainMenuKB())
		return
	}
	p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
		return b.pl.Get(ctx, userID, id)
	})
	if err != nil {
		b.log.Warnf("[PIPELINE:playlists] get %d by %d: %v", id, userID, err)
		if msgID > 0 {
			_ = b.api.Edit(chatID, msgID, "🔴 "+esc(plErr(err)), mainMenuKB())
		} else {
			b.sendText(chatID, "🔴 "+esc(plErr(err)), mainMenuKB())
		}
		return
	}
	items, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
		return b.pl.Items(ctx, userID, id)
	})
	if err != nil {
		b.log.Warnf("[PIPELINE:playlists] items %d by %d: %v", id, userID, err)
		items = nil
	}
	visible, page, total := playlistPage(items, page, maxPlaylistPageSize)
	text := playlistDetailText(p, visible, page, total)
	kb := playlistDetailKB(p, items, page, total)
	if msgID > 0 {
		_ = b.api.Edit(chatID, msgID, text, kb)
		return
	}
	b.sendText(chatID, text, kb)
}

// showPlaylistItem renders one item, with a Back button that returns to the
// correct item page.
func (b *Bot) showPlaylistItem(chatID, userID int64, msgID int, playlistID, itemID int64) {
	p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
		return b.pl.Get(ctx, userID, playlistID)
	})
	if err != nil {
		_ = b.api.Edit(chatID, msgID, "🔴 "+esc(plErr(err)), mainMenuKB())
		return
	}
	items, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
		return b.pl.Items(ctx, userID, playlistID)
	})
	if err != nil {
		_ = b.api.Edit(chatID, msgID, "🔴 "+esc(plErr(err)), mainMenuKB())
		return
	}
	index := playlistItemIndex(items, itemID)
	if index < 0 {
		_ = b.api.Edit(chatID, msgID, "This item is gone.", backKB())
		return
	}
	_ = b.api.Edit(chatID, msgID, playlistItemText(p, items[index], index, len(items)),
		playlistItemKB(p, items[index], index, len(items)))
}

// playlistItemIndex finds an item's zero-based position.
func playlistItemIndex(items []store.PlaylistItem, itemID int64) int {
	for i, it := range items {
		if it.ID == itemID {
			return i
		}
	}
	return -1
}

// handlePlaylistCallback routes every "pl:" button.
func (b *Bot) handlePlaylistCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.Split(strings.TrimPrefix(data, cbPlaylist), ":")
	if len(parts) == 0 || parts[0] == "" {
		_ = b.api.AnswerCallback(q.ID, "bad playlist action")
		return
	}
	verb := parts[0]
	b.log.Infof("[PIPELINE:playlists] callback verb=%q parts=%v user=%d chat=%d", verb, parts, userID, chatID)
	if b.pl == nil {
		_ = b.api.AnswerCallback(q.ID, "playlists unavailable")
		return
	}
	switch verb {
	case "noop":
		_ = b.api.AnswerCallback(q.ID, "")
	case "home":
		_ = b.api.AnswerCallback(q.ID, "")
		b.showPlaylistList(chatID, userID, msgID, 0)
	case "list":
		page := 0
		if len(parts) > 1 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				page = n
			}
		}
		_ = b.api.AnswerCallback(q.ID, "")
		b.showPlaylistList(chatID, userID, msgID, page)
	case "new":
		_ = b.api.AnswerCallback(q.ID, "send a name")
		b.setFlow(userID, &flow{step: stepPlaylistName, parent: "playlist"})
		_ = b.api.Edit(chatID, msgID, playlistNameText("new", ""), playlistCancelKB())
	case "open":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		page := 0
		if len(parts) > 2 {
			if n, err := strconv.Atoi(parts[2]); err == nil {
				page = n
			}
		}
		_ = b.api.AnswerCallback(q.ID, "")
		b.showPlaylist(chatID, userID, msgID, id, page)
	case "i":
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		pid, ok1 := parseID(parts[1])
		iid, ok2 := parseID(parts[2])
		if !ok1 || !ok2 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		_ = b.api.AnswerCallback(q.ID, "")
		b.showPlaylistItem(chatID, userID, msgID, pid, iid)
	case "up", "dn":
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		pid, ok1 := parseID(parts[1])
		iid, ok2 := parseID(parts[2])
		if !ok1 || !ok2 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		delta := -1
		if verb == "dn" {
			delta = 1
		}
		err := playlistCallVoid(b, func(ctx context.Context) error {
			return b.pl.MoveItem(ctx, userID, pid, iid, delta)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
		} else if delta < 0 {
			_ = b.api.AnswerCallback(q.ID, "moved up")
		} else {
			_ = b.api.AnswerCallback(q.ID, "moved down")
		}
		b.showPlaylistItem(chatID, userID, msgID, pid, iid)
	case "rm":
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		pid, ok1 := parseID(parts[1])
		iid, ok2 := parseID(parts[2])
		if !ok1 || !ok2 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, pid)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		items, _ := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
			return b.pl.Items(ctx, userID, pid)
		})
		idx := playlistItemIndex(items, iid)
		if idx < 0 {
			_ = b.api.AnswerCallback(q.ID, "item not found")
			return
		}
		title := items[idx].Title
		if strings.TrimSpace(title) == "" {
			title = store.DefaultPlaylistItemTitle(items[idx].URL)
		}
		_ = b.api.AnswerCallback(q.ID, "")
		text := fmt.Sprintf("⚠️ <b>Remove this item?</b>\n\n<b>%s</b>\n\nIt will be deleted from <b>%s</b>.", esc(title), esc(p.Name))
		kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🗑 Remove", fmt.Sprintf("pl:rmok:%d:%d", pid, iid))},
			{tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", fmt.Sprintf("pl:i:%d:%d", pid, iid))},
		}}
		_ = b.api.Edit(chatID, msgID, text, kb)
	case "rmok":
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		pid, ok1 := parseID(parts[1])
		iid, ok2 := parseID(parts[2])
		if !ok1 || !ok2 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		err := playlistCallVoid(b, func(ctx context.Context) error {
			return b.pl.RemoveItem(ctx, userID, pid, iid)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "removed")
		b.showPlaylist(chatID, userID, msgID, pid, 0)
	case "clr":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, playlistClearText(p), playlistClearKB(id))
	case "clrok":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		n, err := playlistCall(b, func(ctx context.Context) (int, error) {
			return b.pl.ClearItems(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("cleared %d items", n))
		b.showPlaylist(chatID, userID, msgID, id, 0)
	case "del":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, playlistDeleteText(p), playlistDeleteKB(id))
	case "delok":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		err := playlistCallVoid(b, func(ctx context.Context) error {
			return b.pl.Delete(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "deleted")
		b.showPlaylistList(chatID, userID, msgID, 0)
	case "ren":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		b.setFlow(userID, &flow{step: stepPlaylistRename, playlistID: id, parent: "playlist"})
		_ = b.api.AnswerCallback(q.ID, "send the new name")
		_ = b.api.Edit(chatID, msgID, playlistNameText("rename", p.Name), playlistCancelKB())
	case "desc":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		b.setFlow(userID, &flow{step: stepPlaylistDesc, playlistID: id, parent: "playlist"})
		_ = b.api.AnswerCallback(q.ID, "send a description")
		_ = b.api.Edit(chatID, msgID, playlistDescText(p), playlistCancelKB())
	case "add":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		b.setFlow(userID, &flow{step: stepPlaylistAdd, playlistID: id, parent: "playlist"})
		_ = b.api.AnswerCallback(q.ID, "send links")
		_ = b.api.Edit(chatID, msgID, playlistAddText(p), playlistCancelKB())
	case "imp":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		b.setFlow(userID, &flow{step: stepPlaylistImport, playlistID: id, parent: "playlist"})
		_ = b.api.AnswerCallback(q.ID, "paste the list")
		_ = b.api.Edit(chatID, msgID, playlistImportText(p), playlistCancelKB())
	case "dup":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		copyP, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Duplicate(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "duplicated")
		b.showPlaylist(chatID, userID, msgID, copyP.ID, 0)
	case "exp":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		items, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
			return b.pl.Items(ctx, userID, id)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "exported")
		kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("⬅️ Back", fmt.Sprintf("pl:open:%d", p.ID))},
		}}
		_ = b.api.Edit(chatID, msgID, "<pre>"+esc(playlistExportText(p, items))+"</pre>", kb)
	case "play":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		b.startPlaylistPlay(chatID, userID, msgID, q.ID, id)
	case "enq":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		id, ok := parseID(parts[1])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		b.startPlaylistEnqueue(chatID, userID, msgID, q.ID, id)
	case "enqto":
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad request")
			return
		}
		pid, ok1 := parseID(parts[1])
		if !ok1 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		b.enqueuePlaylistToSession(chatID, userID, msgID, q, pid, parts[2])
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown playlist action")
	}
}

// playlistCallVoid is the error-only variant of playlistCall.
func playlistCallVoid(b *Bot, fn func(ctx context.Context) error) error {
	if b.pl == nil {
		return errPlaylistDisabled
	}
	ctx, cancel := context.WithTimeout(context.Background(), playlistOpTimeout)
	defer cancel()
	return fn(ctx)
}

// playlistCancelKB is used by every playlist text prompt.
func playlistCancelKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", cbCancel)},
		{tgbotapi.NewInlineKeyboardButtonData("📚 Playlists", "pl:home")},
	}}
}

// tryPlaylistInput consumes a text message for a playlist wizard step.
func (b *Bot) tryPlaylistInput(chatID, userID int64, f *flow, text string) bool {
	switch f.step {
	case stepPlaylistName:
		b.finishPlaylistName(chatID, userID, f, text)
		return true
	case stepPlaylistRename:
		b.finishPlaylistRename(chatID, userID, f, text)
		return true
	case stepPlaylistDesc:
		b.finishPlaylistDescription(chatID, userID, f, text)
		return true
	case stepPlaylistAdd:
		b.finishPlaylistAdd(chatID, userID, f, text, false)
		return true
	case stepPlaylistImport:
		b.finishPlaylistAdd(chatID, userID, f, text, true)
		return true
	case stepPlaylistPlayMeet:
		b.finishPlaylistPlayMeet(chatID, userID, f, text)
		return true
	default:
		return false
	}
}

// finishPlaylistName creates a new playlist, or saves a session queue into a
// new playlist when the flow came from "💾 Save queue".
func (b *Bot) finishPlaylistName(chatID, userID int64, f *flow, text string) {
	name, err := store.ValidatePlaylistName(text)
	if err != nil {
		b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend another name:", playlistCancelKB())
		return
	}
	if f.parent == "session" && f.sessionID != "" {
		b.saveSessionQueueAsPlaylist(chatID, userID, f, name)
		return
	}
	p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
		return b.pl.Create(ctx, userID, name, "")
	})
	if err != nil {
		b.sendText(chatID, "❌ "+esc(plErr(err))+"\n\nSend another name:", playlistCancelKB())
		return
	}
	b.clearFlow(userID)
	kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("➕ Add links", fmt.Sprintf("pl:add:%d", p.ID))},
		{tgbotapi.NewInlineKeyboardButtonData("📂 Open playlist", fmt.Sprintf("pl:open:%d", p.ID))},
		{tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome)},
	}}
	b.sendText(chatID, fmt.Sprintf("✅ Playlist <b>%s</b> created.\n\nAdd media links now, or open it from 📚 Playlists at any time.", esc(p.Name)), kb)
}

// saveSessionQueueAsPlaylist copies the live queue into a new playlist.
func (b *Bot) saveSessionQueueAsPlaylist(chatID, userID int64, f *flow, name string) {
	s := b.eng.Get(engine.SessionID(f.sessionID))
	if s == nil {
		b.clearFlow(userID)
		b.sendText(chatID, "This session is gone — its queue cannot be saved.", mainMenuKB())
		return
	}
	items, _ := s.QueueSnapshot()
	if len(items) == 0 {
		b.clearFlow(userID)
		b.sendText(chatID, "This session's queue is empty — nothing to save.", mainMenuKB())
		return
	}
	p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
		return b.pl.Create(ctx, userID, name, "Saved from session "+f.sessionID)
	})
	if err != nil {
		b.sendText(chatID, "❌ "+esc(plErr(err))+"\n\nSend another name:", playlistCancelKB())
		return
	}
	inputs := playlistInputsFromQueue(items)
	added, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
		return b.pl.AddItems(ctx, userID, p.ID, inputs)
	})
	if err != nil {
		// Do not leave an empty playlist behind because one link was
		// rejected by the storage fence.
		_ = playlistCallVoid(b, func(ctx context.Context) error {
			return b.pl.Delete(ctx, userID, p.ID)
		})
		b.sendText(chatID, "❌ Could not save the queue: "+esc(plErr(err))+"\n\nSend another name:", playlistCancelKB())
		return
	}
	b.clearFlow(userID)
	kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("📂 Open playlist", fmt.Sprintf("pl:open:%d", p.ID))},
		{tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome)},
	}}
	b.sendText(chatID, fmt.Sprintf("✅ Saved <b>%d</b> item(s) from session <code>%s</code> into playlist <b>%s</b>.",
		len(added), esc(f.sessionID), esc(name)), kb)
}

// playlistInputsFromQueue converts engine queue items into storage inputs.
func playlistInputsFromQueue(items []engine.QueueItem) []store.PlaylistItemInput {
	inputs := make([]store.PlaylistItemInput, 0, len(items))
	for _, it := range items {
		inputs = append(inputs, store.PlaylistItemInput{
			URL:      it.URL,
			Title:    it.Title,
			Kind:     string(it.Kind),
			Duration: it.Duration,
		})
	}
	return inputs
}

// finishPlaylistRename renames and re-renders the playlist.
func (b *Bot) finishPlaylistRename(chatID, userID int64, f *flow, text string) {
	name, err := store.ValidatePlaylistName(text)
	if err != nil {
		b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend another name:", playlistCancelKB())
		return
	}
	_, err = playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
		return b.pl.Rename(ctx, userID, f.playlistID, name)
	})
	if err != nil {
		b.sendText(chatID, "❌ "+esc(plErr(err)), playlistCancelKB())
		return
	}
	b.clearFlow(userID)
	b.showPlaylist(chatID, userID, 0, f.playlistID, 0)
}

// finishPlaylistDescription saves or clears the description.
func (b *Bot) finishPlaylistDescription(chatID, userID int64, f *flow, text string) {
	desc := strings.TrimSpace(text)
	if desc == "-" {
		desc = ""
	}
	if err := playlistCallVoid(b, func(ctx context.Context) error {
		return b.pl.Describe(ctx, userID, f.playlistID, desc)
	}); err != nil {
		b.sendText(chatID, "❌ "+esc(plErr(err)), playlistCancelKB())
		return
	}
	b.clearFlow(userID)
	b.showPlaylist(chatID, userID, 0, f.playlistID, 0)
}

// finishPlaylistAdd validates and stores a pasted list. Import and "add
// links" share this tail; only the prompt text differs.
func (b *Bot) finishPlaylistAdd(chatID, userID int64, f *flow, text string, _ bool) {
	inputs, invalid := parsePlaylistInputs(text)
	if len(inputs) == 0 {
		msg := "❌ No valid media links found."
		if invalid != "" {
			msg += "\n\n" + esc(truncate(invalid, 120))
		}
		msg += "\n\nSend links like <code>https://youtu.be/…</code> or /cancel."
		b.sendText(chatID, msg, playlistCancelKB())
		return
	}
	if invalid != "" {
		b.sendText(chatID, "❌ <b>Nothing was added</b>\n\nThe list contains an invalid entry:\n<code>"+
			esc(truncate(invalid, 140))+"</code>\n\nFix or remove that line and send the whole list again.", playlistCancelKB())
		return
	}
	added, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
		return b.pl.AddItems(ctx, userID, f.playlistID, inputs)
	})
	if err != nil {
		b.sendText(chatID, "❌ "+esc(plErr(err))+"\n\nSend the links again, or /cancel.", playlistCancelKB())
		return
	}
	b.clearFlow(userID)
	kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("➕ Add more", fmt.Sprintf("pl:add:%d", f.playlistID))},
		{tgbotapi.NewInlineKeyboardButtonData("📂 Open playlist", fmt.Sprintf("pl:open:%d", f.playlistID))},
	}}
	b.sendText(chatID, fmt.Sprintf("✅ Added <b>%d</b> item(s).", len(added)), kb)
	b.showPlaylist(chatID, userID, 0, f.playlistID, 0)
}

// parsePlaylistInputs parses a pasted list into storage inputs.
//
// Accepted forms (one per line):
//
//	https://example.com/a.mp4
//	A nice title | https://example.com/a.mp4
//
// A line may contain several bare URLs, in which case each becomes its own
// item. The first invalid line is returned verbatim so the user can fix it;
// nothing is partially imported.
func parsePlaylistInputs(text string) ([]store.PlaylistItemInput, string) {
	var out []store.PlaylistItemInput
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		title := ""
		urlPart := line
		if idx := strings.Index(line, "|"); idx >= 0 {
			title = strings.TrimSpace(line[:idx])
			urlPart = strings.TrimSpace(line[idx+1:])
		}
		urls := extractURLs(urlPart)
		if len(urls) == 0 {
			return out, line
		}
		for i, u := range urls {
			itemTitle := ""
			if i == 0 {
				itemTitle = title
			}
			if _, err := store.ValidatePlaylistURL(u); err != nil {
				return out, line
			}
			if len(out) >= store.MaxPlaylistItems {
				return out, "playlist item limit reached"
			}
			out = append(out, store.PlaylistItemInput{URL: u, Title: itemTitle})
		}
	}
	return out, ""
}

// startPlaylistPlay opens the "which meeting?" step for a playlist.
func (b *Bot) startPlaylistPlay(chatID, userID int64, msgID int, cbID string, id int64) {
	p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
		return b.pl.Get(ctx, userID, id)
	})
	if err != nil {
		_ = b.api.AnswerCallback(cbID, plErr(err))
		return
	}
	if p.ItemCount == 0 {
		_ = b.api.AnswerCallback(cbID, "playlist is empty")
		return
	}
	items, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
		return b.pl.Items(ctx, userID, id)
	})
	if err != nil || len(items) == 0 {
		_ = b.api.AnswerCallback(cbID, "playlist is empty")
		return
	}
	if active := len(b.eng.Active()); active >= b.cfg.Limits.MaxSessions {
		_ = b.api.AnswerCallback(cbID, "session limit reached")
		return
	}
	b.setFlow(userID, &flow{step: stepPlaylistPlayMeet, playlistID: id, parent: "playlist"})
	_ = b.api.AnswerCallback(cbID, "pick a meeting")
	_ = b.api.Edit(chatID, msgID, playlistPlayMeetText(p), playlistPlayMeetKB(p, b.savedMeetings(userID)))
}

// handlePlaylistMeetCallback handles a saved-meeting button from the playlist
// launch screen.
func (b *Bot) handlePlaylistMeetCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.Split(strings.TrimPrefix(data, cbPlaylistMeet), ":")
	if len(parts) < 2 {
		_ = b.api.AnswerCallback(q.ID, "bad meeting")
		return
	}
	pid, ok := parseID(parts[0])
	if !ok {
		_ = b.api.AnswerCallback(q.ID, "bad playlist")
		return
	}
	mid, ok := parseID(parts[1])
	if !ok {
		_ = b.api.AnswerCallback(q.ID, "bad meeting")
		return
	}
	if b.ms == nil {
		_ = b.api.AnswerCallback(q.ID, "saved meetings unavailable")
		return
	}
	m, err := b.ms.Get(context.Background(), userID, mid)
	if err != nil {
		_ = b.api.AnswerCallback(q.ID, hb(err))
		return
	}
	b.clearFlow(userID)
	_ = b.api.AnswerCallback(q.ID, "launching…")
	b.launchPlaylist(chatID, userID, pid, m.MeetURL)
}

// finishPlaylistPlayMeet validates a typed Meet link and launches the queue.
func (b *Bot) finishPlaylistPlayMeet(chatID, userID int64, f *flow, text string) {
	meet, err := engine.NormalizeMeetURL(text)
	if err != nil {
		b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend a Meet link like <code>meet.google.com/abc-defg-hij</code>, or /cancel.", playlistCancelKB())
		return
	}
	b.clearFlow(userID)
	b.launchPlaylist(chatID, userID, f.playlistID, meet)
}

// launchPlaylist starts a session whose initial queue is the whole playlist.
// The first item becomes the opening media; the rest ride along as upcoming
// items so the operator gets "next" controls immediately.
func (b *Bot) launchPlaylist(chatID, userID, playlistID int64, meetURL string) {
	if b.pl == nil {
		b.sendText(chatID, "📚 The playlist library is unavailable.", mainMenuKB())
		return
	}
	if active := len(b.eng.Active()); active >= b.cfg.Limits.MaxSessions {
		b.sendText(chatID, fmt.Sprintf("⚠️ <b>Limit reached</b> (%d/%d). Stop a session first.", active, b.cfg.Limits.MaxSessions), sessionsKB(b.statuses()))
		return
	}
	p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
		return b.pl.Get(ctx, userID, playlistID)
	})
	if err != nil {
		b.sendText(chatID, "🔴 "+esc(plErr(err)), mainMenuKB())
		return
	}
	items, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
		return b.pl.Items(ctx, userID, playlistID)
	})
	if err != nil || len(items) == 0 {
		b.sendText(chatID, "This playlist is empty — add media first.", mainMenuKB())
		return
	}
	urls := make([]string, 0, len(items))
	for _, it := range items {
		urls = append(urls, it.URL)
	}
	// Local files from an expired download cannot be replayed. Skip them
	// up front and say so instead of failing the whole launch.
	urls = b.filterPlayableURLs(chatID, urls)
	if len(urls) == 0 {
		b.sendText(chatID, "🔴 None of this playlist's media is still reachable. Local downloads expire; add fresh links.", mainMenuKB())
		return
	}
	if len(urls) < len(items) {
		b.sendText(chatID, fmt.Sprintf("ℹ️ %d item(s) looked like expired local files and were skipped.", len(items)-len(urls)), nil)
	}
	req := b.startRequestFor(userID, meetURL, engine.StartRequest{
		MeetLink:  meetURL,
		MediaLink: urls[0],
	})
	if len(urls) > 1 {
		req.Queue = urls[1:]
	}
	if guestName, source := b.effectiveGuestName(userID); guestName != "" {
		req.GuestName = guestName
		b.log.Infof("[PIPELINE:workflow] playlist guest name from %s for user=%d", source, userID)
	}
	sess, err := b.eng.Start(context.Background(), req)
	if err != nil {
		b.sendText(chatID, "❌ "+esc(err.Error()), mainMenuKB())
		return
	}
	var qRecs []store.QueueItemRecord
	items2, curIdx := sess.QueueSnapshot()
	for _, it := range items2 {
		qRecs = append(qRecs, store.QueueItemRecord{ID: it.ID, URL: it.URL, Title: it.Title})
	}
	b.st.Add(store.Record{
		ID:        string(sess.ID),
		MeetURL:   sess.MeetURL,
		MediaURL:  sess.MediaURL,
		Title:     sess.TabTitle,
		State:     engine.StateStarting,
		ChatID:    chatID,
		CreatedAt: time.Now(),
		Queue:     qRecs,
		QueueIdx:  curIdx,
	})
	b.mu.Lock()
	b.sessionChats[string(sess.ID)] = chatID
	b.trackLive(chatID, true, string(sess.ID))
	b.mu.Unlock()
	b.persistQueue(sess)
	b.saveCurrentMeet(userID, sess.MeetURL)
	b.sendText(chatID, fmt.Sprintf("▶️ Launching <b>%s</b> with <b>%d</b> item(s).", esc(p.Name), len(urls)), nil)
	b.sendSessionCard(chatID, sess)
}

// filterPlayableURLs drops local media URLs whose download entry is gone.
// Remote links are always kept; the engine will report a dead remote link in
// the session card, which is the right place for a transient network error.
func (b *Bot) filterPlayableURLs(chatID int64, urls []string) []string {
	mgr := b.mediaMgr()
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if engine.IsLocalMediaURL(u) {
			if mgr == nil {
				continue
			}
			if _, ok := mgr.FindByURL(u); !ok {
				continue
			}
		}
		out = append(out, u)
	}
	_ = chatID
	return out
}

// startPlaylistEnqueue shows the live-session picker.
func (b *Bot) startPlaylistEnqueue(chatID, userID int64, msgID int, cbID string, id int64) {
	p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
		return b.pl.Get(ctx, userID, id)
	})
	if err != nil {
		_ = b.api.AnswerCallback(cbID, plErr(err))
		return
	}
	if p.ItemCount == 0 {
		_ = b.api.AnswerCallback(cbID, "playlist is empty")
		return
	}
	list := b.statuses()
	_ = b.api.AnswerCallback(cbID, "")
	_ = b.api.Edit(chatID, msgID, playlistSessionPickText(p, list), playlistSessionPickKB(p, list))
}

// enqueuePlaylistToSession appends a playlist's links to a running queue.
func (b *Bot) enqueuePlaylistToSession(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, playlistID int64, sessionID string) {
	s := b.eng.Get(engine.SessionID(sessionID))
	if s == nil {
		_ = b.api.AnswerCallback(q.ID, "session not found")
		_ = b.api.Edit(chatID, msgID, "This session has ended.", mainMenuKB())
		return
	}
	items, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
		return b.pl.Items(ctx, userID, playlistID)
	})
	if err != nil {
		_ = b.api.AnswerCallback(q.ID, plErr(err))
		return
	}
	if len(items) == 0 {
		_ = b.api.AnswerCallback(q.ID, "playlist is empty")
		return
	}
	urls := make([]string, 0, len(items))
	for _, it := range items {
		urls = append(urls, it.URL)
	}
	urls = b.filterPlayableURLs(chatID, urls)
	if len(urls) == 0 {
		_ = b.api.AnswerCallback(q.ID, "no playable links left")
		return
	}
	added, err := s.Enqueue(urls...)
	if err != nil {
		_ = b.api.AnswerCallback(q.ID, truncate(err.Error(), 60))
		return
	}
	b.persistQueue(s)
	st := s.Snapshot()
	_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("added %d items", len(added)))
	_ = b.api.Edit(chatID, msgID, fmt.Sprintf("✅ Added <b>%d</b> playlist item(s) to session <code>%s</code>.\n\nQueue now has <b>%d</b> item(s).",
		len(added), esc(sessionID), st.QueueLength), sessionCardKB(st))
	b.bindCard(chatID, msgID, s, st)
}

// queueCommand implements /queue [session-id]. With no argument it opens the
// queue of the first active session; without any live session it shows the
// user's most recent interrupted queue, when one was persisted.
func (b *Bot) queueCommand(chatID, userID int64, arg string) {
	var s *engine.Session
	arg = strings.TrimSpace(arg)
	if arg != "" {
		s = b.eng.Get(engine.SessionID(arg))
	} else if list := b.eng.Active(); len(list) > 0 {
		s = list[0]
	}
	if s != nil {
		st := s.Snapshot()
		items, curIdx := s.QueueSnapshot()
		page := 0
		if curIdx > 0 {
			page = curIdx / queuePageSize
		}
		b.sendText(chatID, queueViewText(st, items, curIdx, page), queueViewKB(st, items, curIdx, page))
		return
	}
	// No live session: surface the interrupted queue metadata, if any.
	if b.set != nil {
		ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
		snap, found, err := b.set.LatestInterruptedQueue(ctx, userID)
		cancel()
		if err == nil && found && len(snap.Items) > 0 {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("⚠️ <b>Interrupted queue</b> (session <code>%s</code>)\n\n", esc(snap.SessionID)))
			sb.WriteString("The bot restarted while this session was live; playback cannot be recovered, but this was the queue:\n\n")
			pos := snap.Cursor + 1
			if pos < 1 {
				pos = 1
			}
			sb.WriteString(fmt.Sprintf("▶️ Was at position <b>%d/%d</b>\n\n", pos, len(snap.Items)))
			start := snap.Cursor
			if start < 0 {
				start = 0
			}
			end := start + 10
			if end > len(snap.Items) {
				end = len(snap.Items)
			}
			for i := start; i < end; i++ {
				it := snap.Items[i]
				title := it.Title
				if title == "" {
					title = it.URL
				}
				sb.WriteString(fmt.Sprintf("%d. <code>%s</code>\n", i+1, esc(truncate(title, 50))))
			}
			b.sendText(chatID, sb.String(), mainMenuKB())
			return
		}
	}
	b.sendText(chatID, "No active session to show a queue for. Use /sessions.", mainMenuKB())
}

// saveQueueStart starts the "save queue as playlist" wizard for a session.
func (b *Bot) saveQueueStart(chatID, userID int64, sessionID string) {
	if b.pl == nil {
		b.sendText(chatID, "📚 The playlist library is unavailable.", mainMenuKB())
		return
	}
	s := b.eng.Get(engine.SessionID(sessionID))
	if s == nil {
		b.sendText(chatID, "This session is gone — its queue cannot be saved.", mainMenuKB())
		return
	}
	items, _ := s.QueueSnapshot()
	if len(items) == 0 {
		b.sendText(chatID, "This session's queue is empty — nothing to save.", mainMenuKB())
		return
	}
	b.setFlow(userID, &flow{step: stepPlaylistName, parent: "session", sessionID: sessionID})
	b.sendText(chatID, playlistNameText("session", ""), playlistCancelKB())
}

// playlistQueueToolsText renders the advanced queue tools screen.
func playlistQueueToolsText(st engine.Status, items []engine.QueueItem, curIdx int) string {
	var sb strings.Builder
	sb.WriteString("🎛 <b>Queue tools</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code>\n", esc(string(st.ID))))
	sb.WriteString(fmt.Sprintf("Items: <b>%d</b> · current: <b>%d</b>\n\n", len(items), curIdx+1))
	if len(items) == 0 {
		sb.WriteString("The queue is empty. Send a link to add media.")
		return sb.String()
	}
	sb.WriteString("• <b>Repeat / Shuffle</b> change how playback continues.\n")
	sb.WriteString("• <b>Reorder</b> moves one item up or down.\n")
	sb.WriteString("• <b>Load playlist</b> replaces the upcoming items.\n")
	sb.WriteString("• <b>Save</b> copies the whole queue into a persistent playlist.\n\n")
	sb.WriteString("<i>Playback is not interrupted by reordering.</i>")
	return sb.String()
}

// playlistQueueToolsKB renders the tools keyboard.
func playlistQueueToolsKB(st engine.Status) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	repeatLabel := "🔁 Repeat: off"
	switch st.RepeatMode {
	case engine.RepeatOne:
		repeatLabel = "🔁 Repeat: one 🔂"
	case engine.RepeatAll:
		repeatLabel = "🔁 Repeat: all 🔁"
	}
	shuffleLabel := "🔀 Shuffle: off"
	if st.Shuffle {
		shuffleLabel = "🔀 Shuffle: on ✅"
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData(repeatLabel, "q:repeat:"+id),
			tgbotapi.NewInlineKeyboardButtonData(shuffleLabel, "q:shuftoggle:"+id),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("↕️ Reorder items", "q:move_menu:"+id),
			tgbotapi.NewInlineKeyboardButtonData("💾 Save as playlist", "q:save:"+id),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("📚 Load playlist", "q:load:"+id),
			tgbotapi.NewInlineKeyboardButtonData("🗑 Clear options", "q:clear_confirm:"+id),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("📋 View queue", "act:"+id+":queue"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Session", "ses:"+id),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
		},
	}
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}
