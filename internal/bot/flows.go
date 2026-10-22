package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// Flow steps for the guided new-stream wizard.
const (
	stepNone = iota
	stepMeet
	stepMedia
)

// flowTimeout drops half-finished wizards after inactivity.
const flowTimeout = 15 * time.Minute

type flow struct {
	step    int
	meet    string
	media   string
	updated time.Time
}

// getFlow returns the user's pending wizard.
func (b *Bot) getFlow(userID int64) *flow {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flows[userID]
}

// setFlow stores the user's wizard state.
func (b *Bot) setFlow(userID int64, f *flow) {
	f.updated = time.Now()
	b.mu.Lock()
	b.flows[userID] = f
	b.mu.Unlock()
}

// clearFlow drops the wizard state.
func (b *Bot) clearFlow(userID int64) {
	b.mu.Lock()
	delete(b.flows, userID)
	b.mu.Unlock()
}

// expireFlows removes stale wizards.
func (b *Bot) expireFlows() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for uid, f := range b.flows {
		if time.Since(f.updated) > flowTimeout {
			delete(b.flows, uid)
		}
	}
}

// authorizeUser enforces the admin allow-list (fail-closed) and implements the
// "first /start claims the bot" onboarding.
func (b *Bot) authorizeUser(userID, chatID int64) bool {
	if b.cfg.IsAdmin(userID) {
		return true
	}
	if b.cfg.ClaimAdmin(userID) {
		if err := b.cfg.Save(b.cfgPath); err != nil {
			b.log.Errorf("cannot persist admin claim: %v", err)
		}
		b.log.Infof("admin claimed by %d", userID)
		_, _ = b.api.Send(chatID, fmt.Sprintf("🔐 <b>You are now the admin</b>\n\nYour Telegram ID <code>%d</code> was added to <code>telegram.admins</code>.\nFrom now on only allowed IDs can control this bot.", userID), mainMenuKB())
		return true
	}
	_, _ = b.api.Send(chatID, fmt.Sprintf("⛔️ <b>Access denied</b>\n\nYour Telegram ID: <code>%d</code>\nAsk the operator to add it to <code>telegram.admins</code> in the config.", userID), nil)
	return false
}

// handleMessage routes text messages and commands.
func (b *Bot) handleMessage(m *tgbotapi.Message) {
	userID := b.fromID(m)
	if userID == 0 || m.Chat == nil {
		return
	}
	chatID := m.Chat.ID
	text := strings.TrimSpace(m.Text)
	// Auth first: no commands, no flow input for strangers.
	if !b.authorizeUser(userID, chatID) {
		return
	}

	if m.IsCommand() {
		switch m.Command() {
		case "start":
			b.clearFlow(userID)
			b.sendMain(chatID)
		case "new", "stream":
			b.startFlow(chatID, userID, m.CommandArguments())
		case "sessions":
			b.sendSessions(chatID)
		case "health":
			b.sendHealth(chatID)
		case "settings":
			b.sendText(chatID, settingsText(b.cfg), backKB())
		case "help":
			b.sendText(chatID, helpText(), backKB())
		case "cancel":
			b.clearFlow(userID)
			b.sendText(chatID, "✖️ Cancelled. Nothing pending.", mainMenuKB())
		case "stop":
			b.stopCommand(chatID, m.CommandArguments())
		case "id":
			b.sendText(chatID, fmt.Sprintf("Your Telegram ID: <code>%d</code>", userID), nil)
		case "ping":
			b.sendText(chatID, "🏓 pong — "+esc(health.FormatUptime(time.Since(b.started))), nil)
		default:
			b.sendText(chatID, "Unknown command. Use the dashboard buttons.", mainMenuKB())
		}
		return
	}

	// Free text belongs to the wizard when one is open.
	f := b.getFlow(userID)
	if f == nil || f.step == stepNone {
		b.sendText(chatID, "Use /new (or ➕ New stream) to start a session.", mainMenuKB())
		return
	}
	b.stepInput(chatID, userID, f, text)
}

