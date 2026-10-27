package bot

// Video-quality panel: session menu, per-item overrides, format detection
// and the quality-info view.
//
// Every button here performs a real operation on the media pipeline:
//   - policies change the session's extraction ladder (download mode) or are
//     recorded as requested-and-automatic (direct browser playback, where
//     YouTube's own player controls adaptive quality — stated, never faked);
//   - Detect formats runs a real yt-dlp metadata probe (simulate only);
//   - Apply reloads the current media so the new policy takes effect;
//   - the info view separates Requested / Selected / Observed / Unavailable.

import (
	"context"
	"fmt"
	"strings"

	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// qualityPolicies is the picker order.
var qualityPolicies = []string{
	store.QualityAuto, store.QualitySaver, store.QualityBalanced,
	store.QualityHD720, store.QualityFHD1080, store.QualityBest,
}

// handleQualityCallback routes the session quality menu and its actions.
func (b *Bot) handleQualityCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	// data: act:<sid>:quality[|qset:<p>|qinfo|qformats|qfmt:<n>|qapply]
	parts := strings.SplitN(strings.TrimPrefix(data, "act:"), ":", 4)
	if len(parts) < 2 {
		_ = b.api.AnswerCallback(q.ID, "bad quality action")
		return
	}
	sid := parts[0]
	verb := parts[1]

	s := b.eng.Get(engine.SessionID(sid))
	if s == nil {
		_ = b.api.AnswerCallback(q.ID, "session not found")
		_ = b.api.Edit(chatID, msgID, "This session has ended.", mainMenuKB())
		return
	}
	st := s.Snapshot()

	switch verb {
	case "quality":
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, qualityMenuText(st), qualityMenuKB(st))
	case "qset":
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad policy")
			return
		}
		policy := engine.NormalizeQualityPolicy(parts[2])
		if err := s.SetQualityPolicy(policy); err != nil {
			_ = b.api.AnswerCallback(q.ID, "failed: "+truncate(err.Error(), 60))
			return
		}
		_ = b.api.AnswerCallback(q.ID, "requested: "+engine.QualityPolicyDisplay(policy))
		st = s.Snapshot()
		_ = b.api.Edit(chatID, msgID, qualityMenuText(st), qualityMenuKB(st))
	case "qapply":
		// Reload the current media so the policy actually reaches the
		// extraction ladder. Honest about what is happening: the media
		// restarts, playback resumes at the beginning.
		_ = b.api.AnswerCallback(q.ID, "reloading media…")
		if err := s.RestartMedia(); err != nil {
			_ = b.api.Edit(chatID, msgID, "🔴 <b>Apply failed</b>\n\n"+esc(truncate(err.Error(), 250)), qualityMenuKB(st))
			return
		}
		st = s.Snapshot()
		_ = b.api.Edit(chatID, msgID, sessionCardText(st), sessionCardKB(st))
		b.bindCard(chatID, msgID, s, st)
	case "qinfo":
		_ = b.api.AnswerCallback(q.ID, "")
		_ = b.api.Edit(chatID, msgID, b.qualityInfoText(st), qualityInfoKB(st))
	case "qformats":
		_ = b.api.AnswerCallback(q.ID, "probing source…")
		b.showSourceFormats(chatID, msgID, q.ID, s, st)
	case "qfmt":
		// A detected height becomes a capped policy: choosing "1080" maps
		// to the ≤1080p policy, "360" to Data Saver, and heights above
		// 1080 to Best Available. The mapping is stated on the button.
		if len(parts) < 3 {
			_ = b.api.AnswerCallback(q.ID, "bad format")
			return
		}
		var height int
		if _, err := fmt.Sscanf(parts[2], "%d", &height); err != nil || height <= 0 {
			_ = b.api.AnswerCallback(q.ID, "bad height")
			return
		}
		policy := policyForHeight(height)
		if err := s.SetQualityPolicy(policy); err != nil {
			_ = b.api.AnswerCallback(q.ID, "failed: "+truncate(err.Error(), 60))
			return
		}
		_ = b.api.AnswerCallback(q.ID, fmt.Sprintf("capped at ≤%dp", height))
		st = s.Snapshot()
		_ = b.api.Edit(chatID, msgID, qualityMenuText(st), qualityMenuKB(st))
	case "qmode":
		// The honest escape hatch for direct YouTube: switch the session to
		// Download & Stream so the policy can actually be enforced.
		_ = b.api.AnswerCallback(q.ID, "switching to Download & Stream…")
		if err := s.SetPlaybackMode(media.ModeDownload); err != nil {
			_ = b.api.Edit(chatID, msgID, "🔴 Switch failed: "+esc(truncate(err.Error(), 200)), qualityMenuKB(st))
			return
		}
		if err := s.RestartMedia(); err != nil {
			st = s.Snapshot()
			_ = b.api.Edit(chatID, msgID, qualityMenuText(st), qualityMenuKB(st))
			return
		}
		st = s.Snapshot()
		_ = b.api.Edit(chatID, msgID, qualityMenuText(st), qualityMenuKB(st))
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown quality action")
	}
}

