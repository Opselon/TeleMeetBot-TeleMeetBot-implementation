package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// Flow steps for the guided new-stream wizard.
const (
	stepNone = iota
	stepMeet
	stepMedia
	// stepSwitchMedia waits for a replacement media link for a live session;
	// unlike the wizard it never changes the Meet link or creates a session.
	stepSwitchMedia
)

// flowTimeout drops half-finished wizards after inactivity.
const flowTimeout = 15 * time.Minute

type flow struct {
	step  int
	meet  string
	media string
	// sessionID is set only by stepSwitchMedia: the live session whose media
	// tab will be pointed at the new link.
	sessionID string
	updated   time.Time
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
		case "audiotrack", "audiolang", "lang", "language":
			lang := b.st.GetUserAudioLang(userID)
			b.sendText(chatID, audioLangText(lang), audioLangKB(lang))
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

	// A message with no usable text is not necessarily empty: forwarded media
	// (audio/video/document/voice) arrives with an empty caption. Ingest it
	// instead of dropping it, so a user can forward a file to present it.
	if text == "" && tgHasMedia(m) {
		b.handleTelegramMedia(chatID, userID, m)
		return
	}

	// Free text belongs to the wizard when one is open.
	f := b.getFlow(userID)
	if f == nil || f.step == stepNone {
		// A bare media link while a session is live is most likely an attempt
		// to change what is on screen: point at the switch flow instead of
		// pretending the message was meaningless.
		if target, err := b.classify(text); err == nil && len(b.eng.Active()) > 0 {
			b.sendText(chatID, fmt.Sprintf("🔀 Got <code>%s</code>.\n\nOpen the session card and press <b>Switch media</b> to put it on the share — the meeting and the share stay up.", esc(truncate(target.URL, 120))), sessionsKB(b.statuses()))
			return
		}
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
		target, err := b.classify(text)
		if err != nil {
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend an http(s) media link.", cancelKB())
			return
		}
		b.applyWizardMedia(chatID, userID, f, target)
	case stepSwitchMedia:
		b.applyMediaSwitch(chatID, userID, f, text)
	default:
		b.clearFlow(userID)
		b.sendText(chatID, "Wizard restarted. Send a Meet link, or /cancel.", mainMenuKB())
	}
}

// classify validates and classifies a media link, using a live HTTP probe so
// the server's actual Content-Type wins over an extension guess: an ".mp3"
// URL serving HTML is a page, and a bare CDN URL serving video/mp4 is direct
// media. A network failure (offline, timeout) falls back to the offline
// heuristics, so behaviour in an isolated environment is unchanged.
func (b *Bot) classify(raw string) (engine.MediaTarget, error) {
	return engine.ClassifyMediaWithProbe(raw, nil)
}

// applyWizardMedia completes the wizard's media step with an already
// classified target. It is shared by the text path (a typed link) and the
// Telegram-ingestion path (a downloaded local file) so both end in the same
// confirmation card.
func (b *Bot) applyWizardMedia(chatID, userID int64, f *flow, target engine.MediaTarget) {
	f.media = target.URL
	f.step = stepNone
	b.setFlow(userID, f)
	userLang := b.st.GetUserAudioLang(userID)
	b.sendText(chatID, confirmText(f.meet, f.media, target.Kind, userLang, b.st.GetUserMediaMode(userID)), confirmKB(userLang, b.st.GetUserMediaMode(userID)))
}

// applyMediaSwitch hands a validated replacement link to the engine, which
// repoints the live session's media tab at it. The Meet session, the screen
// share and every other session field are left alone, so the flow only waits
// for a URL and then gets out of the way.
func (b *Bot) applyMediaSwitch(chatID, userID int64, f *flow, text string) {
	target, err := b.classify(text)
	if err != nil {
		b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend an http(s) media link, or /cancel.", cancelKB())
		return
	}
	b.applyMediaSwitchTarget(chatID, userID, f, target)
}