// stepInput handles a wizard answer.
func (b *Bot) stepInput(chatID, userID int64, f *flow, text string) {
	switch f.step {
	case stepMeet:
		normalized, err := engine.NormalizeMeetURL(text)
		if err != nil {
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend a Meet link like <code>meet.google.com/abc-defg-hij</code>.", cancelKB())
			return
		}
		f.meet = normalized
		f.step = stepMedia
		b.setFlow(userID, f)
		b.sendText(chatID, flowText(stepMedia, f.meet), cancelKB())
	case stepMedia:
		target, err := engine.ClassifyMedia(text)
		if err != nil {
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend an http(s) media link.", cancelKB())
			return
		}
		f.media = target.URL
		f.step = stepNone
		b.setFlow(userID, f)
		b.sendText(chatID, confirmText(f.meet, f.media, target.Kind), confirmKB())
	default:
		b.clearFlow(userID)
		b.sendText(chatID, "Wizard restarted. Send a Meet link, or /cancel.", mainMenuKB())
	}
}

// startFlow begins the wizard. args optionally carries "/new <meet> [media]"
// so a session can be opened in a single message.
func (b *Bot) startFlow(chatID, userID int64, args string) {
	active := len(b.eng.Active())
	if active >= b.cfg.Limits.MaxSessions {
		b.sendText(chatID, fmt.Sprintf("⚠️ <b>Limit reached</b>\n\nActive sessions: %d/%d. Stop one first.", active, b.cfg.Limits.MaxSessions), sessionsKB(b.statuses()))
		return
	}
	f := &flow{step: stepMeet}
	fields := strings.Fields(args)
	if len(fields) >= 1 {
		normalized, err := engine.NormalizeMeetURL(fields[0])
		if err != nil {
			b.setFlow(userID, f)
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\n"+flowText(stepMeet, ""), cancelKB())
			return
		}
		f.meet = normalized
		f.step = stepMedia
	}
	if len(fields) >= 2 {
		target, err := engine.ClassifyMedia(fields[1])
		if err != nil {
			b.setFlow(userID, f)
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\n"+flowText(stepMedia, f.meet), cancelKB())
			return
		}
		f.media = target.URL
		f.step = stepNone
		b.setFlow(userID, f)
		b.sendText(chatID, confirmText(f.meet, f.media, target.Kind), confirmKB())
		return
	}
	b.setFlow(userID, f)
	b.sendText(chatID, flowText(f.step, f.meet), cancelKB())
}

// launch starts the engine for the pending wizard.
func (b *Bot) launch(chatID, userID int64) {
	f := b.getFlow(userID)
	if f == nil || strings.TrimSpace(f.meet) == "" || strings.TrimSpace(f.media) == "" {
		b.sendText(chatID, "Nothing pending. Start again with ➕ New stream.", mainMenuKB())
		return
	}
	if active := len(b.eng.Active()); active >= b.cfg.Limits.MaxSessions {
		b.sendText(chatID, fmt.Sprintf("⚠️ <b>Limit reached</b> (%d/%d). Stop a session first.", active, b.cfg.Limits.MaxSessions), sessionsKB(b.statuses()))
		return
	}
	sess, err := b.eng.Start(context.Background(), engine.StartRequest{
		MeetLink:  f.meet,
		MediaLink: f.media,
	})
	if err != nil {
		b.sendText(chatID, "❌ "+esc(err.Error()), mainMenuKB())
		return
	}
	b.clearFlow(userID)
	b.st.Add(store.Record{
		ID:        string(sess.ID),
		MeetURL:   sess.MeetURL,
		MediaURL:  sess.MediaURL,
		Title:     sess.TabTitle,
		State:     engine.StateStarting,
		ChatID:    chatID,
		CreatedAt: time.Now(),
	})
	b.mu.Lock()
	b.sessionChats[string(sess.ID)] = chatID
	b.mu.Unlock()
	b.sendSessionCard(chatID, sess)
}