// policyForHeight maps a detected height to the policy that caps there.
func policyForHeight(height int) string {
	switch {
	case height <= 360:
		return store.QualitySaver
	case height <= 720:
		return store.QualityBalanced
	case height <= 1080:
		return store.QualityFHD1080
	default:
		return store.QualityBest
	}
}

// qualityMenuText renders the session quality menu.
func qualityMenuText(st engine.Status) string {
	var sb strings.Builder
	sb.WriteString("🎞️ <b>Video Quality</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code> · %s mode\n\n", esc(string(st.ID)), esc(engine.PlaybackModeDisplay(st.PlayMode))))
	sb.WriteString(fmt.Sprintf("🎛 Requested: <b>%s</b>\n", esc(engine.QualityPolicyDisplay(st.QualityPolicy))))
	if st.SelectedFormat != "" {
		sb.WriteString(fmt.Sprintf("📦 Selected: <b>%s</b>\n", esc(st.SelectedFormat)))
	} else {
		sb.WriteString("📦 Selected: <i>not applicable (no extraction ladder)</i>\n")
	}
	if obs := st.Observed.Describe(); obs != "" {
		sb.WriteString(fmt.Sprintf("📺 Observed: <b>%s</b> <i>(live player telemetry)</i>\n", esc(obs)))
	} else {
		sb.WriteString("📺 Observed: <i>waiting for the player to report…</i>\n")
	}
	if engine.NormalizePlaybackMode(st.PlayMode) == engine.MediaModeBrowser {
		sb.WriteString("\n<i>Direct playback: YouTube's own player controls adaptive quality; a fixed policy cannot be forced in this mode. Pick a policy and Apply, or switch to Download &amp; Stream to enforce a cap.</i>")
	} else {
		sb.WriteString("\n<i>Apply reloads the current media with the new policy. The next queued item uses it automatically.</i>")
	}
	return sb.String()
}

// qualityMenuKB renders the policy buttons and the real actions.
func qualityMenuKB(st engine.Status) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 2)
	for _, p := range qualityPolicies {
		label := engine.QualityPolicyDisplay(p)
		if engine.NormalizeQualityPolicy(st.QualityPolicy) == engine.NormalizeQualityPolicy(p) {
			label = "✅ " + label
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, "act:"+id+":qset:"+p))
		if len(row) == 2 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 2)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("🔍 Detect available formats", "act:"+id+":qformats"),
		tgbotapi.NewInlineKeyboardButtonData("📊 Quality & playback info", "act:"+id+":qinfo"),
	})
	action := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("♻️ Apply to current media", "act:"+id+":qapply"),
	}
	if engine.NormalizePlaybackMode(st.PlayMode) == engine.MediaModeBrowser {
		action = append(action, tgbotapi.NewInlineKeyboardButtonData("⬇️ Switch to Download & Stream", "act:"+id+":qmode"))
	}
	rows = append(rows, action)
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Session controls", "ses:"+id),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// showSourceFormats probes the source (yt-dlp -J, no download) and lists the
// real ladder. Each detected height becomes a cap button, so the dynamic
// choices map onto policies the pipeline can actually honor.
func (b *Bot) showSourceFormats(chatID int64, msgID int, cbID string, s *engine.Session, st engine.Status) {
	mgr := b.mediaMgr()
	if mgr == nil {
		_ = b.api.AnswerCallback(cbID, "download & stream is disabled")
		_ = b.api.Edit(chatID, msgID, qualityMenuText(st), qualityMenuKB(st))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), formatProbeTimeout)
	defer cancel()
	info, err := mgr.Inspect(ctx, st.MediaURL)
	if err != nil {
		_ = b.api.AnswerCallback(cbID, "probe failed")
		_ = b.api.Edit(chatID, msgID, "🔴 <b>Format probe failed</b>\n\n"+esc(truncate(err.Error(), 300))+"\n\n<i>Direct pages and some sources have no inspectable ladder; Download &amp; Stream still works through the policy ladder.</i>", qualityMenuKB(st))
		return
	}
	_ = b.api.AnswerCallback(cbID, "formats detected")
	_ = b.api.Edit(chatID, msgID, sourceFormatsText(st, info), sourceFormatsKB(st, info))
}