// applyMediaSwitchTarget is the shared tail of the switch flow: it takes an
// already classified target (a typed link or an ingested Telegram file) and
// repoints the live session at it.
func (b *Bot) applyMediaSwitchTarget(chatID, userID int64, f *flow, target engine.MediaTarget) {
	s := b.eng.Get(engine.SessionID(f.sessionID))
	if s == nil {
		b.clearFlow(userID)
		b.sendText(chatID, "This session is gone — nothing to switch.", mainMenuKB())
		return
	}
	switch s.Snapshot().State {
	case engine.StateEnded, engine.StateError:
		b.clearFlow(userID)
		b.sendText(chatID, "This session has ended — nothing to switch.", mainMenuKB())
		return
	}
	b.clearFlow(userID)
	b.sendText(chatID, fmt.Sprintf("🔀 Switching to <code>%s</code>…\n\nThe share stays up; only the shared tab changes.", esc(truncate(target.URL, 120))), nil)
	if err := s.SwitchMedia(target.URL); err != nil {
		b.log.Warnf("switch media %s: %v", s.ID, err)
		b.sendText(chatID, "🔴 <b>Switch failed</b>\n\n"+esc(truncate(err.Error(), 300))+"\n\nThe session kept streaming; retry with 🔀 Switch media.", sessionCardKB(s.Snapshot()))
		return
	}
	st := s.Snapshot()
	b.sendText(chatID, sessionCardText(st), sessionCardKB(st))
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
		target, err := b.classify(fields[1])
		if err != nil {
			b.setFlow(userID, f)
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\n"+flowText(stepMedia, f.meet), cancelKB())
			return
		}
		f.media = target.URL
		f.step = stepNone
		b.setFlow(userID, f)
		userLang := b.st.GetUserAudioLang(userID)
		b.sendText(chatID, confirmText(f.meet, f.media, target.Kind, userLang, b.st.GetUserMediaMode(userID)), confirmKB(userLang, b.st.GetUserMediaMode(userID)))
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
	userLang := b.st.GetUserAudioLang(userID)
	sess, err := b.eng.Start(context.Background(), engine.StartRequest{
		MeetLink:  f.meet,
		MediaLink: f.media,
		AudioLang: userLang,
		PlayMode:  b.st.GetUserMediaMode(userID),
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
	case data == cbAudioLang:
		_ = b.api.AnswerCallback(q.ID, "")
		lang := b.st.GetUserAudioLang(userID)
		_ = b.api.Edit(chatID, msgID, audioLangText(lang), audioLangKB(lang))
	case data == cbMediaMode:
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, mediaModeText(b.st.GetUserMediaMode(userID)), mediaModeKB(b.st.GetUserMediaMode(userID)))
	case strings.HasPrefix(data, "mode:"):
		chosen := strings.TrimPrefix(data, "mode:")
		if err := b.st.SetUserMediaMode(userID, chosen); err != nil {
			b.log.Warnf("set media mode %d: %v", userID, err)
		}
		norm := b.st.GetUserMediaMode(userID)
		_ = b.api.AnswerCallback(q.ID, "Saved: "+engine.PlaybackModeDisplay(norm))
		_ = b.api.Edit(chatID, msgID, mediaModeText(norm), mediaModeKB(norm))
	case strings.HasPrefix(data, "lang:"):
		chosen := strings.TrimPrefix(data, "lang:")
		norm := engine.NormalizeAudioLang(chosen)
		b.st.SetUserAudioLang(userID, norm)
		_ = b.api.AnswerCallback(q.ID, "Saved: "+engine.AudioLangDisplayName(norm))
		_ = b.api.Edit(chatID, msgID, audioLangText(norm), audioLangKB(norm))
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

// seekStepFor maps a seek button to its delta in seconds. The ±10s buttons
// honour media.seek_step_seconds so the existing configured behaviour is
// preserved; ±30s/±60s are the fixed finer/coarser steps.
func seekStepFor(action string, cfg *config.Config) int {
	switch action {
	case "back30", "fwd30":
		return 30
	case "back60", "fwd60":
		return 60
	default:
		step := int(cfg.Media.SeekStepSeconds)
		if step <= 0 {
			step = 10
		}
		return step
	}
}

// seekAction runs one verified seek and describes the outcome honestly. The
// toast always carries the position the player actually reported; a seek that
// the player refused (live stream, not ready, past the end) becomes a clear
// reason instead of a silent "skipped".
func (b *Bot) seekAction(s *engine.Session, action string) (error, string) {
	delta := -seekStepFor(action, b.cfg)
	if strings.HasPrefix(action, "fwd") {
		delta = seekStepFor(action, b.cfg)
	}
	rep, err := s.SeekBy(delta)
	if err != nil {
		b.log.Warnf("seek %s %d s: %v", s.ID, delta, err)
		return err, ""
	}
	if !rep.OK {
		// Not an engine error: the call reached the player and the player
		// declined. Tell the operator why and where it actually is — on a
		// refusal the position is unchanged, so report Before (After is 0).
		pos := rep.After
		if pos == 0 {
			pos = rep.Before
		}
		return nil, fmt.Sprintf("%s · %s", fmtClock(pos), truncate(rep.Error, 80))
	}
	if delta < 0 {
		return nil, fmt.Sprintf("⏪ %s · %s", fmtClock(rep.After), fmtClock(rep.Duration))
	}
	return nil, fmt.Sprintf("⏩ %s / %s", fmtClock(rep.After), fmtClock(rep.Duration))
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
	// switchurl is a two-step action: open a URL flow, then act on the message.
	// It must come before the generic actions below so the flow setup never
	// races with a media-control call.
	st := s.Snapshot()
	if action == "switchurl" {
		userID := q.From.ID
		b.setFlow(userID, &flow{step: stepSwitchMedia, sessionID: string(id), meet: st.MeetURL, media: st.MediaURL})
		_ = b.api.AnswerCallback(q.ID, "send the new media link")
		_ = b.api.Edit(chatID, msgID, switchMediaText(st), cancelKB())
		return
	}
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
	case "back10", "fwd10", "back30", "fwd30", "back60", "fwd60":
		err, toast = b.seekAction(s, action)
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
