package bot

// 💬 Subtitles & Captions panel: session menu, language detection, enable/
// disable actions and the honest status view.
//
// Source-aware behavior, all real operations:
//   - Download & Stream: the language steers the yt-dlp subtitle fetch; the
//     artifact is verified (WebVTT), served over loopback, attached as a
//     native <track>, and the player's textTracks state is read back —
//     "enabled" is what the player reports.
//   - Direct YouTube: the captions-module getOption/setOption API is issued
//     best-effort; the probe reads the module state back, and an unconfirmed
//     request is labelled "issued", never "enabled".
//   - Generic pages: reported as source-controlled.
//
// Settings persist the user default (policy + preferred language, stored
// independently of the audio language); per-queue-item overrides win.

import (
	"context"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// captionPolicies is the picker order (off last so it reads as "disable").
var captionPolicies = []string{
	store.CaptionAuto, store.CaptionPreferred, store.CaptionOriginal, store.CaptionOff,
}

// commonCaptionLangs is the language offer when no source probe is available
// (a preference, not an availability claim — the picker says so).
var commonCaptionLangs = []string{"en", "fa", "es", "ar", "fr", "de", "ja", "ko", "pt", "ru"}

// handleCaptionsCallback routes the session captions menu.
// data: act:<sid>:captions | cappol:<p> | caplang:<code> | capon | capoff |
// capdetect | capapply | capinfo
func (b *Bot) handleCaptionsCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.SplitN(strings.TrimPrefix(data, "act:"), ":", 4)
	if len(parts) < 2 {
		_ = b.api.AnswerCallback(q.ID, "bad caption action")
		return
	}
	sid, verb := parts[0], parts[1]

	s := b.eng.Get(engine.SessionID(sid))
	if s == nil {
		_ = b.api.AnswerCallback(q.ID, "session not found")
		_ = b.api.Edit(chatID, msgID, "This session has ended.", mainMenuKB())
		return
	}
	st := s.Snapshot()

	switch verb {
	case "captions":
		_ = b.api.AnswerCallback(q.ID, "")
		b.setCardView(sid, chatID, msgID, "captions")
		if err := b.api.Edit(chatID, msgID, captionsMenuText(st), captionsMenuKB(st)); err != nil {
			b.log.Warnf("[PIPELINE:bot] failed to open captions menu session=%s: %v", sid, err)
		}
	case "cappol":
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad policy")
			return
		}
		policy := engine.NormalizeCaptionPolicy(parts[2])
		if err := s.SetCaptionPolicy(policy); err != nil {
			_ = b.api.AnswerCallback(q.ID, "failed: "+truncate(err.Error(), 60))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "captions: "+engine.CaptionPolicyDisplay(policy))
		st = s.Snapshot()
		b.setCardView(sid, chatID, msgID, "captions")
		if err := b.api.Edit(chatID, msgID, captionsMenuText(st), captionsMenuKB(st)); err != nil {
			b.log.Warnf("[PIPELINE:bot] failed to update captions menu session=%s: %v", sid, err)
		}
	case "caplang":
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad language")
			return
		}
		lang := engine.NormalizeCaptionLang(parts[2])
		if lang == "" {
			_ = b.api.AnswerCallback(q.ID, "bad language tag")
			return
		}
		if err := s.SetCaptionLang(lang); err != nil {
			_ = b.api.AnswerCallback(q.ID, "failed: "+truncate(err.Error(), 60))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "preferred subtitle language: "+lang)
		st = s.Snapshot()
		b.setCardView(sid, chatID, msgID, "captions")
		if err := b.api.Edit(chatID, msgID, captionsMenuText(st), captionsMenuKB(st)); err != nil {
			b.log.Warnf("[PIPELINE:bot] failed to update captions menu session=%s: %v", sid, err)
		}
	case "capon", "capoff":
		// Real player operation with read-back; the note distinguishes
		// "issued" from "confirmed".
		confirmed, note := s.ToggleCaptions(verb == "capon", st.CaptionLang)
		if confirmed {
			_ = b.api.AnswerCallback(q.ID, note)
		} else {
			_ = b.api.AnswerCallback(q.ID, truncate(note, 60))
		}
		st = s.Snapshot()
		b.setCardView(sid, chatID, msgID, "captions")
		if err := b.api.Edit(chatID, msgID, captionsMenuText(st), captionsMenuKB(st)); err != nil {
			b.log.Warnf("[PIPELINE:bot] failed to update captions menu session=%s: %v", sid, err)
		}
	case "capdetect":
		_ = b.api.AnswerCallback(q.ID, "probing subtitles…")
		b.setCardView(sid, chatID, msgID, "captions")
		b.showCaptionTracks(chatID, msgID, s)
	case "capapply":
		// Reload the current media so the caption policy/language reaches
		// the extraction pipeline. Failures are reported honestly.
		_ = b.api.AnswerCallback(q.ID, "reloading media…")
		if err := s.RestartMedia(); err != nil {
			b.setCardView(sid, chatID, msgID, "captions")
			_ = b.api.Edit(chatID, msgID, "🔴 <b>Apply failed</b>\n\n"+esc(truncate(err.Error(), 250)), captionsMenuKB(st))
			return
		}
		st = s.Snapshot()
		b.setCardView(sid, chatID, msgID, "captions")
		_ = b.api.Edit(chatID, msgID, captionsMenuText(st), captionsMenuKB(st))
	case "capinfo":
		_ = b.api.AnswerCallback(q.ID, "")
		b.setCardView(sid, chatID, msgID, "captions")
		if err := b.api.Edit(chatID, msgID, b.captionInfoText(st), captionsMenuKB(st)); err != nil {
			b.log.Warnf("[PIPELINE:bot] failed to show captions info session=%s: %v", sid, err)
		}
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown caption action")
	}
}