// formatProbeTimeout bounds one yt-dlp -J probe.
const formatProbeTimeout = 30 * time.Second

// sourceFormatsText renders the detected ladder with honest metadata.
func sourceFormatsText(st engine.Status, info *media.SourceInfo) string {
	var sb strings.Builder
	sb.WriteString("🔍 <b>Available formats</b>\n\n")
	if info.Title != "" {
		sb.WriteString(fmt.Sprintf("<i>%s</i>\n", esc(truncate(info.Title, 70))))
	}
	if info.Duration > 0 {
		sb.WriteString(fmt.Sprintf("⏱ Duration: %s\n", fmtClock(info.Duration)))
	}
	if info.IsLive {
		sb.WriteString("🔴 <i>Live source: renditions may change mid-stream.</i>\n")
	}
	sb.WriteString("\n")
	if info.SingleFile {
		sb.WriteString("This is a direct media file: the browser plays exactly what the server serves; there is no ladder to choose from. The quality policies still steer Download &amp; Stream re-fetches.\n")
		return sb.String()
	}
	heights := media.DistinctHeights(info.Formats)
	if len(heights) == 0 {
		sb.WriteString("No video renditions reported (audio-only source?).\n")
		return sb.String()
	}
	sb.WriteString("Detected video heights (best available at each rung):\n")
	for _, h := range heights {
		f := media.BestAtOrBelow(info.Formats, h)
		line := fmt.Sprintf("• <b>%dp</b>", h)
		if f.VCodec != "" && f.VCodec != "none" {
			line += " · " + esc(media.CodecShort(f.VCodec))
		}
		if f.FPS > 0 {
			line += fmt.Sprintf(" %.4gfps", f.FPS)
		}
		if size := f.SizeBest(); size > 0 {
			line += fmt.Sprintf(" · ~%.0f MB", float64(size)/(1024*1024))
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("\n<i>Pick a height to cap the policy at it; the extractor then chooses the best compatible format at or below it. Separate video/audio streams are merged with ffmpeg.</i>")
	return sb.String()
}

// sourceFormatsKB offers one cap button per detected height.
func sourceFormatsKB(st engine.Status, info *media.SourceInfo) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 3)
	for _, h := range media.DistinctHeights(info.Formats) {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("≤%dp", h), fmt.Sprintf("act:%s:qfmt:%d", id, h)))
		if len(row) == 3 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 3)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("🎞️ Back to quality", "act:"+id+":quality"),
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Session", "ses:"+id),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// qualityInfoText renders the 📊 Quality & Playback Info view. Requested,
// Selected and Observed are labelled separately; anything the pipeline
// cannot measure is stated as unavailable instead of guessed.
func (b *Bot) qualityInfoText(st engine.Status) string {
	var sb strings.Builder
	sb.WriteString("📊 <b>Quality &amp; Playback Info</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code>\n\n", esc(string(st.ID))))

	sb.WriteString(fmt.Sprintf("🎛 Requested policy: <b>%s</b>\n", esc(engine.QualityPolicyDisplay(st.QualityPolicy))))
	sb.WriteString(fmt.Sprintf("%s Playback mode: <b>%s</b>\n", engine.PlaybackModeEmoji(st.PlayMode), esc(engine.PlaybackModeDisplay(st.PlayMode))))

	if st.SelectedFormat != "" {
		sb.WriteString(fmt.Sprintf("📦 Selected format: <b>%s</b>\n", esc(st.SelectedFormat)))
	} else {
		sb.WriteString("📦 Selected format: <i>unavailable</i> — direct playback has no extraction ladder\n")
	}

	if obs := st.Observed.Describe(); obs != "" {
		label := "📺 Observed resolution"
		if st.Observed.Height > 0 {
			label = "📺 Observed (player):"
		}
		sb.WriteString(fmt.Sprintf("%s <b>%s</b>\n", label, esc(obs)))
	} else {
		sb.WriteString("📺 Observed resolution: <i>unavailable</i> — the player has not reported a size yet\n")
	}

	if st.Title != "" {
		sb.WriteString(fmt.Sprintf("🎬 Title: <b>%s</b>\n", esc(truncate(st.Title, 60))))
	}
	if st.Dur > 0 {
		sb.WriteString(fmt.Sprintf("⏱ Position: %s / %s\n", fmtClock(st.Cur), fmtClock(st.Dur)))
	}
	if st.LocalFile.Bytes > 0 {
		sb.WriteString(fmt.Sprintf("💾 File: %.1f MiB (actual)\n", float64(st.LocalFile.Bytes)/(1024*1024)))
	}
	if st.AudioTrack != "" {
		sb.WriteString(fmt.Sprintf("🎵 Audio track: <b>%s</b>\n", esc(st.AudioTrack)))
	}
	if st.Error != "" {
		sb.WriteString("\n🔴 <b>Playback error:</b> " + esc(truncate(st.Error, 250)) + "\n")
	}

	if engine.NormalizePlaybackMode(st.PlayMode) == engine.MediaModeBrowser {
		sb.WriteString("\n<i>Direct mode: the observed values are what YouTube's player chose adaptively; the source-video resolution and the Meet screen share are separate measurements.</i>")
	} else {
		sb.WriteString("\n<i>Download mode: the selected format was extracted for you; the Meet share re-encodes it, so share quality is a separate measurement.</i>")
	}
	return sb.String()
}

