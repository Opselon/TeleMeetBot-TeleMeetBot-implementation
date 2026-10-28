// Package bot implements the Telegram layer: a live dashboard with inline
// keyboards, a guided new-stream flow, and per-session control cards.
package bot

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/googleauth"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// Logger is the logging surface of the bot layer.
type Logger interface {
	Debugf(format string, args ...interface{})
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

type noopLogger struct{}

func (noopLogger) Debugf(string, ...interface{}) {}
func (noopLogger) Infof(string, ...interface{})  {}
func (noopLogger) Warnf(string, ...interface{})  {}
func (noopLogger) Errorf(string, ...interface{}) {}

// cardRef points at a dashboard message that mirrors a session.
type cardRef struct {
	chat     int64
	msg      int
	text     string
	fixedAt  time.Time
	lastEdit time.Time
	errs     int
	view     string
	viewAt   time.Time
}

// Engine is the engine surface the bot needs (Manager implements it; tests fake it).
type Engine interface {
	Start(ctx context.Context, req engine.StartRequest) (*engine.Session, error)
	Get(id engine.SessionID) *engine.Session
	Active() []*engine.Session
	List() []*engine.Session
	Events() <-chan engine.Event
	// MediaManager returns the Download & Stream manager, or nil when the
	// feature is disabled. The bot uses it to register Telegram-ingested
	// files so they follow the same directory, TTL and cleanup as downloads.
	MediaManager() *media.Manager
}

// Meetings is the saved-meetings surface the bot needs (store.MeetingStore
// implements it; tests fake it). A nil meeting store is valid: the /new flow
// simply omits the saved-links picker.
type Meetings interface {
	Save(ctx context.Context, ownerID int64, name, meetURL string) (store.Meeting, error)
	List(ctx context.Context, ownerID int64) ([]store.Meeting, error)
	Get(ctx context.Context, ownerID, id int64) (store.Meeting, error)
	Rename(ctx context.Context, ownerID, id int64, name string) (store.Meeting, error)
	Delete(ctx context.Context, ownerID, id int64) error
}

// Bot is the Telegram front end.
type Bot struct {
	api     API
	cfg     *config.Config
	eng     Engine
	st      *store.Store
	ms      Meetings
	log     Logger
	cfgPath string

	browserPath string
	browserErr  string
	started     time.Time

	mu           sync.Mutex
	flows        map[int64]*flow
	cards        map[string]*cardRef
	sessionChats map[string]int64
	// dashboardMsg per chat: reused for menu edits (home/sessions/...).
	menus map[int64]int
	// mgr is a test seam: a real media manager the bot uses instead of the
	// engine's when set. Production leaves it nil so mediaMgr() resolves
	// through the engine, keeping one owner for the manager.
	mgr *media.Manager
	// pending holds staged Telegram files per user (audio/video forwarded
	// before a wizard was open). Staged files are leased so the janitor
	// cannot reclaim them while the user decides what to do with them.
	pending map[int64][]*pendingMedia
	// pl is the persistent playlist library. Nil means the feature is
	// unavailable (an old data directory or a store that failed to open);
	// the dashboard then hides the playlist entry point.
	pl Playlists
	// set is the persistent per-user settings store (SQLite). Nil means the
	// preferences stay in the legacy JSON state file: the bot falls back to
	// the old accessor methods so an upgrade never loses the feature.
	set Settings
	// gauth manages per-user Google Account profile cookies.
	gauth *googleauth.Manager
}

// Option customizes a Bot after the required dependencies are wired.
type Option func(*Bot)

// WithPlaylists attaches the persistent playlist library.
func WithPlaylists(pl Playlists) Option {
	return func(b *Bot) { b.pl = pl }
}

// WithSettings attaches the persistent per-user settings store.
func WithSettings(st Settings) Option {
	return func(b *Bot) { b.set = st }
}

// WithGoogleAuth attaches the per-user Google Account profile manager.
func WithGoogleAuth(gauth *googleauth.Manager) Option {
	return func(b *Bot) { b.gauth = gauth }
}

// New wires a Telegram bot. (api can be a fake in tests.)
func New(api API, cfg *config.Config, eng Engine, st *store.Store, ms Meetings, log Logger, cfgPath, browserPath, browserErr string, opts ...Option) *Bot {
	if log == nil {
		log = noopLogger{}
	}
	b := &Bot{
		api:          api,
		cfg:          cfg,
		eng:          eng,
		st:           st,
		ms:           ms,
		log:          log,
		cfgPath:      cfgPath,
		browserPath:  browserPath,
		browserErr:   browserErr,
		started:      time.Now(),
		flows:        map[int64]*flow{},
		cards:        map[string]*cardRef{},
		sessionChats: map[string]int64{},
		menus:        map[int64]int{},
		pending:      map[int64][]*pendingMedia{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	if cfg != nil {
		health.SetResourceDataDir(cfg.Paths.DataDir)
	}
	return b
}

// Run consumes Telegram updates until the context is cancelled.
// ctx is used to keep handling sane on shutdown: in-flight updates finish.
func (b *Bot) Run(ctx context.Context) error {
	if b.cfg != nil {
		health.StartResourceMonitor(ctx, b.cfg.Paths.DataDir)
	}
	updates := b.api.Updates(b.cfg.Telegram.PollTimeoutSeconds)
	b.log.Infof("telegram bot @%s is polling", b.api.BotUsername())
	for {
		select {
		case <-ctx.Done():
			return nil
		case upd, ok := <-updates:
			if !ok {
				return errors.New("telegram update channel closed")
			}
			// Handle inline, but never forever: sessions can take minutes.
			go b.handleUpdate(ctx, &upd)
		}
	}
}

// RunEvents turns engine lifecycle events into chat notifications.
func (b *Bot) RunEvents(ctx context.Context) {
	// One panic anywhere on this loop would otherwise kill event delivery
	// forever: the operator sees a frozen dashboard and reads it as an app
	// crash. Recover, log with a stack digest, and keep serving.
	defer func() {
		if r := recover(); r != nil {
			b.log.Errorf("[PIPELINE:bot] panic in RunEvents (recovered, loop continues): %v", r)
			go b.RunEvents(ctx)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-b.eng.Events():
			if !ok {
				return
			}
			b.handleEvent(ev)
		}
	}
}

// refreshLoop keeps session cards up to date.
func (b *Bot) refreshLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A panic during one refresh tick (a session snapshot taken
			// mid-teardown, a card rendered from stale state) must not kill
			// the loop — the dashboard would freeze forever and the operator
			// reads that as an app crash. Log and carry on with the next tick.
			func() {
				defer func() {
					if r := recover(); r != nil {
						b.log.Errorf("[PIPELINE:bot] panic in refresh tick (recovered, loop continues): %v", r)
					}
				}()
				b.refreshAll()
			}()
		}
	}
}

// refreshAll re-renders active session cards.
func (b *Bot) refreshAll() {
	for _, s := range b.eng.List() {
		st := s.Snapshot()
		b.mu.Lock()
		ref := b.cards[string(st.ID)]
		if ref != nil && ref.view != "" && ref.view != "card" {
			// Subview active (e.g. speed, quality, captions, tools, queue):
			// pause periodic re-rendering so user interaction is not interrupted.
			// Revert to card view after 2 minutes of idle inactivity.
			if time.Since(ref.viewAt) < 2*time.Minute {
				b.mu.Unlock()
				continue
			}
			b.log.Warnf("[PIPELINE:bot] subview %q timed out after inactivity session=%s, reverting to card", ref.view, st.ID)
			ref.view = "card"
		}
		b.mu.Unlock()
		if ref == nil {
			continue
		}
		if engine.IsTerminalState(st.State) {
			// Final card is already sent by handleEvent; stop touching it.
			continue
		}
		text := sessionCardText(st)
		kb := sessionCardKB(st)
		b.refreshCard(ref, st.ID, text, kb)
	}
	// Expire stale flows.
	b.expireFlows()
}

// minCardEditInterval is the floor between two edits of the same card.
// Telegram allows roughly one edit per second per chat; without a floor a
// chatty state machine turns into an edit storm (and 429 retries) that burns CPU
// on TLS handshakes rather than on streaming.
const minCardEditInterval = 1200 * time.Millisecond

func (b *Bot) refreshCard(ref *cardRef, id engine.SessionID, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	b.mu.Lock()
	if ref.text == text {
		b.mu.Unlock()
		return
	}
	if !ref.lastEdit.IsZero() && time.Since(ref.lastEdit) < minCardEditInterval {
		// Leave ref.text stale so the next tick retries instead of dropping
		// the update for good.
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	if err := b.api.Edit(ref.chat, ref.msg, text, kb); err != nil {
		b.mu.Lock()
		ref.errs++
		errs := ref.errs
		b.mu.Unlock()
		if errs <= 2 {
			b.log.Warnf("[PIPELINE:bot] edit card %s failed (chat=%d msg=%d): %v", id, ref.chat, ref.msg, err)
		}
		// Do not record the text: a transient Telegram failure must not leave
		// the card permanently stale.
		return
	}
	b.mu.Lock()
	ref.text = text
	ref.errs = 0
	ref.lastEdit = time.Now()
	b.mu.Unlock()
}

// handleEvent reacts to engine events.
func (b *Bot) handleEvent(ev engine.Event) {
	st := engine.Status{ID: ev.Session, State: string(ev.Kind), Detail: ev.Text}
	if s := b.eng.Get(ev.Session); s != nil {
		st = s.Snapshot()
	}
	switch ev.Kind {
	case engine.EventError:
		text := fmt.Sprintf("🔴 <b>Session %s failed</b>\n\n%s", ev.Session, esc(truncate(ev.Text, 350)))
		b.notifySession(ev.Session, text, true)
	case engine.EventEnded:
		if s := b.eng.Get(ev.Session); s != nil {
			acts := s.Snapshot()
			b.notifySession(ev.Session, sessionCardText(acts), false)
			b.st.Update(string(acts.ID), acts.State, acts.Error)
			b.mu.Lock()
			chat := b.sessionChats[string(acts.ID)]
			delete(b.cards, string(acts.ID))
			delete(b.sessionChats, string(acts.ID))
			b.mu.Unlock()
			// The session finished cleanly: its durable queue metadata is no
			// longer needed (the history record keeps the final state), so
			// remove it instead of leaving an "interrupted" ghost.
			b.deletePersistedQueue(chat, string(acts.ID))
		} else {
			b.st.Update(string(ev.Session), "ended", "")
		}
	default:
		// progress events are rendered by the refresh loop
		_ = st
		if s := b.eng.Get(ev.Session); s != nil {
			acts := s.Snapshot()
			b.st.Update(string(acts.ID), acts.State, "")
		}
	}
}

// notifySession sends (or edits) a card on the owner chat.
func (b *Bot) notifySession(id engine.SessionID, text string, final bool) {
	b.mu.Lock()
	chat := b.sessionChats[string(id)]
	ref := b.cards[string(id)]
	b.mu.Unlock()
	if final {
		if ref != nil {
			_ = b.api.Edit(ref.chat, ref.msg, text, sessionCardKB(engine.Status{ID: id, State: "error"}))
			return
		}
	}
	if chat == 0 {
		return
	}
	_, err := b.api.Send(chat, text, nil)
	if err != nil {
		b.log.Warnf("notify session %s: %v", id, err)
	}
}

// Snapshot returns a health snapshot for the web endpoint.
func (b *Bot) Snapshot() health.Snapshot {
	active := len(b.eng.Active())
	dataDir := b.cfg.Paths.DataDir
	freeGB, totalGB := health.DiskUsage(dataDir)
	snap := health.Snapshot{
		Version:     config.Version,
		GoVersion:   runtime.Version(),
		Uptime:      time.Since(b.started),
		StartedAt:   b.started,
		ActiveCount: active,
		MaxSessions: b.cfg.Limits.MaxSessions,
		BrowserPath: b.browserPath,
		ConfigPath:  b.cfgPath,
		DataDir:     dataDir,
		DiskFreeGB:  freeGB,
		DiskTotalGB: totalGB,
	}
	return snap
}

// queueOwnerChat returns the chat a session belongs to (private chats share
// the user id). It prefers the live binding, then the history record.
func (b *Bot) queueOwnerChat(id engine.SessionID) int64 {
	b.mu.Lock()
	chat := b.sessionChats[string(id)]
	b.mu.Unlock()
	if chat != 0 {
		return chat
	}
	if rec := b.st.GetRecord(string(id)); rec != nil {
		return rec.ChatID
	}
	return 0
}

// deletePersistedQueue removes a finished session's durable queue metadata.
// The owner chat is resolved best-effort so cleanup still works after the
// in-memory binding is gone.
func (b *Bot) deletePersistedQueue(chat int64, sessionID string) {
	if b.set == nil {
		return
	}
	if chat == 0 {
		chat = b.queueOwnerChat(engine.SessionID(sessionID))
	}
	if chat == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
	defer cancel()
	if err := b.set.DeleteSessionQueue(ctx, chat, sessionID); err != nil {
		b.log.Warnf("[PIPELINE:queue] delete persisted queue %s: %v", sessionID, err)
	}
}

// BrowserProbe returns the visible browser name for cards (or error text).
func (b *Bot) BrowserProbe() (string, string) { return b.browserPath, b.browserErr }

// RefreshLoop keeps session cards live (wired by main).
func (b *Bot) RefreshLoop(ctx context.Context) { b.refreshLoop(ctx, 3*time.Second) }

// Broadcast sends a message to every admin chat (private chats share the user id).
func (b *Bot) Broadcast(text string) {
	for _, id := range b.cfg.AdminIDs() {
		if _, err := b.api.Send(id, text, nil); err != nil {
			b.log.Warnf("broadcast to %d: %v", id, err)
		}
	}
}

// handleUpdate dispatches a Telegram update.
func (b *Bot) handleUpdate(ctx context.Context, upd *tgbotapi.Update) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Errorf("panic handling update: %v", r)
		}
	}()
	switch {
	case upd.Message != nil && upd.Message.Chat != nil:
		b.handleMessage(upd.Message)
	case upd.CallbackQuery != nil:
		b.handleCallback(ctx, upd.CallbackQuery)
	default:
	}
}

// fromID extracts the acting user id from a message.
func (b *Bot) fromID(m *tgbotapi.Message) int64 {
	if m.From != nil {
		return m.From.ID
	}
	return 0
}