// captionsMenuText renders the 💬 menu with the real state.
func captionsMenuText(st engine.Status) string {
	var sb strings.Builder
	sb.WriteString("💬 <b>Subtitles &amp; Captions</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code> · %s mode\n\n", esc(string(st.ID)), esc(engine.PlaybackModeDisplay(st.PlayMode))))
	sb.WriteString(fmt.Sprintf("🎛 Requested: <b>%s</b>", esc(engine.CaptionPolicyDisplay(st.CaptionPolicy))))
	if st.CaptionLang != "" {
		sb.WriteString(fmt.Sprintf(" · language <b>%s</b>", esc(st.CaptionLang)))
	}
	sb.WriteString("\n")

	c := st.Caption
	switch {
	case c.Source == "source-controlled":
		sb.WriteString("📺 Player: <i>source-controlled</i> — this page's own player decides caption availability\n")
	case c.Source != "":
		state := "off"
		if c.Enabled {
			state = "ON"
		}
		sb.WriteString(fmt.Sprintf("📺 Player: <b>%s</b> (%s)", state, esc(c.Source)))
		if c.Lang != "" {
			sb.WriteString(fmt.Sprintf(" · language <b>%s</b>", esc(c.Lang)))
		}
		if !c.Loaded && c.Enabled {
			sb.WriteString(" · <i>track not loaded yet</i>")
		}
		sb.WriteString("\n")
		if len(c.Tracks) > 0 {
			sb.WriteString(fmt.Sprintf("📝 Player tracks: <code>%s</code>\n", esc(strings.Join(c.Tracks, ", "))))
		}
	default:
		sb.WriteString("📺 Player: <i>nothing observable yet</i>\n")
	}
	if c.Note != "" {
		sb.WriteString(fmt.Sprintf("ℹ️ <i>%s</i>\n", esc(truncate(c.Note, 120))))
	}
	if st.LocalFile.Subtitle != "" {
		sb.WriteString(fmt.Sprintf("📦 Attached: <b>%s</b>\n", esc(st.LocalFile.Subtitle)))
	}

	if engine.NormalizePlaybackMode(st.PlayMode) == engine.MediaModeBrowser {
		sb.WriteString("\n<i>Direct playback: YouTube's captions module is used best-effort; a language the module does not expose cannot be applied here. Switch to Download &amp; Stream to attach a real track.</i>")
	} else {
		sb.WriteString("\n<i>Download &amp; Stream: the requested language is fetched with the media (manual track first, auto-generated as labelled fallback), converted to WebVTT and attached to the player.</i>")
	}
	return sb.String()
}

