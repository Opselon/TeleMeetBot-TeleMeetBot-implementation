package bot

import (
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/googleauth"
)

// accountText formats the Google Account status card.
func accountText(sum googleauth.AccountSummary) string {
	var sb strings.Builder
	sb.WriteString("🔐 <b>Google Account Profile</b>\n\n")
	if sum.Connected {
		sb.WriteString("Status: <b>Connected</b> ✅\n")
		if sum.Name != "" {
			sb.WriteString(fmt.Sprintf("• <b>Name:</b> %s\n", esc(sum.Name)))
		}
		sb.WriteString(fmt.Sprintf("• <b>Email:</b> <code>%s</code>\n", esc(sum.Email)))
		sb.WriteString("• <b>Protocol:</b> OAuth 2.0 / OpenID Connect (PKCE S256)\n")
		sb.WriteString("• <b>Security:</b> AES-256-GCM encrypted tokens at rest\n")
		if !sum.ConnectedAt.IsZero() {
			sb.WriteString(fmt.Sprintf("• <b>Connected:</b> <code>%s</code>\n", sum.ConnectedAt.Format("2006-01-02 15:04:05 UTC")))
		}
		sb.WriteString("\n<i>Your verified Google identity will be presented to meeting organizers when joining Google Meet sessions.</i>")
	} else {
		sb.WriteString("Status: <b>Guest Mode (Not connected)</b> 🚪\n\n")
		sb.WriteString("Connect your Google Account securely using official Google Sign-In (OAuth 2.0 + OpenID Connect).\n\n")
		sb.WriteString("• <b>Zero passwords or manual cookies:</b> Official Google consent screen\n")
		sb.WriteString("• <b>PKCE & State Validation:</b> Protected against CSRF and code injection\n")
		sb.WriteString("• <b>AES-256-GCM:</b> Tokens encrypted at rest with multi-tenant isolation\n")
		sb.WriteString("• <b>Identity Verification:</b> Bot joins meetings with your verified Google name & email\n\n")
		sb.WriteString("Tap <b>Continue with Google</b> below to sign in.")
	}
	return sb.String()
}

// accountKB builds the keyboard for the Google Account card.
func accountKB(sum googleauth.AccountSummary, authURL string) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	if sum.Connected {
		var actionRow []tgbotapi.InlineKeyboardButton
		if authURL != "" {
			actionRow = append(actionRow, tgbotapi.NewInlineKeyboardButtonURL("🔄 Reconnect Google", authURL))
		}
		actionRow = append(actionRow, tgbotapi.NewInlineKeyboardButtonData("🗑 Disconnect Account", cbAccountRemove))
		rows = append(rows, actionRow)
	} else {
		if authURL != "" {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonURL("🌐 Continue with Google", authURL),
			})
		} else {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData("🌐 Sign In with Google", cbAccountConnect),
			})
		}
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("ℹ️ Meet & OAuth Info", cbAccountHelp),
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Back to Menu", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// accountHelpText explains the OAuth 2.0 architecture and Google Meet integration policies.
func accountHelpText() string {
	return strings.TrimSpace(`
ℹ️ <b>Google OAuth 2.0 & Google Meet Integration</b>

<b>Security Architecture:</b>
• <b>OAuth 2.0 + OpenID Connect:</b> Official Google Sign-In using Authorization Code flow with PKCE (Proof Key for Code Exchange, S256).
• <b>Strict State & Nonce:</b> Cryptographically signed state and random nonces prevent CSRF and replay attacks.
• <b>Token Protection:</b> All OAuth tokens are encrypted at rest using AES-256-GCM. Tokens are never exposed in logs or messages.
• <b>Multi-Tenant Isolation:</b> Token keys and storage are strictly isolated per Telegram user.

<b>Google Meet & Browser Authentication Boundary:</b>
• Google's security policy strictly isolates browser session cookies (meet.google.com) from OAuth 2.0 API tokens to protect against session hijacking.
• Third-party OAuth tokens cannot generate first-party Google Account browser cookies.
• TeleMeetBot presents your verified Google Identity (name & email) when requesting entry, ensuring meeting organizers can recognize and admit you with full trust.
• You can revoke access or disconnect your account at any time with <b>Disconnect Account</b>.
`)
}
