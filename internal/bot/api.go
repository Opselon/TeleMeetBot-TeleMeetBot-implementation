package bot

import (
	"fmt"
	"html"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// API is the narrow Telegram surface the bot uses (a fake implements it in tests).
type API interface {
	Send(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) (int, error)
	Edit(chatID int64, messageID int, text string, kb *tgbotapi.InlineKeyboardMarkup) error
	AnswerCallback(id, text string) error
	BotUsername() string
	Updates(timeoutSeconds int) tgbotapi.UpdatesChannel
	// File resolves a file_id to its Telegram File (FilePath is set when the
	// file is small enough for the Bot API to serve). Used to download
	// forwarded media; the derived URL is signed and must never be logged.
	File(fileID string) (*tgbotapi.File, error)
	FileDirectURL(filePath string) string
}

// tgAPI is the real implementation backed by go-telegram-bot-api.
type tgAPI struct {
	api      *tgbotapi.BotAPI
	username string
	endpoint string
}

// NewTelegramAPI validates the token and returns a long-polling API client.
func NewTelegramAPI(token string) (API, error) {
	return NewTelegramAPIWithEndpoint(token, "")
}

// NewTelegramAPIWithEndpoint supports standard cloud or custom local Bot API endpoints.
func NewTelegramAPIWithEndpoint(token, endpoint string) (API, error) {
	var api *tgbotapi.BotAPI
	var err error
	if endpoint != "" {
		api, err = tgbotapi.NewBotAPIWithAPIEndpoint(token, endpoint)
	} else {
		api, err = tgbotapi.NewBotAPI(token)
	}
	if err != nil {
		return nil, fmt.Errorf("telegram auth failed: %w", err)
	}
	return &tgAPI{api: api, username: api.Self.UserName, endpoint: endpoint}, nil
}

// Send posts an HTML message and returns its message id.
func (t *tgAPI) Send(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.DisableWebPagePreview = true
	if kb != nil {
		msg.ReplyMarkup = kb
	}
	sent, err := t.api.Send(msg)
	if err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

// Edit replaces an existing message.
func (t *tgAPI) Edit(chatID int64, messageID int, text string, kb *tgbotapi.InlineKeyboardMarkup) error {
	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.ParseMode = "HTML"
	msg.DisableWebPagePreview = true
	if kb != nil {
		msg.ReplyMarkup = kb
	}
	_, err := t.api.Send(msg)
	return err
}

// AnswerCallback acknowledges a button press.
func (t *tgAPI) AnswerCallback(id, text string) error {
	_, err := t.api.Request(tgbotapi.NewCallback(id, text))
	return err
}

// BotUsername returns the bot's @username.
func (t *tgAPI) BotUsername() string { return t.username }

// File resolves a file_id through getFile. The FilePath is present only when
// the file is under the Bot API's 20 MB ceiling.
func (t *tgAPI) File(fileID string) (*tgbotapi.File, error) {
	file, err := t.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("telegram getFile: %w", err)
	}
	return &file, nil
}

// FileDirectURL builds the download URL for a FilePath. The link is signed
// and short-lived; callers must use it immediately and never log it.
func (t *tgAPI) FileDirectURL(filePath string) string {
	if t.endpoint != "" {
		base := strings.Replace(t.endpoint, "/bot%s/%s", "", 1)
		base = strings.TrimSuffix(base, "/")
		return fmt.Sprintf("%s/file/bot%s/%s", base, t.api.Token, filePath)
	}
	return fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", t.api.Token, filePath)
}

// Updates starts the long-poll channel.
func (t *tgAPI) Updates(timeoutSeconds int) tgbotapi.UpdatesChannel {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = timeoutSeconds
	return t.api.GetUpdatesChan(u)
}

// botInfo is used by the wizard for token validation without the bot package.
func botInfo(token string) (username string, id int64, err error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return "", 0, err
	}
	return api.Self.UserName, api.Self.ID, nil
}

// BotInfo is the exported token validator used by setup.
func BotInfo(token string) (string, int64, error) { return botInfo(token) }

// esc escapes user text for HTML parse mode.
func esc(s string) string {
	return html.EscapeString(strings.TrimSpace(s))
}

// truncate shortens a string for compact card display.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
