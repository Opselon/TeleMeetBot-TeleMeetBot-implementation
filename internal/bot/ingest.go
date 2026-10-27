package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// Telegram file ingestion. A forwarded audio/video/document never arrives as
// text: handleMessage would otherwise only see m.Text and the file would be
// silently dropped. The file comes down through the Bot API (the same client
// the bot already authenticates with), is validated against the size limit
// and free disk space, lands inside the media manager's directory and is
// registered with the loopback file server so the media tab can play it from
// an http(s) origin — exactly like a downloaded file.

// tgDefaultCloudMaxBytes is the Bot API ceiling for getFile+download: Telegram only serves
// a file through the cloud API when it is smaller than this. Bigger files must be
// refused before any byte is moved rather than failing mid-download. When a local Bot API
// server is configured, this can be extended up to 2000 MiB (2 GiB).
const tgDefaultCloudMaxBytes = 20 * 1024 * 1024 // 20 MB

func (b *Bot) tgMaxBytes() int64 {
	if b.cfg != nil && b.cfg.Telegram.MaxFileSizeMB > 0 {
		return int64(b.cfg.Telegram.MaxFileSizeMB) * 1024 * 1024
	}
	return tgDefaultCloudMaxBytes
}

// telegramKind decides whether a Telegram attachment is presentable media and
// picks its media kind. MIME is authoritative when Telegram sends one; the
// extension is the fallback. Documents that are neither audio nor video are
// rejected: the bot presents media, it does not relay arbitrary files, and a
// PDF must not reach the direct-media player.
func telegramKind(fileName, mimeType string) (engine.MediaKind, bool) {
	mime := strings.ToLower(strings.TrimSpace(mimeType))
	file := strings.ToLower(filepath.Ext(fileName))

	if strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/") {
		return engine.MediaDirect, true
	}
	switch mime {
	case "application/ogg", "application/x-matroska", "":
		// Possibly media; check the extension below.
	default:
		return "", false // a document that is not media (pdf, zip, apk, ...)
	}

	switch strings.TrimPrefix(file, ".") {
	case "mp3", "m4a", "aac", "flac", "wav", "opus", "oga", "ogg",
		"mp4", "m4v", "webm", "mkv", "mov", "ogv", "avi":
		return engine.MediaDirect, true
	}
	return "", false
}

// ingestTelegramFile downloads a Telegram attachment into the media manager's
// directory and returns the loopback URL the session should play plus its
// kind. id is the owning session/handle so TTL cleanup treats the file like
// any other download and the file is deleted when the session ends.
//
// Size and disk-space checks run before any byte is moved. The signed Bot API
// download URL is derived from getFile and never logged.
func (b *Bot) ingestTelegramFile(ctx context.Context, userID int64, m *tgbotapi.Message, id string) (string, engine.MediaKind, error) {
	fileID, fileName, mime, size := tgAttachment(m)
	if fileID == "" {
		return "", "", fmt.Errorf("message has no downloadable media")
	}

	kind, ok := telegramKind(fileName, mime)
	if !ok {
		return "", "", fmt.Errorf("unsupported file type %q (%s): only audio and video can be presented", fileName, mime)
	}

	limit := b.tgMaxBytes()
	if size > limit {
		return "", "", fmt.Errorf("file is %s; Telegram only serves files under %s to bots. (Note: media must be retrieved from Telegram so Chromium can stream it in the meeting; no upload occurs). Tip: Send a direct video/web link instead, or configure a local Bot API server",
			humanBytesLocal(size), humanBytesLocal(limit))
	}
	if max := b.maxDownloadBytes(); max > 0 && size > 0 && size > max {
		return "", "", fmt.Errorf("file is %s; the configured download limit is %s", humanBytesLocal(size), humanBytesLocal(max))
	}

	mgr := b.mediaMgr()
	if mgr == nil || !mgr.Enabled() {
		return "", "", fmt.Errorf("download & stream is disabled; telegram files cannot be stored (enable download.enabled)")
	}

	// The file must land inside the managed directory so the janitor owns it.
	dir := mgr.Opts().Dir
	if free := diskFreeBytes(dir); free > 0 && size > 0 && size > free {
		return "", "", fmt.Errorf("not enough disk space: %s free in %s", humanBytesLocal(free), dir)
	}

	dest := filepath.Join(dir, sanitizeTgName(id, fileName, mime))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", "", fmt.Errorf("create download dir: %w", err)
	}
	if err := copyTgFile(ctx, b.api, fileID, dest, size, limit); err != nil {
		_ = os.Remove(dest)
		return "", "", err
	}
	if err := validateTgMedia(dest, b.maxDownloadBytes()); err != nil {
		_ = os.Remove(dest)
		return "", "", err
	}

	info, _ := os.Stat(dest)
	var bytes int64
	if info != nil {
		bytes = info.Size()
	}
	url, err := mgr.RegisterLocal(id, dest, "telegram:"+fileID, mime, bytes)
	if err != nil {
		_ = os.Remove(dest)
		return "", "", err
	}
	// Lease the freshly registered file to the user's flow: an ingested file
	// is useless if the janitor reaps it before the user picks a session. The
	// pin moves to the session at adoption, and is dropped on discard.
	mgr.Lease(id, pendingOwner(userID))
	b.log.Infof("[PIPELINE:ingest] user %d: registered telegram file %q (%s, %s) as local media",
		userID, truncate(filepath.Base(dest), 60), mime, humanBytesLocal(bytes))
	return url, kind, nil
}

