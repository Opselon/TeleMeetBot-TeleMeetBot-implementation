package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	// stepQueueAdd appends links to a live session's queue.
	stepQueueAdd
	// stepQueueAddNext inserts links right after the playing item.
	stepQueueAddNext
	// stepMeetPick is the /new landing screen: saved meetings, new Meet, cancel.
	stepMeetPick
	// stepMeetRename waits for the text message that renames a saved meeting.
	stepMeetRename
	// Playlist wizard steps: naming a new playlist, renaming/describing an
	// existing one, appending links and importing a bulk list.
	stepPlaylistName
	stepPlaylistRename
	stepPlaylistDesc
	stepPlaylistAdd
	stepPlaylistImport
	// stepPlaylistPlayMeet waits for the Meet link that will receive a whole
	// playlist as its initial queue.
	stepPlaylistPlayMeet
	// Admin-console text steps: adding an admin id and composing a broadcast.
	stepAdminAdd
	stepAdminBroadcast
	// stepGuestName waits for the Meet display-name override text.
	stepGuestName
)

// flowTimeout drops half-finished wizards after inactivity.
const flowTimeout = 15 * time.Minute

type flow struct {
	step  int
	meet  string
	media string
	// meetName is the display name of a saved meeting selected from the
	// picker, so the confirmation card can show what the user chose.
	meetName string
	// meetID is the SQLite row id of the meeting being renamed.
	meetID int64
	// sessionID is set only by stepSwitchMedia: the live session whose media
	// tab will be pointed at the new link.
	sessionID string
	// mediaID is the media-manager entry backing a staged Telegram file
	// (f.media holds its loopback URL). It is empty for typed links: only an
	// ingested file carries a lease that must be handed to the session.
	mediaID string
	// mediaKind is the classified media kind of f.media ("" until known).
	// It rides along in the persisted draft so a resumed wizard can render
	// the review card without re-probing the link.
	mediaKind string
	// queue holds any additional media URLs to populate the playlist.
	queue   []string
	updated time.Time
	// playlistID scopes a playlist wizard step to one library entry.
	playlistID int64
	// adminID scopes an admin-console text step (remove/select).
	adminID int64
	// parent remembers which screen a text step should return to
	// ("playlist" or "manage") so cancel goes back where the user was.
	parent string
	// payload is the composed text of a management broadcast. It is kept in
	// the flow between the preview and the send confirmation.
	payload string
}

// getFlow returns the user's pending wizard.
func (b *Bot) getFlow(userID int64) *flow {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flows[userID]
}

// setFlow stores the user's wizard state and mirrors creation workflows
// into the durable draft, so a restart can resume the wizard.
func (b *Bot) setFlow(userID int64, f *flow) {
	f.updated = time.Now()
	b.mu.Lock()
	b.flows[userID] = f
	b.mu.Unlock()
	b.persistDraft(userID, f)
}