// qualityInfoKB is the info view's keyboard.
func qualityInfoKB(st engine.Status) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🎞️ Video quality", "act:"+id+":quality"),
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "act:"+id+":qinfo"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Session controls", "ses:"+id),
			tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
		},
	}}
}

// handleQueueItemQualityMenu lists queue items for a per-item override.
func (b *Bot) handleQueueItemQualityMenu(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.Split(data, ":") // q:itemq:<sid> | q:itemqset:<sid>:<itemID>:<p>
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
	if len(parts) >= 5 && parts[1] == "itemqset" {
		itemID, policy := parts[3], parts[4]
		if err := s.SetQueueItemQuality(itemID, policy); err != nil {
			_ = b.api.AnswerCallback(q.ID, "item not found (queue changed)")
		} else {
			_ = b.api.AnswerCallback(q.ID, "item will use "+engine.QualityPolicyDisplay(policy))
			b.persistQueue(s)
		}
		b.showQueueItemQualityMenu(chatID, msgID, s)
		return
	}
	_ = b.api.AnswerCallback(q.ID, "")
	b.showQueueItemQualityMenu(chatID, msgID, s)
}

// showQueueItemQualityMenu renders the per-item picker.
func (b *Bot) showQueueItemQualityMenu(chatID int64, msgID int, s *engine.Session) {
	st := s.Snapshot()
	items, curIdx := s.QueueSnapshot()
	var sb strings.Builder
	sb.WriteString("🎞️ <b>Per-item quality</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code> — pick an item, then a policy. An explicit override wins while that item plays; other items fall back to the session policy.\n\n", esc(string(st.ID))))
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, it := range items {
		marker := "·"
		if i == curIdx {
			marker = "▶️"
		}
		label := fmt.Sprintf("%s %d. %s", marker, i+1, truncate(itemTitleForQueue(it), 26))
		if it.Quality != "" {
			label += " 🎞️"
		}
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(label, "q:itemqopen:"+string(st.ID)+":"+it.ID),
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

// itemTitleForQueue is the display title of a queue item.
func itemTitleForQueue(it engine.QueueItem) string {
	if strings.TrimSpace(it.Title) != "" {
		return it.Title
	}
	return it.URL
}

// handleQueueItemQualityOpen shows the policy picker for one item.
func (b *Bot) handleQueueItemQualityOpen(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.Split(data, ":") // q:itemqopen:<sid>:<itemID>
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
	current := s.QueueItemQuality(itemID)
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 2)
	for _, p := range qualityPolicies {
		label := engine.QualityPolicyDisplay(p)
		if p == current || (current == "" && p == store.QualityAuto) {
			label = "✅ " + label
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("q:itemqset:%s:%s:%s", sid, itemID, p)))
		if len(row) == 2 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 2)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Item list", "q:itemq:"+sid),
		tgbotapi.NewInlineKeyboardButtonData("📋 Queue", "act:"+sid+":queue"),
	})
	_ = b.api.AnswerCallback(q.ID, "")
	_ = b.api.Edit(chatID, msgID, "🎞️ <b>Quality for this item</b>\n\nThe override applies while the item plays; other items fall back to the session policy.", &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}