// captionsMenuKB renders the menu: policies, enable/disable, detection.
func captionsMenuKB(st engine.Status) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 2)
	for _, p := range captionPolicies {
		label := engine.CaptionPolicyDisplay(p)
		if engine.NormalizeCaptionPolicy(st.CaptionPolicy) == engine.NormalizeCaptionPolicy(p) {
			label = "✅ " + label
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, "act:"+id+":cappol:"+p))
		if len(row) == 2 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 2)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("✅ Enable captions", "act:"+id+":capon"),
		tgbotapi.NewInlineKeyboardButtonData("🚫 Disable captions", "act:"+id+":capoff"),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("🔍 Detect available subtitles", "act:"+id+":capdetect"),
		tgbotapi.NewInlineKeyboardButtonData("🌐 Preferred language", "act:"+id+":caplangs"),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("♻️ Apply to current media", "act:"+id+":capapply"),
		tgbotapi.NewInlineKeyboardButtonData("📊 Caption status", "act:"+id+":capinfo"),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Session controls", "ses:"+id),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// showCaptionTracks probes the source (yt-dlp -J) for caption tracks and
// renders them: manual first, automatic labelled, per-language buttons.
func (b *Bot) showCaptionTracks(chatID int64, msgID int, s *engine.Session) {
	st := s.Snapshot()
	mgr := b.mediaMgr()
	if mgr == nil {
		_ = b.api.Edit(chatID, msgID,
			"💬 <b>Available subtitles</b>\n\nThe source probe needs Download &amp; Stream (yt-dlp), which is disabled on this installation. Direct YouTube caption controls remain available through the captions module.",
			captionsMenuKB(st))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), formatProbeTimeout)
	defer cancel()
	info, err := mgr.Inspect(ctx, st.MediaURL)
	if err != nil {
		_ = b.api.Edit(chatID, msgID,
			"🔴 <b>Subtitle probe failed</b>\n\n"+esc(truncate(err.Error(), 280))+"\n\n<i>Sources without extractor metadata expose no inspectable tracks; the direct-player caption controls stay available.</i>",
			captionsMenuKB(st))
		return
	}
	_ = b.api.Edit(chatID, msgID, captionTracksText(st, info), captionTracksKB(st, info))
}

// captionTracksText renders the discovered tracks honestly.
func captionTracksText(st engine.Status, info *media.SourceInfo) string {
	var sb strings.Builder
	sb.WriteString("📝 <b>Available subtitles</b>\n\n")
	if info.Title != "" {
		sb.WriteString(fmt.Sprintf("<i>%s</i>\n\n", esc(truncate(info.Title, 70))))
	}
	if len(info.Subtitles) == 0 {
		sb.WriteString("No caption tracks were reported for this source.\n\n" +
			"<i>Not every video has manual or automatic captions; none can be offered that do not exist.</i>")
		return sb.String()
	}
	manual, auto := 0, 0
	for _, tr := range info.Subtitles {
		if tr.Auto {
			auto++
		} else {
			manual++
		}
	}
	sb.WriteString(fmt.Sprintf("%d manual · %d automatic (labelled, never conflated)\n\n", manual, auto))
	for _, tr := range info.Subtitles {
		label := tr.Lang
		if tr.Auto {
			label += " <i>(auto-generated)</i>"
		}
		if tr.Ext != "" {
			label += " · " + esc(tr.Ext)
		}
		sb.WriteString("• " + label + "\n")
	}
	sb.WriteString("\n<i>Pick a language to request it. Auto-generated tracks are exactly that — no translation is fabricated.</i>")
	return sb.String()
}

