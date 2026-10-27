package bot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// showAccount displays or edits the Google Account profile card for a user.
func (b *Bot) showAccount(chatID, userID int64, msgID int) {
	if b.gauth == nil {
		b.sendText(chatID, "🔴 Google Account profile management is unavailable on this installation.", mainMenuKB())
		return
	}
	sum := b.gauth.Summary(userID)
	text := accountText(sum)
	kb := accountKB(sum.Connected)
	if msgID > 0 {
		_ = b.api.Edit(chatID, msgID, text, kb)
	} else {
		b.sendText(chatID, text, kb)
	}
}

// hasGoogleAccount reports whether userID has active Google authentication cookies.
func (b *Bot) hasGoogleAccount(userID int64) bool {
	if b.gauth == nil {
		return false
	}
	return b.gauth.Summary(userID).Connected
}

// saveUserCookies validates and persists raw cookie data for userID.
func (b *Bot) saveUserCookies(chatID, userID int64, raw []byte, source string) {
	if b.gauth == nil {
		b.sendText(chatID, "🔴 Google Account profile management is unavailable on this installation.", mainMenuKB())
		return
	}

	sum, err := b.gauth.Save(userID, raw)
	if err != nil {
		b.log.Warnf("[PIPELINE:googleauth] user %d: failed to save cookies from %s: %v", userID, source, err)
		b.sendText(chatID, fmt.Sprintf("❌ <b>Failed to load cookies:</b>\n%s\n\nMake sure the file contains active Google cookies (.google.com) and is not expired.", esc(err.Error())), cancelKB())
		return
	}

	b.clearFlow(userID)
	b.log.Infof("[PIPELINE:googleauth] user %d: successfully saved %d Google cookies from %s", userID, sum.CookieCount, source)

	msg := fmt.Sprintf("✅ <b>Google Account Connected!</b>\n\nSuccessfully saved <b>%d</b> Google session cookies.\nFuture Google Meet and YouTube sessions will join with your authenticated profile.", sum.CookieCount)
	b.sendText(chatID, msg, accountKB(true))
}

// handleCookieDocUpload downloads an incoming Telegram document and parses it as cookies.
func (b *Bot) handleCookieDocUpload(m *tgbotapi.Message, userID, chatID int64) {
	if m.Document == nil {
		return
	}

	b.log.Infof("[PIPELINE:googleauth] user %d: downloading cookie file %s (%d bytes)", userID, m.Document.FileName, m.Document.FileSize)
	data, err := b.downloadTelegramBytes(m.Document.FileID)
	if err != nil {
		b.log.Warnf("[PIPELINE:googleauth] user %d: download cookie file failed: %v", userID, err)
		b.sendText(chatID, fmt.Sprintf("❌ <b>Download failed:</b> %s", esc(err.Error())), cancelKB())
		return
	}

	b.saveUserCookies(chatID, userID, data, m.Document.FileName)
}

// downloadTelegramBytes downloads up to 2MB from the Telegram Bot API file storage.
func (b *Bot) downloadTelegramBytes(fileID string) ([]byte, error) {
	file, err := b.api.File(fileID)
	if err != nil {
		return nil, fmt.Errorf("lookup file: %w", err)
	}

	url := b.api.FileDirectURL(file.FilePath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("User-Agent", "TeleMeetBot/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	// Cap at 2MB for cookies
	if _, err := io.Copy(&buf, io.LimitReader(resp.Body, 2<<20)); err != nil {
		return nil, fmt.Errorf("read file body: %w", err)
	}
	return buf.Bytes(), nil
}
