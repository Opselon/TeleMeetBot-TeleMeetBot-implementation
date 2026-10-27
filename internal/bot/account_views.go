package bot

import (
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/googleauth"
)

// accountText formats the Google Account status card.
func accountText(sum googleauth.AccountSummary) string {
	var sb strings.Builder
	sb.WriteString("👤 <b>Google Account Profile</b>\n\n")
	if sum.Connected {
		sb.WriteString("Status: <b>Connected</b> ✅\n")
		sb.WriteString(fmt.Sprintf("Cookies: <b>%d Google session cookies</b>\n", sum.CookieCount))
		if !sum.ExpiresAt.IsZero() {
			if sum.HasExpired {
				sb.WriteString(fmt.Sprintf("Expiry: ⚠️ <b>Expired on %s</b>\n", sum.ExpiresAt.Format("2006-01-02 15:04")))
			} else {
				d := time.Until(sum.ExpiresAt).Round(time.Hour)
				sb.WriteString(fmt.Sprintf("Expires: <b>in %d days</b> (%s)\n", int(d.Hours()/24), sum.ExpiresAt.Format("2006-01-02")))
			}
		}
		sb.WriteString(fmt.Sprintf("Updated: <code>%s</code>\n\n", sum.UpdatedAt.Format("2006-01-02 15:04:05 UTC")))
		sb.WriteString("<i>Your sessions will automatically open under this authenticated Google profile in Google Meet and YouTube, bypassing guest admission and restricted-meeting gates.</i>")
	} else {
		sb.WriteString("Status: <b>Guest Mode (Not connected)</b> 🚪\n\n")
		sb.WriteString("By default, the bot enters Google Meet as an anonymous guest.\n")
		sb.WriteString("To join meetings under your Google Account without needing host admission or when meetings require signed-in accounts:\n\n")
		sb.WriteString("1. Log in to your Google Account on your browser.\n")
		sb.WriteString("2. Export your cookies using an extension (e.g. <i>Get cookies.txt LOCALLY</i> or <i>Cookie-Editor</i>).\n")
		sb.WriteString("3. Send or upload the <code>cookies.txt</code> or <code>.json</code> file here, or click <b>Upload Cookies</b> below.")
	}
	return sb.String()
}

// accountKB builds the keyboard for the Google Account card.
func accountKB(connected bool) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	if connected {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("🔄 Update Cookies", cbAccountUpload),
			tgbotapi.NewInlineKeyboardButtonData("🗑 Disconnect Account", cbAccountRemove),
		})
	} else {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📥 Upload Cookies", cbAccountUpload),
			tgbotapi.NewInlineKeyboardButtonData("ℹ️ How To Export", cbAccountHelp),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Back to Menu", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// accountHelpText explains how users can export their Google cookies safely.
func accountHelpText() string {
	return strings.TrimSpace(`
ℹ️ <b>How to Export and Connect Google Cookies</b>

<b>Method 1: Using Chrome / Firefox / Edge Extension</b>
1. Install an extension like <b>Get cookies.txt LOCALLY</b> or <b>Cookie-Editor</b>.
2. Open <a href="https://meet.google.com">meet.google.com</a> while signed in to your Google Account.
3. Open the extension and click <b>Export</b> (choose <i>Netscape HTTP Cookie File</i> format or <i>JSON</i>).
4. Send the downloaded <code>cookies.txt</code> file directly to this bot in Telegram!

<b>Method 2: Paste Cookie Text</b>
Click <b>Upload Cookies</b>, then paste the contents of your exported cookie file directly into the chat.

<b>Security & Privacy:</b>
• Your cookies are stored isolated in a private directory on this host.
• Cookies are never shared with other users or exposed in logs.
• You can remove your account profile at any time with <b>Disconnect Account</b>.
`)
}