// captionTracksKB offers one language button per discovered track.
func captionTracksKB(st engine.Status, info *media.SourceInfo) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 3)
	for _, tr := range info.Subtitles {
		label := tr.Lang
		if tr.Auto {
			label = "🔤 " + label + "*"
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label,
			fmt.Sprintf("act:%s:caplang:%s", id, tr.Lang)))
		if len(row) == 3 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 3)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("💬 Back to captions", "act:"+id+":captions"),
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Session", "ses:"+id),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// captionInfoText renders the detailed caption status with the requested /
// available / selected / loaded / enabled distinctions.
func (b *Bot) captionInfoText(st engine.Status) string {
	var sb strings.Builder
	sb.WriteString("📊 <b>Caption status</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code>\n\n", esc(string(st.ID))))
	sb.WriteString(fmt.Sprintf("🎛 Requested: <b>%s</b>", esc(engine.CaptionPolicyDisplay(st.CaptionPolicy))))
	if st.CaptionLang != "" {
		sb.WriteString(fmt.Sprintf(" · language <b>%s</b>", esc(st.CaptionLang)))
	}
	sb.WriteString("\n")

	c := st.Caption
	available := "unavailable"
	if c.Available {
		available = fmt.Sprintf("yes (%s)", strings.Join(c.Tracks, ", "))
	}
	sb.WriteString(fmt.Sprintf("🔎 Available: %s\n", esc(available)))
	if c.Source == "" {
		sb.WriteString("📦 Selected: <i>unavailable</i> — nothing observable on this media yet\n")
	} else {
		sb.WriteString(fmt.Sprintf("📦 Selected: <b>%s</b> (%s)\n", esc(c.Lang), esc(c.Source)))
		loaded := "no"
		if c.Loaded {
			loaded = "yes"
		}
		sb.WriteString(fmt.Sprintf("📥 Loaded: %s\n", loaded))
		enabled := "no"
		if c.Enabled {
			enabled = "yes — the player reports the track showing"
		}
		sb.WriteString(fmt.Sprintf("✅ Enabled: %s\n", enabled))
	}
	if c.Note != "" {
		sb.WriteString(fmt.Sprintf("ℹ️ <i>%s</i>\n", esc(truncate(c.Note, 160))))
	}
	if st.LocalFile.Subtitle != "" {
		sb.WriteString(fmt.Sprintf("💾 Artifact: <b>%s</b> served over loopback\n", esc(st.LocalFile.Subtitle)))
	}
	sb.WriteString("\n<i>A successful selection is not a visible caption: Enabled is only set when the player itself reports the track showing in the shared tab.</i>")
	return sb.String()
}