// mediaMgr returns the engine's Download & Stream manager (nil when the
// feature is off). Telegram ingestion reuses it so one directory, one janitor
// and one TTL policy cover both sources.
func (b *Bot) mediaMgr() *media.Manager {
	if b.mgr != nil {
		return b.mgr
	}
	if b.eng == nil {
		return nil
	}
	return b.eng.MediaManager()
}

// maxDownloadBytes returns the configured per-file limit (0 = unlimited),
// read from the media manager so the bot and the engine can never disagree.
func (b *Bot) maxDownloadBytes() int64 {
	if mgr := b.mediaMgr(); mgr != nil {
		return mgr.Opts().MaxBytes
	}
	return 0
}

// sanitizeTgName builds a safe file name inside dir from the session id and
// the Telegram-provided name. The Telegram name is attacker-controlled: only
// the extension is kept, the id carries the identity.
func sanitizeTgName(id, fileName, mime string) string {
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		ext = guessExtFromMIME(mime)
	}
	if !isSafeExt(ext) {
		ext = ".bin"
	}
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '-'
	}, id)
	if base == "" {
		base = "tg"
	}
	return base + ext
}

// guessExtFromMIME maps a MIME type to a playable extension for the common
// Telegram audio/video types.
func guessExtFromMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "audio/opus":
		return ".opus"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "video/x-matroska", "application/x-matroska":
		return ".mkv"
	}
	return ".bin"
}

// isSafeExt allows only extensions the embedded player can play.
func isSafeExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mp3", ".m4a", ".aac", ".flac", ".wav", ".opus", ".oga", ".ogg",
		".mp4", ".m4v", ".webm", ".mkv", ".mov", ".ogv", ".avi":
		return true
	}
	return false
}