// handleCallback routes inline keyboard presses.
func (b *Bot) handleCallback(ctx context.Context, q *tgbotapi.CallbackQuery) {
	if q == nil || q.Message == nil || q.Message.Chat == nil || q.From == nil {
		return
	}
	_ = ctx
	userID := q.From.ID
	chatID := q.Message.Chat.ID
	msgID := q.Message.MessageID
	if !b.authorizeUser(userID, chatID) {
		return
	}
	data := q.Data
	switch {
	case data == cbHome || data == cbRefresh:
		_ = b.api.AnswerCallback(q.ID, "dashboard")
		_ = b.api.Edit(chatID, msgID, b.mainText(), mainMenuKB())
	case data == cbNew:
		_ = b.api.AnswerCallback(q.ID, "new stream")
		b.startFlow(chatID, userID, "")
	case data == cbSessions:
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, sessionsText(b.statuses()), sessionsKB(b.statuses()))
	case data == cbHealth:
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, b.healthText(), backKB())
	case data == cbSettings:
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, settingsText(b.cfg), backKB())
	case data == cbHelp:
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, helpText(), backKB())
	case data == cbLaunch:
		_ = b.api.AnswerCallback(q.ID, "launching…")
		b.launch(chatID, userID)
	case data == cbCancel:
		b.clearFlow(userID)
		_ = b.api.AnswerCallback(q.ID, "cancelled")
		_ = b.api.Edit(chatID, msgID, "✖️ Cancelled.", mainMenuKB())
	case data == cbAgain:
		b.setFlow(userID, &flow{step: stepMeet})
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, flowText(stepMeet, ""), cancelKB())
	case strings.HasPrefix(data, "ses:"):
		id := strings.TrimPrefix(data, "ses:")
		s := b.eng.Get(engine.SessionID(id))
		if s == nil {
			_ = b.api.AnswerCallback(q.ID, "session not found")
			return
		}
		_ = b.api.AnswerCallback(q.ID, "")
		st := s.Snapshot()
		_ = b.api.Edit(chatID, msgID, sessionCardText(st), sessionCardKB(st))
		b.bindCard(chatID, msgID, s, st)
	case strings.HasPrefix(data, "act:"):
		b.handleAction(chatID, msgID, q, data)
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown action")
	}
}

// handleAction performs a session control press.
func (b *Bot) handleAction(chatID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.SplitN(strings.TrimPrefix(data, "act:"), ":", 2)
	if len(parts) != 2 {
		_ = b.api.AnswerCallback(q.ID, "bad action")
		return
	}
	id := engine.SessionID(parts[0])
	action := parts[1]
	s := b.eng.Get(id)
	if s == nil {
		_ = b.api.AnswerCallback(q.ID, "session not found")
		_ = b.api.Edit(chatID, msgID, "This session is gone.", mainMenuKB())
		return
	}
	st := s.Snapshot()
	var err error
	toast := ""
	switch action {
	case "pause":
		err, toast = s.Pause(), "paused"
	case "resume":
		err, toast = s.Resume(), "playing"
	case "mute":
		err, toast = s.SetMuted(true), "muted"
	case "unmute":
		err, toast = s.SetMuted(false), "unmuted"
	case "voldown":
		err, toast = s.SetVolume(st.Volume-10), fmt.Sprintf("volume %d%%", max(0, st.Volume-10))
	case "volup":
		err, toast = s.SetVolume(st.Volume+10), fmt.Sprintf("volume %d%%", min(100, st.Volume+10))
	case "back10":
		err, toast = s.Seek(-b.cfg.Media.SeekStepSeconds), "rewound"
	case "fwd10":
		err, toast = s.Seek(b.cfg.Media.SeekStepSeconds), "skipped"
	case "restart":
		err, toast = s.RestartMedia(), "restarting"
	case "refresh":
		toast = "refreshed"
	case "stop":
		s.Stop()
		toast = "stopping"
		err = nil
		s = nil
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown action")
		return
	}
	if err != nil {
		_ = b.api.AnswerCallback(q.ID, "error: "+truncate(err.Error(), 60))
	} else {
		_ = b.api.AnswerCallback(q.ID, toast)
	}
	if s == nil {
		_ = b.api.Edit(chatID, msgID, "⏹ Session stopped.", mainMenuKB())
		return
	}
	st = s.Snapshot()
	_ = b.api.Edit(chatID, msgID, sessionCardText(st), sessionCardKB(st))
	b.bindCard(chatID, msgID, s, st)
}

