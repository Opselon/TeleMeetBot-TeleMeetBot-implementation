package bot

import (
	"context"
	"fmt"
)

// showAccount displays or edits the Google Account profile card for a user.
func (b *Bot) showAccount(chatID, userID int64, msgID int) {
	if b.gauth == nil {
		b.sendText(chatID, "🔴 Google Account profile management is unavailable on this installation.", mainMenuKB())
		return
	}
	sum := b.gauth.Summary(userID)
	authURL, err := b.gauth.AuthURL(userID)
	if err != nil && b.log != nil {
		b.log.Debugf("[PIPELINE:googleauth] generate authURL for user %d: %v", userID, err)
	}

	text := accountText(sum)
	kb := accountKB(sum, authURL)
	if msgID > 0 {
		_ = b.api.Edit(chatID, msgID, text, kb)
	} else {
		b.sendText(chatID, text, kb)
	}
}

// hasGoogleAccount reports whether userID has an active Google authentication profile.
func (b *Bot) hasGoogleAccount(userID int64) bool {
	if b.gauth == nil {
		return false
	}
	return b.gauth.Summary(userID).Connected
}

// handleAccountDisconnect revokes and deletes the stored Google OAuth profile for userID.
func (b *Bot) handleAccountDisconnect(chatID, userID int64, msgID int) {
	if b.gauth == nil {
		return
	}
	if err := b.gauth.Revoke(context.Background(), userID); err != nil && b.log != nil {
		b.log.Warnf("[PIPELINE:googleauth] user %d: revoke profile error: %v", userID, err)
	}
	b.showAccount(chatID, userID, msgID)
}

// NotifyGoogleConnected notifies a user in Telegram when their Google OAuth flow succeeds.
func (b *Bot) NotifyGoogleConnected(userID int64, email, name string) {
	displayName := name
	if displayName == "" {
		displayName = email
	}
	text := fmt.Sprintf("🎉 <b>Google Account Connected!</b>\n\n"+
		"Signed in as: <b>%s</b> (<code>%s</code>)\n\n"+
		"Your verified Google identity is now securely linked to your Telegram account. Future sessions will present this identity when joining Google Meet.",
		esc(displayName), esc(email))

	// Send to user's chat ID (for private bots, chatID == userID)
	b.sendText(userID, text, mainMenuKB())
}