// copyTgFile streams the Telegram file to dest, enforcing the announced size
// and refusing to write past it. It supports both local filesystem fast-copy
// (when running against a local Bot API server in --local mode) and HTTP streaming.
func copyTgFile(ctx context.Context, api API, fileID, dest string, size, maxBytes int64) error {
	file, err := api.File(fileID)
	if err != nil {
		return err
	}
	if file == nil || file.FilePath == "" {
		return fmt.Errorf("telegram getFile returned no file path: the file may exceed the Bot API size limit")
	}

	limit := size
	if limit <= 0 {
		limit = maxBytes
	}
	if limit <= 0 {
		limit = tgDefaultCloudMaxBytes
	}

	// 1. If running against a local Bot API server (--local), the file may already exist
	// directly on the local filesystem. Check if file.FilePath is an accessible local file.
	if filepath.IsAbs(file.FilePath) {
		if st, err := os.Stat(file.FilePath); err == nil && !st.IsDir() {
			src, err := os.Open(file.FilePath)
			if err != nil {
				return fmt.Errorf("open local telegram file: %w", err)
			}
			defer src.Close()

			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("create %s: %w", dest, err)
			}
			defer f.Close()

			n, err := io.Copy(f, io.LimitReader(src, limit))
			if err != nil {
				return fmt.Errorf("copy local telegram file: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("local telegram file is empty")
			}
			return nil
		}
	}

	// 2. HTTP download fallback (standard cloud API or remote/container HTTP server)
	url := api.FileDirectURL(file.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", "TeleMeetBot/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download telegram file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download telegram file: HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(resp.Body, limit))
	if err != nil {
		return fmt.Errorf("copy body: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("downloaded file is empty")
	}
	return nil
}

// tgHasMedia reports whether the message carries a downloadable attachment.
// A captioned photo counts as text by the router, so only true media
// messages (no usable text) take the ingestion path.
func tgHasMedia(m *tgbotapi.Message) bool {
	return m != nil && (m.Audio != nil || m.Document != nil || m.Video != nil ||
		m.VideoNote != nil || m.Voice != nil || m.Animation != nil)
}

// handleTelegramMedia downloads a forwarded Telegram file and points the
// wizard (or a live session's switch flow) at the local URL. The file is
// validated before it enters a session, and failures are reported honestly
// instead of silently ignored.
func (b *Bot) handleTelegramMedia(chatID, userID int64, m *tgbotapi.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The file is owned by the flow that will present it: the entry id ties
	// the file to the wizard so cleanup removes it when the flow is abandoned.
	f := b.getFlow(userID)
	id := "tg"
	if f != nil && f.sessionID != "" {
		id = f.sessionID
	}
	url, kind, err := b.ingestTelegramFile(ctx, userID, m, id)
	if err != nil {
		b.log.Warnf("[PIPELINE:ingest] user %d: telegram media rejected: %v", userID, err)
		b.sendText(chatID, "❌ "+esc(err.Error()), cancelKB())
		return
	}
	// A forwarded file is a real asset: it is already inside the managed
	// media directory. Stage it for the user instead of demanding a link
	// they cannot open — the card offers "start a new stream" and, when a
	// session is live, "switch" directly.
	entryID := id
	if e, ok := b.mediaMgr().FindByURL(url); ok {
		entryID = e.ID
	}
	if f != nil && (f.step == stepMedia || f.step == stepSwitchMedia) {
		target := engine.MediaTarget{URL: url, Kind: kind}
		switch f.step {
		case stepMedia:
			b.applyWizardMedia(chatID, userID, f, target)
		case stepSwitchMedia:
			b.applyMediaSwitchTarget(chatID, userID, f, target)
		}
		return
	}
	if f != nil && f.step == stepMeet && strings.TrimSpace(f.media) != "" {
		// A pending file already backs this wizard: append this additional file to the playlist!
		b.addPending(&pendingMedia{
			UserID: userID, EntryID: entryID, URL: url, Kind: kind,
			Name: nameOf(m), MIME: mimeOf(m), Bytes: sizeOf(m), Created: time.Now(),
		})
		f.queue = append(f.queue, url)
		b.sendText(chatID, fmt.Sprintf("📥 <b>Added to pending playlist (#%d):</b> <code>%s</code>\nSend the Google Meet link to launch.", len(f.queue)+1, esc(nameOf(m))), cancelKB())
		return
	}

	live := b.statuses()
	if len(live) == 1 && (f == nil || f.step == stepNone) {
		s := b.eng.Get(live[0].ID)
		if s != nil {
			added, err := s.Enqueue(url)
			if err == nil && len(added) > 0 {
				b.handoffPending(userID, entryID)
				items, curIdx := s.QueueSnapshot()
				var qRecs []store.QueueItemRecord
				for _, it := range items {
					qRecs = append(qRecs, store.QueueItemRecord{ID: it.ID, URL: it.URL, Title: it.Title})
				}
				b.st.UpdateQueue(string(s.ID), qRecs, curIdx)
				b.sendText(chatID, fmt.Sprintf("➕ Enqueued <code>%s</code>\nPosition: <b>%d / %d</b>",
					esc(nameOf(m)), curIdx+len(added), len(items)),
					enqueuedKB(string(s.ID), added[0].ID))
				return
			}
		}
	}

	b.addPending(&pendingMedia{
		UserID: userID, EntryID: entryID, URL: url, Kind: kind,
		Name: nameOf(m), MIME: mimeOf(m), Bytes: sizeOf(m), Created: time.Now(),
	})
	items := b.getPending(userID)
	b.sendText(chatID, pendingText(items, live), b.pendingKB(items, live))
}

// nameOf/mimeOf/sizeOf extract display metadata from the attachment the user
// forwarded, for the staged-file card.
func nameOf(m *tgbotapi.Message) string {
	_, fileName, _, _ := tgAttachment(m)
	if fileName == "" {
		return "telegram file"
	}
	return fileName
}

func mimeOf(m *tgbotapi.Message) string { _, _, mime, _ := tgAttachment(m); return mime }

func sizeOf(m *tgbotapi.Message) int64 { _, _, _, size := tgAttachment(m); return size }

// tgAttachment extracts the file handle, name, MIME type and size from any
// media-bearing Telegram message. Only one attachment is used: a multi-file
// album arrives as separate messages, each with its own handle.
func tgAttachment(m *tgbotapi.Message) (fileID, fileName, mime string, size int64) {
	switch {
	case m.Audio != nil:
		fileID, fileName, mime, size = m.Audio.FileID, m.Audio.FileName, m.Audio.MimeType, int64(m.Audio.FileSize)
	case m.Document != nil:
		fileID, fileName, mime, size = m.Document.FileID, m.Document.FileName, m.Document.MimeType, int64(m.Document.FileSize)
	case m.Video != nil:
		if m.Video.FileName == "" {
			m.Video.FileName = "video.mp4"
		}
		fileID, fileName, mime, size = m.Video.FileID, m.Video.FileName, m.Video.MimeType, int64(m.Video.FileSize)
	case m.VideoNote != nil:
		fileID, mime, size = m.VideoNote.FileID, "video/mp4", int64(m.VideoNote.FileSize)
	case m.Voice != nil:
		fileID, mime, size = m.Voice.FileID, m.Voice.MimeType, int64(m.Voice.FileSize)
	case m.Animation != nil:
		fileID, mime, size = m.Animation.FileID, m.Animation.MimeType, int64(m.Animation.FileSize)
	}
	return fileID, fileName, mime, size
}

// humanBytesLocal formats a byte count for user-facing errors.
func humanBytesLocal(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for nn := n / unit; nn >= unit; nn /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// validateTgMedia sniffs the file's real bytes instead of trusting the
// Telegram-provided MIME type, and rejects anything the pipeline would refuse
// at download time. Sniffing is what keeps a ".mp3" document that is actually
// a PDF or an HTML page out of the media player.
func validateTgMedia(path string, max int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open for sniff: %w", err)
	}
	defer f.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return fmt.Errorf("read for sniff: %w", err)
	}
	ct := http.DetectContentType(head[:n])
	if !media.AllowedContentType(ct) {
		return fmt.Errorf("file content is %s, not playable audio/video", strings.TrimSpace(ct))
	}
	if max > 0 {
		st, err := os.Stat(path)
		if err == nil && st.Size() > max {
			return fmt.Errorf("file is %s; the configured limit is %s", humanBytesLocal(st.Size()), humanBytesLocal(max))
		}
	}
	return nil
}

// diskFreeBytes reports free space on the volume holding dir (0 = unknown).
func diskFreeBytes(dir string) int64 {
	free, _, ok := media.DiskUsageForPathPublic(dir)
	if !ok {
		return 0
	}
	return int64(free)
}