// clearFlow drops the wizard state and its persisted draft, if it was one.
func (b *Bot) clearFlow(userID int64) {
	b.mu.Lock()
	f := b.flows[userID]
	delete(b.flows, userID)
	b.mu.Unlock()
	if _, isCreation := workflowStateOf(f); isCreation {
		b.dropDraft(userID)
	}
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
		b.log.Infof("[PIPELINE:bot] command /%s received (args=%q) from user=%d chat=%d", m.Command(), m.CommandArguments(), userID, chatID)
		switch m.Command() {
		case "start":
			b.clearFlow(userID)
			b.sendMain(chatID, userID)
		case "new", "stream":
			b.startFlow(chatID, userID, m.CommandArguments())
		case "sessions":
			b.sendSessions(chatID)
		case "playlists", "playlist", "library", "pl":
			b.sendPlaylists(chatID, userID)
		case "manage", "admin", "dashboard":
			b.sendManageHome(chatID, userID)
		case "queue":
			b.queueCommand(chatID, userID, m.CommandArguments())
		case "audiotrack", "audiolang", "lang", "language":
			lang := b.st.GetUserAudioLang(userID)
			b.sendText(chatID, audioLangText(lang), audioLangKB(lang))
		case "health":
			b.sendHealth(chatID)
		case "settings", "prefs", "preferences":
			b.showUserSettings(chatID, 0, userID)
		case "account", "google", "profile":
			b.showAccount(chatID, userID, 0)
		case "help":
			b.sendText(chatID, helpText(), helpKB())
		case "about":
			b.sendText(chatID, aboutText(b.api.BotUsername()), helpKB())
		case "cancel":
			b.clearFlow(userID)
			b.sendText(chatID, "✖️ Cancelled. Nothing pending.", mainMenuKB())
		case "home":
			// Navigation only: an active session keeps running.
			b.clearFlow(userID)
			b.sendMain(chatID, userID)
		case "back":
			b.backFromWorkflow(chatID, userID)
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

	// Free text or documents belong to the cookie flow when active.
	f := b.getFlow(userID)

	// Forwarded media (audio/video/document/voice) arrives through the ingestion path.
	if tgHasMedia(m) {
		b.handleTelegramMedia(chatID, userID, m)
		return
	}

	urls := extractURLs(text)
	// Free text belongs to the wizard when one is open.
	if f == nil || f.step == stepNone {
		if len(urls) > 0 {
			if len(b.eng.Active()) > 0 {
				// Live session active! Enqueue valid URLs directly in submission order
				s := b.eng.Active()[0]
				added, err := s.Enqueue(urls...)
				if err != nil {
					b.sendText(chatID, "❌ "+esc(err.Error()), mainMenuKB())
					return
				}
				items, curIdx := s.QueueSnapshot()
				var qRecs []store.QueueItemRecord
				for _, it := range items {
					qRecs = append(qRecs, store.QueueItemRecord{ID: it.ID, URL: it.URL, Title: it.Title})
				}
				b.st.UpdateQueue(string(s.ID), qRecs, curIdx)
				b.sendText(chatID, fmt.Sprintf("➕ Enqueued %d item(s) to session <code>%s</code> (Total in queue: %d)",
					len(added), s.ID, len(items)), enqueuedKB(string(s.ID), added[0].ID))
				return
			}
			if len(urls) > 1 {
				// Batch URLs when no session is live: stage them and prompt for Meet link
				f := &flow{step: stepMeet, media: urls[0], queue: urls[1:]}
				b.setFlow(userID, f)
				b.sendText(chatID, fmt.Sprintf("▶️ <b>New stream (%d items queued)</b>\n\nSend a Google Meet link to launch playback.", len(urls)), cancelKB())
				return
			}
			target, err := b.classify(urls[0])
			if err == nil {
				f := &flow{step: stepMeet, media: target.URL}
				b.setFlow(userID, f)
				b.sendText(chatID, fmt.Sprintf("▶️ <b>New stream</b>\n\n🎞 <code>%s</code>\n\nSend a Google Meet link to launch.", esc(truncate(target.URL, 70))), cancelKB())
				return
			}
		}
		b.sendText(chatID, "Use /new (or ➕ New stream) to start a session.", mainMenuKB())
		return
	}
	// The saved-meeting rename step consumes the next text message.
	if b.tryMeetingsInput(chatID, userID, f, text) {
		return
	}
	// Settings, playlist and admin-console text steps consume their own
	// messages.
	if b.trySettingsInput(chatID, userID, f, text) {
		return
	}
	if b.tryPlaylistInput(chatID, userID, f, text) {
		return
	}
	if b.tryManageInput(chatID, userID, f, text) {
		return
	}
	b.stepInput(chatID, userID, f, text)
}

// stepInput handles a wizard answer.
func (b *Bot) stepInput(chatID, userID int64, f *flow, text string) {
	switch f.step {
	case stepMeetPick:
		// The picker is up; a typed link is accepted as the meet link and
		// the flow continues at media selection.
		normalized, err := engine.NormalizeMeetURL(text)
		if err != nil {
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend a Meet link like <code>meet.google.com/abc-defg-hij</code>, or pick a saved meeting.", cancelKB())
			return
		}
		f.meet = normalized
		f.step = stepMedia
		b.setFlow(userID, f)
		b.sendText(chatID, flowText(stepMedia, f.meet), cancelKB())
	case stepMeet:
		normalized, err := engine.NormalizeMeetURL(text)
		if err != nil {
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nSend a Meet link like <code>meet.google.com/abc-defg-hij</code>.", cancelKB())
			return
		}
		f.meet = normalized
		// A staged Telegram file already supplies the media: the wizard is
		// done as soon as the Meet link is known, so the user is never asked
		// to re-send a link they cannot reach.
		if strings.TrimSpace(f.media) != "" {
			f.step = stepNone
			b.setFlow(userID, f)
			userLang := b.st.GetUserAudioLang(userID)
			kind := engine.MediaDirect
			if items := b.getPending(userID); len(items) > 0 {
				for _, it := range items {
					if it.URL == f.media {
						kind = it.Kind
						break
					}
				}
			}
			f.mediaKind = string(kind)
			guestName, _ := b.effectiveGuestName(userID)
			b.sendText(chatID, confirmText(f.meet, f.media, kind, userLang, b.st.GetUserMediaMode(userID), b.userSettings(userID).QualityPolicy, guestName, b.hasGoogleAccount(userID)), confirmKB(userLang, b.st.GetUserMediaMode(userID), b.userSettings(userID).QualityPolicy, b.hasGoogleAccount(userID)))
			return
		}
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
	case stepQueueAdd, stepQueueAddNext:
		b.applyQueueAdd(chatID, userID, f, text)
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
	// A staged Telegram file keeps its own lease until the session that will
	// play it has leased it too; only then may the flow's pin be dropped.
	if engine.IsLocalMediaURL(target.URL) {
		b.handoffPending(userID, f.mediaID)
	}
	f.media = target.URL
	f.mediaKind = string(target.Kind)
	f.step = stepNone
	b.setFlow(userID, f)
	userLang := b.st.GetUserAudioLang(userID)
	guestName, _ := b.effectiveGuestName(userID)
	b.sendText(chatID, confirmText(f.meet, f.media, target.Kind, userLang, b.st.GetUserMediaMode(userID), b.userSettings(userID).QualityPolicy, guestName, b.hasGoogleAccount(userID)), confirmKB(userLang, b.st.GetUserMediaMode(userID), b.userSettings(userID).QualityPolicy, b.hasGoogleAccount(userID)))
}

// applyQueueAdd consumes the queue-add flow: every link in the message is
// validated, then appended (stepQueueAdd) or inserted after the playing item
// (stepQueueAddNext). Nothing is enqueued when any link is invalid, so the
// user can fix the whole batch at once.
func (b *Bot) applyQueueAdd(chatID, userID int64, f *flow, text string) {
	s := b.eng.Get(engine.SessionID(f.sessionID))
	if s == nil {
		b.clearFlow(userID)
		b.sendText(chatID, "This session is gone — nothing to add to.", mainMenuKB())
		return
	}
	urls := extractURLs(text)
	if len(urls) == 0 {
		b.sendText(chatID, "❌ No media links found.\n\nSend one or more http(s) links, or /cancel.", playlistCancelKB())
		return
	}
	// Validate up front so a typo cannot half-fill the queue.
	for _, u := range urls {
		if _, err := b.classify(u); err != nil {
			b.sendText(chatID, "❌ "+esc(err.Error())+"\n\nLink: <code>"+esc(truncate(u, 90))+"</code>\n\nFix it and send the batch again, or /cancel.", playlistCancelKB())
			return
		}
	}
	b.clearFlow(userID)
	var (
		added []engine.QueueItem
		err   error
		label string
	)
	if f.step == stepQueueAddNext {
		added, err = s.EnqueueNext(urls...)
		label = "queued to play next"
	} else {
		added, err = s.Enqueue(urls...)
		label = "enqueued"
	}
	if err != nil {
		b.sendText(chatID, "❌ "+esc(truncate(err.Error(), 200)), mainMenuKB())
		return
	}
	b.persistQueue(s)
	st := s.Snapshot()
	kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("📋 View queue", "act:"+string(s.ID)+":queue")},
		{tgbotapi.NewInlineKeyboardButtonData("📱 Session controls", "ses:"+string(s.ID))},
	}}
	b.sendText(chatID, fmt.Sprintf("✅ %s %d item(s) — queue now has <b>%d</b> item(s).", esc(label), len(added), st.QueueLength), kb)
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
	if engine.IsTerminalState(s.Snapshot().State) {
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
	fields := strings.Fields(args)
	if len(fields) == 0 {
		// A draft that survived a restart wins: resume it where it stopped
		// instead of making the user retype everything. An in-memory wizard
		// is already live, so it takes precedence.
		if b.getFlow(userID) == nil {
			if draft, ok := b.restoreDraft(userID); ok {
				b.setFlow(userID, draft)
				b.sendText(chatID, "📝 <b>Draft restored</b> — continue where you left off:", b.workflowScreenKB(userID, draft))
				b.sendText(chatID, b.workflowScreenText(userID, draft), nil)
				return
			}
		}
		// Plain /new lands on the saved-meetings picker: saved links first,
		// then New Google Meet, then Cancel.
		b.startMeetPicker(chatID, userID)
		return
	}
	f := &flow{step: stepMeet}
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
		f.mediaKind = string(target.Kind)
		f.step = stepNone
		b.setFlow(userID, f)
		userLang := b.st.GetUserAudioLang(userID)
		guestName, _ := b.effectiveGuestName(userID)
		b.sendText(chatID, confirmText(f.meet, f.media, target.Kind, userLang, b.st.GetUserMediaMode(userID), b.userSettings(userID).QualityPolicy, guestName, b.hasGoogleAccount(userID)), confirmKB(userLang, b.st.GetUserMediaMode(userID), b.userSettings(userID).QualityPolicy, b.hasGoogleAccount(userID)))
		return
	}
	b.setFlow(userID, f)
	b.sendText(chatID, flowText(f.step, f.meet), cancelKB())
}