// bindCard ties a dashboard message to a session for live updates.
func (b *Bot) bindCard(chatID int64, msgID int, s *engine.Session, st engine.Status) {
	b.mu.Lock()
	b.cards[string(st.ID)] = &cardRef{chat: chatID, msg: msgID, text: sessionCardText(st), fixedAt: time.Now()}
	b.sessionChats[string(st.ID)] = chatID
	b.mu.Unlock()
}

// sendSessionCard sends a fresh card for a session.
func (b *Bot) sendSessionCard(chatID int64, s *engine.Session) {
	st := s.Snapshot()
	msgID, err := b.api.Send(chatID, sessionCardText(st), sessionCardKB(st))
	if err != nil {
		b.log.Warnf("send session card: %v", err)
		return
	}
	b.bindCard(chatID, msgID, s, st)
}

// stopCommand implements /stop [id|all].
func (b *Bot) stopCommand(chatID int64, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" || arg == "all" {
		list := b.eng.Active()
		for _, s := range list {
			s.Stop()
		}
		b.sendText(chatID, fmt.Sprintf("⏹ Stopping %d session(s).", len(list)), mainMenuKB())
		return
	}
	s := b.eng.Get(engine.SessionID(arg))
	if s == nil {
		b.sendText(chatID, "Session not found.", mainMenuKB())
		return
	}
	s.Stop()
	b.sendText(chatID, "⏹ Stopping "+esc(arg)+".", mainMenuKB())
}

// statuses snapshots active sessions.
func (b *Bot) statuses() []engine.Status {
	list := b.eng.Active()
	out := make([]engine.Status, 0, len(list))
	for _, s := range list {
		out = append(out, s.Snapshot())
	}
	return out
}

// mainText renders the landing dashboard text.
func (b *Bot) mainText() string {
	return mainMenuText(b.api.BotUsername(), len(b.eng.Active()), b.cfg.Limits.MaxSessions, health.FormatUptime(time.Since(b.started)))
}

// healthText renders the health card.
func (b *Bot) healthText() string {
	path, errMsg := b.BrowserProbe()
	snap := b.Snapshot()
	snap.BrowserPath = path
	return healthText(snap, errMsg)
}

// sendMain sends the dashboard as a new message.
func (b *Bot) sendMain(chatID int64) {
	msgID, err := b.api.Send(chatID, b.mainText(), mainMenuKB())
	if err == nil {
		b.mu.Lock()
		b.menus[chatID] = msgID
		b.mu.Unlock()
	}
}

// sendSessions sends the session list.
func (b *Bot) sendSessions(chatID int64) {
	list := b.statuses()
	b.sendText(chatID, sessionsText(list), sessionsKB(list))
}

// sendHealth sends the health card.
func (b *Bot) sendHealth(chatID int64) {
	b.sendText(chatID, b.healthText(), backKB())
}

// sendText is a small helper.
func (b *Bot) sendText(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	if _, err := b.api.Send(chatID, text, kb); err != nil {
		b.log.Warnf("send message: %v", err)
	}
}

// max/min for ints (Go 1.21 has builtins; keep local names explicit).
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