// handleCaptionLangsMenu offers the language picker: detected tracks when a
// probe is possible, otherwise the common-languages preference list (clearly
// labelled as preferences, not availability).
func (b *Bot) handleCaptionLangsMenu(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.SplitN(strings.TrimPrefix(data, "act:"), ":", 3)
	if len(parts) < 2 {
		_ = b.api.AnswerCallback(q.ID, "bad request")
		return
	}
	sid := parts[0]
	s := b.eng.Get(engine.SessionID(sid))
	if s == nil {
		_ = b.api.AnswerCallback(q.ID, "session not found")
		return
	}
	st := s.Snapshot()
	if mgr := b.mediaMgr(); mgr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), formatProbeTimeout)
		defer cancel()
		if info, err := mgr.Inspect(ctx, st.MediaURL); err == nil && len(info.Subtitles) > 0 {
			_ = b.api.AnswerCallback(q.ID, "tracks detected")
			_ = b.api.Edit(chatID, msgID, captionTracksText(st, info), captionTracksKB(st, info))
			return
		}
	}
	// Preference list fallback.
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 3)
	for _, lang := range commonCaptionLangs {
		label := lang
		if st.CaptionLang == lang {
			label = "✅ " + label
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("act:%s:caplang:%s", sid, lang)))
		if len(row) == 3 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 3)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("💬 Back to captions", "act:"+sid+":captions"),
	})
	_ = b.api.AnswerCallback(q.ID, "")
	_ = b.api.Edit(chatID, msgID,
		"🌐 <b>Preferred subtitle language</b>\n\nSaved with your settings and applied to sessions you launch. These are <i>preferences</i> — availability depends on the source and is detected per media item.",
		&tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// handleQueueItemCaptionMenu lists queue items for per-item subtitle
// overrides (q:itemcap:<sid> | q:itemcapset:<sid>:<itemID>:<lang|off>).
func (b *Bot) handleQueueItemCaptionMenu(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.Split(data, ":")
	if len(parts) < 3 {
		_ = b.api.AnswerCallback(q.ID, "bad request")
		return
	}
	sid := parts[2]
	s := b.eng.Get(engine.SessionID(sid))
	if s == nil {
		_ = b.api.AnswerCallback(q.ID, "session not found")
		_ = b.api.Edit(chatID, msgID, "This session has ended.", mainMenuKB())
		return
	}
	if len(parts) >= 5 && parts[1] == "itemcapset" {
		itemID, lang := parts[3], parts[4]
		if err := s.SetQueueItemCaption(itemID, lang); err != nil {
			_ = b.api.AnswerCallback(q.ID, "item not found (queue changed)")
			return
		}
		_ = b.api.AnswerCallback(q.ID, "item captions: "+lang)
		b.persistQueue(s)
		b.showQueueItemCaptionMenu(chatID, msgID, s)
		return
	}
	_ = b.api.AnswerCallback(q.ID, "")
	b.showQueueItemCaptionMenu(chatID, msgID, s)
}

// showQueueItemCaptionMenu renders the item list with caption overrides.
func (b *Bot) showQueueItemCaptionMenu(chatID int64, msgID int, s *engine.Session) {
	st := s.Snapshot()
	items, curIdx := s.QueueSnapshot()
	var sb strings.Builder
	sb.WriteString("💬 <b>Per-item subtitles</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code> — pick an item, then a language. An explicit override applies while that item plays; other items follow the session policy.\n\n", esc(string(st.ID))))
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, it := range items {
		marker := "·"
		if i == curIdx {
			marker = "▶️"
		}
		label := fmt.Sprintf("%s %d. %s", marker, i+1, truncate(itemTitleForQueue(it), 26))
		if it.CaptionLang != "" {
			label += " 💬"
		}
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(label, "q:itemcapopen:"+string(st.ID)+":"+it.ID),
		})
		if len(rows) >= 8 {
			break
		}
	}
	if len(rows) == 0 {
		sb.WriteString("The queue is empty.\n")
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Back to queue", "act:"+string(st.ID)+":queue"),
	})
	_ = b.api.Edit(chatID, msgID, sb.String(), &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// handleQueueItemCaptionOpen shows the language picker for one item.
func (b *Bot) handleQueueItemCaptionOpen(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.Split(data, ":") // q:itemcapopen:<sid>:<itemID>
	if len(parts) < 4 {
		_ = b.api.AnswerCallback(q.ID, "bad request")
		return
	}
	sid, itemID := parts[2], parts[3]
	s := b.eng.Get(engine.SessionID(sid))
	if s == nil {
		_ = b.api.AnswerCallback(q.ID, "session not found")
		return
	}
	current := s.QueueItemCaption(itemID)
	choices := append([]string{"off"}, commonCaptionLangs...)
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 3)
	for _, lang := range choices {
		label := lang
		if lang == "off" {
			label = "🚫 off (session policy)"
		}
		if current == lang {
			label = "✅ " + label
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label,
			fmt.Sprintf("q:itemcapset:%s:%s:%s", sid, itemID, lang)))
		if len(row) == 3 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 3)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Item list", "q:itemcap:"+sid),
		tgbotapi.NewInlineKeyboardButtonData("📋 Queue", "act:"+sid+":queue"),
	})
	_ = b.api.AnswerCallback(q.ID, "")
	_ = b.api.Edit(chatID, msgID,
		"💬 <b>Subtitles for this item</b>\n\nThe override applies while the item plays; other items follow the session policy.",
		&tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}