// startRequestFor builds the engine request for a user, applying their saved
// preferences (audio language, media mode, volume, speed, repeat, shuffle,
// auto-advance) on top of the server defaults.
func (b *Bot) startRequestFor(userID int64, meetURL string, req engine.StartRequest) engine.StartRequest {
	u := b.userSettings(userID)
	req.AudioLang = u.AudioLang
	req.PlayMode = u.MediaMode
	req.Volume = u.Volume
	req.Speed = u.Speed
	req.RepeatMode = u.RepeatMode
	req.Quality = u.QualityPolicy
	req.Shuffle = false // no per-user shuffle default yet; toggle lives on the session
	autoplay := u.Autoplay
	req.AutoPlay = &autoplay
	return req
}

// effectiveGuestName resolves the Meet display name for a user. Precedence:
// the user's explicit setting wins, then the verified Google identity, then
// the server default (empty string, which startRequestFor's callers leave to
// the engine config). The second return names the source for the review card.
func (b *Bot) effectiveGuestName(userID int64) (string, string) {
	if u := b.userSettings(userID); strings.TrimSpace(u.GuestName) != "" {
		return u.GuestName, "your settings"
	}
	if b.gauth != nil {
		if sum := b.gauth.Summary(userID); sum.Connected && sum.Name != "" {
			return sum.Name, "verified Google account"
		}
	}
	return "", "server default"
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
	req := b.startRequestFor(userID, f.meet, engine.StartRequest{
		MeetLink:  f.meet,
		MediaLink: f.media,
	})
	if len(f.queue) > 0 {
		req.Queue = f.queue
	}
	if b.gauth != nil {
		sum := b.gauth.Summary(userID)
		if sum.Connected {
			if sum.Name != "" {
				req.GuestName = fmt.Sprintf("%s (%s)", sum.Name, sum.Email)
			} else {
				req.GuestName = sum.Email
			}
			b.log.Infof("[PIPELINE:googleauth] launching session with verified Google identity: %s for user=%d", req.GuestName, userID)
		}
	}
	if guestName, source := b.effectiveGuestName(userID); guestName != "" {
		req.GuestName = guestName
		b.log.Infof("[PIPELINE:workflow] guest name from %s for user=%d", source, userID)
	}
	sess, err := b.eng.Start(context.Background(), req)
	if err != nil {
		b.sendText(chatID, "❌ "+esc(err.Error()), mainMenuKB())
		return
	}
	b.clearFlow(userID)
	var qItemRecs []store.QueueItemRecord
	items, curIdx := sess.QueueSnapshot()
	for _, it := range items {
		qItemRecs = append(qItemRecs, store.QueueItemRecord{ID: it.ID, URL: it.URL, Title: it.Title})
	}
	b.st.Add(store.Record{
		ID:        string(sess.ID),
		MeetURL:   sess.MeetURL,
		MediaURL:  sess.MediaURL,
		Title:     sess.TabTitle,
		State:     engine.StateStarting,
		ChatID:    chatID,
		CreatedAt: time.Now(),
		Queue:     qItemRecs,
		QueueIdx:  curIdx,
	})
	b.mu.Lock()
	b.sessionChats[string(sess.ID)] = chatID
	b.mu.Unlock()
	// Persist the initial queue metadata (JSON history + SQLite).
	b.persistQueue(sess)
	// Remember the Meet link for the owner so /new can offer it next time.
	// The save is best-effort: a store failure must never block a session
	// that is already running.
	b.saveCurrentMeet(userID, sess.MeetURL)
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
		b.log.Warnf("[PIPELINE:bot] unauthorized callback query from user=%d chat=%d data=%q", userID, chatID, q.Data)
		return
	}
	data := q.Data
	b.log.Infof("[PIPELINE:bot] callback button press: data=%q user=%d chat=%d msg=%d", data, userID, chatID, msgID)
	switch {
	case data == cbHome || data == cbRefresh:
		b.log.Infof("[PIPELINE:bot] action: home/refresh from user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "dashboard")
		_ = b.api.Edit(chatID, msgID, b.mainText(userID), mainMenuKB())
	case data == cbNew:
		b.log.Infof("[PIPELINE:bot] action: new stream wizard started by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "new stream")
		b.startFlow(chatID, userID, "")
	case data == cbSessions:
		b.log.Infof("[PIPELINE:bot] action: view sessions by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, sessionsText(b.statuses()), sessionsKB(b.statuses()))
	case data == cbPlaylists:
		b.log.Infof("[PIPELINE:bot] action: open playlists by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		b.showPlaylistList(chatID, userID, msgID, 0)
	case data == cbManage:
		b.log.Infof("[PIPELINE:bot] action: open management console by user=%d chat=%d", userID, chatID)
		b.handleManageCallback(chatID, userID, msgID, q, data)
	case data == cbHealth:
		b.log.Infof("[PIPELINE:bot] action: view health by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, b.healthText(), backKB())
	case data == cbSettings:
		b.log.Infof("[PIPELINE:bot] action: view settings by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		b.showUserSettings(chatID, msgID, userID)
	case data == cbAccount:
		b.log.Infof("[PIPELINE:bot] action: view account by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		b.showAccount(chatID, userID, msgID)
	case data == cbAccountConnect:
		b.log.Infof("[PIPELINE:bot] action: connect account by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		b.showAccount(chatID, userID, msgID)
	case data == cbAccountRemove:
		b.log.Infof("[PIPELINE:bot] action: remove account by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "Google Account disconnected")
		b.handleAccountDisconnect(chatID, userID, msgID)
	case data == cbAccountHelp:
		b.log.Infof("[PIPELINE:bot] action: account help by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, accountHelpText(), backKB())
	case data == cbHelp:
		b.log.Infof("[PIPELINE:bot] action: view help by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, helpText(), helpKB())
	case data == cbAudioLang:
		b.log.Infof("[PIPELINE:bot] action: audio language menu by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		lang := b.st.GetUserAudioLang(userID)
		_ = b.api.Edit(chatID, msgID, audioLangText(lang), audioLangKB(lang))
	case data == cbMediaMode:
		b.log.Infof("[PIPELINE:bot] action: media mode menu by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, mediaModeText(b.st.GetUserMediaMode(userID)), mediaModeKB(b.st.GetUserMediaMode(userID)))
	case strings.HasPrefix(data, "mode:"):
		chosen := strings.TrimPrefix(data, "mode:")
		b.log.Infof("[PIPELINE:bot] action: set media mode %q by user=%d chat=%d", chosen, userID, chatID)
		if err := b.st.SetUserMediaMode(userID, chosen); err != nil {
			b.log.Warnf("set media mode %d: %v", userID, err)
		}
		norm := b.st.GetUserMediaMode(userID)
		_ = b.api.AnswerCallback(q.ID, "Saved: "+engine.PlaybackModeDisplay(norm))
		_ = b.api.Edit(chatID, msgID, mediaModeText(norm), mediaModeKB(norm))
	case strings.HasPrefix(data, "lang:"):
		chosen := strings.TrimPrefix(data, "lang:")
		b.log.Infof("[PIPELINE:bot] action: set audio lang %q by user=%d chat=%d", chosen, userID, chatID)
		norm := engine.NormalizeAudioLang(chosen)
		b.st.SetUserAudioLang(userID, norm)
		_ = b.api.AnswerCallback(q.ID, "Saved: "+engine.AudioLangDisplayName(norm))
		_ = b.api.Edit(chatID, msgID, audioLangText(norm), audioLangKB(norm))
	case data == cbEditMeet || data == cbEditMedia || data == cbResumeDraft:
		b.log.Infof("[PIPELINE:workflow] callback %q by user=%d chat=%d", data, userID, chatID)
		b.handleWorkflowCallback(chatID, userID, msgID, q, data)
	case data == cbLaunch:
		b.log.Infof("[PIPELINE:bot] action: launch session confirmed by user=%d chat=%d", userID, chatID)
		_ = b.api.AnswerCallback(q.ID, "launching…")
		b.launch(chatID, userID)
	case data == cbCancel:
		b.log.Infof("[PIPELINE:bot] action: cancel flow by user=%d chat=%d", userID, chatID)
		b.clearFlow(userID)
		_ = b.api.AnswerCallback(q.ID, "cancelled")
		_ = b.api.Edit(chatID, msgID, "✖️ Cancelled.", mainMenuKB())
	case data == cbAgain:
		b.log.Infof("[PIPELINE:bot] action: restart wizard by user=%d chat=%d", userID, chatID)
		b.setFlow(userID, &flow{step: stepMeet})
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, flowText(stepMeet, ""), cancelKB())
	case strings.HasPrefix(data, "ses:"):
		id := strings.TrimPrefix(data, "ses:")
		b.log.Infof("[PIPELINE:bot] action: open session card session=%s by user=%d chat=%d", id, userID, chatID)
		s := b.eng.Get(engine.SessionID(id))
		if s == nil {
			b.log.Warnf("[PIPELINE:bot] open session card: session %s not found", id)
			_ = b.api.AnswerCallback(q.ID, "session not found")
			return
		}
		_ = b.api.AnswerCallback(q.ID, "")
		st := s.Snapshot()
		_ = b.api.Edit(chatID, msgID, sessionCardText(st), sessionCardKB(st))
		b.bindCard(chatID, msgID, s, st)
	case strings.HasPrefix(data, "set:"):
		b.handleSettingsCallback(chatID, userID, msgID, q, data)
	case strings.HasPrefix(data, "pm:"):
		b.handlePendingCallback(chatID, userID, msgID, data)
	case strings.HasPrefix(data, "q:"):
		b.handleQueueCallback(chatID, userID, msgID, q, data)
	case strings.HasPrefix(data, "act:"):
		b.handleAction(chatID, msgID, q, data)
	case strings.HasPrefix(data, cbMeetPick) || strings.HasPrefix(data, cbMeetDelete) ||
		strings.HasPrefix(data, cbMeetRename) || data == cbMeetNew || data == cbMeetList || data == cbMeetDone:
		b.handleMeetCallback(chatID, userID, msgID, data)
	case strings.HasPrefix(data, cbPlaylistMeet):
		b.handlePlaylistMeetCallback(chatID, userID, msgID, q, data)
	case strings.HasPrefix(data, cbPlaylist):
		b.handlePlaylistCallback(chatID, userID, msgID, q, data)
	case strings.HasPrefix(data, cbManage):
		b.handleManageCallback(chatID, userID, msgID, q, data)
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
		b.log.Warnf("[PIPELINE:bot] invalid action callback data=%q from user=%d", data, q.From.ID)
		_ = b.api.AnswerCallback(q.ID, "bad action")
		return
	}
	id := engine.SessionID(parts[0])
	action := parts[1]
	b.log.Infof("[PIPELINE:bot] session control button pressed: session=%s action=%q user=%d chat=%d", id, action, q.From.ID, chatID)
	s := b.eng.Get(id)
	if s == nil {
		b.log.Warnf("[PIPELINE:bot] session control: session=%s not found", id)
		_ = b.api.AnswerCallback(q.ID, "session not found")
		_ = b.api.Edit(chatID, msgID, "This session is gone.", mainMenuKB())
		return
	}
	// switchurl is a two-step action: open a URL flow, then act on the message.
	// It must come before the generic actions below so the flow setup never
	// races with a media-control call.
	st := s.Snapshot()
	switch action {
	case "quality", "qinfo", "qformats", "qapply", "qmode":
		b.handleQualityCallback(chatID, q.From.ID, msgID, q, data)
		return
	}
	if strings.HasPrefix(action, "qset:") || strings.HasPrefix(action, "qfmt:") {
		b.handleQualityCallback(chatID, q.From.ID, msgID, q, data)
		return
	}
	if action == "stop_confirm" {
		_ = b.api.AnswerCallback(q.ID, "confirm to stop")
		_ = b.api.Edit(chatID, msgID, stopConfirmText(st), stopConfirmKB(string(id)))
		return
	}
	if action == "stop_ok" {
		// The confirm button re-looks the session up by id; a session that
		// ended between the two presses is answered honestly instead of
		// tearing anything else down.
		_ = b.api.AnswerCallback(q.ID, "stopping")
		s.Stop()
		_ = b.api.Edit(chatID, msgID, "⏹ Session "+string(id)+" is stopping.", mainMenuKB())
		return
	}
	if action == "speed" {
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, speedMenuText(st), speedMenuKB(st))
		return
	}
	if strings.HasPrefix(action, "rate:") {
		var centis int
		if _, err := fmt.Sscanf(strings.TrimPrefix(action, "rate:"), "%d", &centis); err != nil || centis < 25 || centis > 400 {
			_ = b.api.AnswerCallback(q.ID, "invalid speed")
			return
		}
		if rerr := s.SetRate(float64(centis) / 100); rerr != nil {
			_ = b.api.AnswerCallback(q.ID, "speed failed: "+truncate(rerr.Error(), 60))
		} else {
			_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("speed %.2gx", float64(centis)/100))
		}
		st = s.Snapshot()
		_ = b.api.Edit(chatID, msgID, sessionCardText(st), sessionCardKB(st))
		b.bindCard(chatID, msgID, s, st)
		return
	}
	if action == "switchurl" {
		userID := q.From.ID
		b.setFlow(userID, &flow{step: stepSwitchMedia, sessionID: string(id), meet: st.MeetURL, media: st.MediaURL})
		_ = b.api.AnswerCallback(q.ID, "send the new media link")
		_ = b.api.Edit(chatID, msgID, switchMediaText(st), cancelKB())
		return
	}
	if action == "tools" {
		items, curIdx := s.QueueSnapshot()
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, playlistQueueToolsText(st, items, curIdx), playlistQueueToolsKB(st))
		return
	}
	var err error
	toast := ""
	switch action {
	case "next":
		err = s.Next()
		if err != nil {
			toast = err.Error()
		} else {
			toast = "⏭ Next track"
		}
		b.persistQueue(s)
		items, curIdx := s.QueueSnapshot()
		var qRecs []store.QueueItemRecord
		for _, it := range items {
			qRecs = append(qRecs, store.QueueItemRecord{ID: it.ID, URL: it.URL, Title: it.Title})
		}
		b.st.UpdateQueue(string(s.ID), qRecs, curIdx)
	case "prev":
		err = s.Previous()
		switch {
		case err == nil:
			toast = "⏮ Previous track"
		case errors.Is(err, engine.ErrQueueStart):
			// Documented boundary behaviour: at the first item, Previous
			// restarts it from the beginning (a seek, so the share never
			// blinks) instead of failing silently.
			rep, rerr := s.SeekTo(0)
			switch {
			case rerr != nil:
				err = rerr
				toast = "restart failed"
			case !rep.OK:
				err = nil
				toast = "⏮ Already first — " + truncate(rep.Error, 60)
			default:
				err = nil
				toast = "⏮ Already first — restarted item"
			}
		default:
			toast = err.Error()
		}
		b.persistQueue(s)
		items, curIdx := s.QueueSnapshot()
		var qRecs []store.QueueItemRecord
		for _, it := range items {
			qRecs = append(qRecs, store.QueueItemRecord{ID: it.ID, URL: it.URL, Title: it.Title})
		}
		b.st.UpdateQueue(string(s.ID), qRecs, curIdx)
	case "queue":
		_ = b.api.AnswerCallback(q.ID, "")
		b.renderQueue(chatID, msgID, s, -1)
		return
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
		b.log.Warnf("[PIPELINE:bot] session control failed: session=%s action=%q user=%d err=%v", id, action, q.From.ID, err)
		_ = b.api.AnswerCallback(q.ID, "error: "+truncate(err.Error(), 60))
	} else {
		b.log.Infof("[PIPELINE:bot] session control succeeded: session=%s action=%q user=%d toast=%q", id, action, q.From.ID, toast)
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

// mainText renders the landing dashboard text, advertising a resumable
// creation draft when one survived a restart for this user.
func (b *Bot) mainText(userID int64) string {
	text := mainMenuText(b.api.BotUsername(), len(b.eng.Active()), b.cfg.Limits.MaxSessions, health.FormatUptime(time.Since(b.started)))
	if b.hasFreshDraft(userID) {
		text += resumeDraftNotice()
	}
	return text
}

// healthText renders the health card.
func (b *Bot) healthText() string {
	path, errMsg := b.BrowserProbe()
	snap := b.Snapshot()
	snap.BrowserPath = path
	return healthText(snap, errMsg)
}

// sendMain sends the dashboard as a new message.
func (b *Bot) sendMain(chatID, userID int64) {
	msgID, err := b.api.Send(chatID, b.mainText(userID), mainMenuKB())
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

// extractURLs finds all http(s) URLs in a text message, preserving submission order.
func extractURLs(text string) []string {
	fields := strings.Fields(text)
	var urls []string
	for _, f := range fields {
		f = strings.Trim(f, "<>(),;\"'[]")
		if strings.HasPrefix(strings.ToLower(f), "http://") || strings.HasPrefix(strings.ToLower(f), "https://") {
			urls = append(urls, f)
		}
	}
	return urls
}

// renderQueue shows the queue screen. page < 0 selects the page holding the
// currently playing item.
func (b *Bot) renderQueue(chatID int64, msgID int, s *engine.Session, page int) {
	st := s.Snapshot()
	items, curIdx := s.QueueSnapshot()
	if page < 0 {
		page = 0
		if curIdx > 0 {
			page = curIdx / queuePageSize
		}
	}
	if total := queuePageCount(len(items)); total > 0 && page >= total {
		page = total - 1
	}
	_ = b.api.Edit(chatID, msgID, queueViewText(st, items, curIdx, page), queueViewKB(st, items, curIdx, page))
}

// persistQueue mirrors a session queue into both durable stores: SQLite is
// authoritative for restart survival, the JSON history keeps the management
// console working during the transition.
func (b *Bot) persistQueue(s *engine.Session) {
	if s == nil {
		return
	}
	items, curIdx := s.QueueSnapshot()
	recs := make([]store.QueueItemRecord, 0, len(items))
	snap := store.SessionQueueSnapshot{
		SessionID: string(s.ID),
		State:     store.QueueStateActive,
		Cursor:    curIdx,
	}
	for _, it := range items {
		recs = append(recs, store.QueueItemRecord{ID: it.ID, URL: it.URL, Title: it.Title})
		snap.Items = append(snap.Items, store.QueueItemSnapshot{
			URL:      it.URL,
			Title:    it.Title,
			Kind:     string(it.Kind),
			Duration: it.Duration,
		})
	}
	b.st.UpdateQueue(string(s.ID), recs, curIdx)
	if b.set == nil {
		return
	}
	owner := b.queueOwnerChat(s.ID)
	if owner == 0 {
		return
	}
	snap.OwnerID = owner
	ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
	defer cancel()
	if err := b.set.SaveSessionQueue(ctx, snap); err != nil {
		b.log.Warnf("[PIPELINE:queue] persist queue %s: %v", s.ID, err)
	}
}

// handleQueueCallback handles playlist operations.
func (b *Bot) handleQueueCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.Split(data, ":")
	if len(parts) < 3 {
		b.log.Warnf("[PIPELINE:queue] invalid queue callback data=%q from user=%d", data, userID)
		_ = b.api.AnswerCallback(q.ID, "bad queue action")
		return
	}
	sub := parts[1]
	sid := parts[2]
	b.log.Infof("[PIPELINE:queue] queue callback: session=%s sub=%q data=%q user=%d chat=%d", sid, sub, data, userID, chatID)
	s := b.eng.Get(engine.SessionID(sid))
	if s == nil {
		b.log.Warnf("[PIPELINE:queue] queue action on expired/missing session=%s by user=%d", sid, userID)
		_ = b.api.AnswerCallback(q.ID, "session not found")
		_ = b.api.Edit(chatID, msgID, "This session has ended.", mainMenuKB())
		return
	}
	st := s.Snapshot()

	// Advanced queue tools (move/save) live in their own handler so the
	// basic view stays readable.
	if b.handleQueueToolsCallback(chatID, userID, msgID, q, sub, sid, parts) {
		return
	}

	switch sub {
	case "jump":
		if len(parts) < 4 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		itemID := parts[3]
		items, _ := s.QueueSnapshot()
		idx := -1
		for i, it := range items {
			if it.ID == itemID {
				idx = i
				break
			}
		}
		if idx < 0 {
			// The item was removed (or the callback is stale) — say so
			// instead of leaving the button spinner forever.
			b.log.Warnf("[PIPELINE:queue] jump to missing item %s in session %s by user %d", itemID, sid, userID)
			_ = b.api.AnswerCallback(q.ID, "item not found (queue changed)")
			b.renderQueue(chatID, msgID, s, -1)
			return
		}
		b.log.Infof("[PIPELINE:queue] jump to item %s (index %d/%d) in session %s by user %d", itemID, idx+1, len(items), sid, userID)
		if jerr := s.JumpTo(idx); jerr != nil {
			b.log.Warnf("[PIPELINE:queue] jump failed: %v", jerr)
			_ = b.api.AnswerCallback(q.ID, "jump failed: "+truncate(jerr.Error(), 60))
		} else {
			_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("playing item %d/%d", idx+1, len(items)))
		}
		b.persistQueue(s)
		st = s.Snapshot()
		_ = b.api.Edit(chatID, msgID, sessionCardText(st), sessionCardKB(st))

	case "page":
		if len(parts) < 4 {
			_ = b.api.AnswerCallback(q.ID, "")
			b.renderQueue(chatID, msgID, s, 0)
			return
		}
		page := 0
		if n, err := strconv.Atoi(parts[3]); err == nil && n >= 0 {
			page = n
		}
		_ = b.api.AnswerCallback(q.ID, "")
		b.renderQueue(chatID, msgID, s, page)

	case "repeat":
		next := engine.RepeatOne
		switch engine.NormalizeRepeatMode(st.RepeatMode) {
		case engine.RepeatOne:
			next = engine.RepeatAll
		case engine.RepeatAll:
			next = engine.RepeatOff
		}
		if rerr := s.SetRepeatMode(next); rerr != nil {
			_ = b.api.AnswerCallback(q.ID, "repeat failed: "+truncate(rerr.Error(), 60))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "repeat: "+repeatDisplay(next))
		b.renderQueue(chatID, msgID, s, -1)

	case "shuftoggle":
		s.SetShuffle(!st.Shuffle)
		_ = b.api.AnswerCallback(q.ID, "shuffle "+boolWord(!st.Shuffle))
		b.renderQueue(chatID, msgID, s, -1)

	case "add":
		_ = b.api.AnswerCallback(q.ID, "send one or more links")
		b.setFlow(userID, &flow{step: stepQueueAdd, sessionID: sid, parent: "queue"})
		_ = b.api.Edit(chatID, msgID, queueAddText(st, "add"), playlistCancelKB())

	case "addnext":
		_ = b.api.AnswerCallback(q.ID, "send the links to play next")
		b.setFlow(userID, &flow{step: stepQueueAddNext, sessionID: sid, parent: "queue"})
		_ = b.api.Edit(chatID, msgID, queueAddText(st, "addnext"), playlistCancelKB())

	case "load":
		// Load a saved playlist in place of the upcoming items.
		if b.pl == nil {
			_ = b.api.AnswerCallback(q.ID, "playlists unavailable")
			return
		}
		list, err := playlistCall(b, func(ctx context.Context) ([]store.Playlist, error) {
			return b.pl.List(ctx, userID)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		if len(list) == 0 {
			_ = b.api.AnswerCallback(q.ID, "no saved playlists")
			_ = b.api.Edit(chatID, msgID, "You have no saved playlists yet. Open 📚 <b>Playlists</b> to create one.", backKB())
			return
		}
		_ = b.api.AnswerCallback(q.ID, "pick a playlist")
		_ = b.api.Edit(chatID, msgID, queueLoadText(st), queueLoadKB(st, list))

	case "loadpl":
		if len(parts) < 4 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		pid, ok := parseID(parts[3])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		p, err := playlistCall(b, func(ctx context.Context) (store.Playlist, error) {
			return b.pl.Get(ctx, userID, pid)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		items, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
			return b.pl.Items(ctx, userID, pid)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
			return
		}
		if len(items) == 0 {
			_ = b.api.AnswerCallback(q.ID, "playlist is empty")
			return
		}
		// Confirmation: loading replaces everything after the current item.
		_ = b.api.AnswerCallback(q.ID, "")
		text := fmt.Sprintf("⚠️ <b>Load playlist?</b>\n\n<b>%s</b> (%d item(s)) will replace everything after the currently playing item in session <code>%s</code>.",
			esc(p.Name), len(items), esc(sid))
		kb := &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("📥 Load playlist", fmt.Sprintf("q:loadok:%s:%d", sid, p.ID))},
			{tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "act:"+sid+":queue")},
		}}
		_ = b.api.Edit(chatID, msgID, text, kb)

	case "loadok":
		if len(parts) < 4 {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		pid, ok := parseID(parts[3])
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "bad playlist")
			return
		}
		items, err := playlistCall(b, func(ctx context.Context) ([]store.PlaylistItem, error) {
			return b.pl.Items(ctx, userID, pid)
		})
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, plErr(err))
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
		n, err := s.ReplaceUpcoming(urls)
		if err != nil {
			// ReplaceUpcoming never touches the queue when validation
			// fails, so nothing to roll back here.
			_ = b.api.AnswerCallback(q.ID, "load failed: "+truncate(err.Error(), 60))
			return
		}
		b.persistQueue(s)
		_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("loaded %d item(s)", n))
		b.renderQueue(chatID, msgID, s, -1)

	case "clear_confirm":
		b.log.Infof("[PIPELINE:queue] clear_confirm prompted for session %s by user %d", sid, userID)
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, fmt.Sprintf("⚠️ <b>Clear Upcoming Queue?</b>\n\nThis will remove all upcoming items after the currently playing item from session <code>%s</code>.", sid), queueClearConfirmKB(sid))

	case "do_clear":
		b.log.Infof("[PIPELINE:queue] do_clear executed for session %s by user %d", sid, userID)
		if err := s.ClearUpcoming(); err != nil {
			_ = b.api.AnswerCallback(q.ID, "clear failed: "+truncate(err.Error(), 60))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "upcoming queue cleared")
		b.persistQueue(s)
		b.renderQueue(chatID, msgID, s, -1)

	case "do_clear_all":
		b.log.Infof("[PIPELINE:queue] do_clear_all executed for session %s by user %d", sid, userID)
		if err := s.ClearAllExceptCurrent(); err != nil {
			_ = b.api.AnswerCallback(q.ID, "clear failed: "+truncate(err.Error(), 60))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "cleared all except current")
		b.persistQueue(s)
		b.renderQueue(chatID, msgID, s, -1)

	case "rm_menu":
		b.log.Infof("[PIPELINE:queue] rm_menu opened for session %s by user %d", sid, userID)
		_ = b.api.AnswerCallback(q.ID, "")
		items, curIdx := s.QueueSnapshot()
		_ = b.api.Edit(chatID, msgID, "❌ <b>Select an item to remove:</b>", queueRemoveMenuKB(sid, items, curIdx))

	case "rm":
		if len(parts) < 4 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return
		}
		itemID := parts[3]
		b.log.Infof("[PIPELINE:queue] remove item %s from session %s by user %d", itemID, sid, userID)
		if err := s.RemoveQueueItem(itemID); err != nil {
			_ = b.api.AnswerCallback(q.ID, "remove failed: "+truncate(err.Error(), 60))
			b.renderQueue(chatID, msgID, s, -1)
			return
		}
		_ = b.api.AnswerCallback(q.ID, "item removed")
		b.persistQueue(s)
		b.renderQueue(chatID, msgID, s, -1)

	case "itemq", "itemqset":
		b.handleQueueItemQualityMenu(chatID, userID, msgID, q, data)
	case "itemqopen":
		b.handleQueueItemQualityOpen(chatID, userID, msgID, q, data)
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown queue action")
	}
}

// queueAddText prompts for media links to append or play next.
func queueAddText(st engine.Status, mode string) string {
	head := "➕ <b>Add media to queue</b>"
	if mode == "addnext" {
		head = "⏭ <b>Play next</b>"
	}
	return fmt.Sprintf(`%s

Session <code>%s</code> — queue has <b>%d</b> item(s).

Send one or more media links (any chat message):
• YouTube watch/shorts links
• direct files (<code>.mp4 .webm .mp3 …</code>)
• several links in one message = several items

<i>%s</i>`,
		head, esc(string(st.ID)), st.QueueLength,
		map[string]string{
			"add":     "Links are appended to the end of the queue; playback is not interrupted.",
			"addnext": "The first link plays immediately after the current item, without replacing the queue.",
		}[mode])
}

// queueLoadText prompts for the playlist to load.
func queueLoadText(st engine.Status) string {
	return fmt.Sprintf("📚 <b>Load playlist</b>\n\nSession <code>%s</code> · queue has <b>%d</b> item(s).\n\nPick a saved playlist to load after the current item.", esc(string(st.ID)), st.QueueLength)
}

// queueLoadKB lists the user's playlists as load targets.
func queueLoadKB(st engine.Status, list []store.Playlist) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, p := range list {
		if p.ItemCount == 0 {
			continue
		}
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("📚 %s (%d)", truncate(p.Name, 32), p.ItemCount),
				fmt.Sprintf("q:loadpl:%s:%d", id, p.ID)),
		})
	}
	if len(rows) == 0 {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("No playable playlists", "q:noop:"+id),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Back to queue", "act:"+id+":queue"),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}
