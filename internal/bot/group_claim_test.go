package bot

import (
	"context"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// TestFirstStartClaimRefusedFromGroupChat is the regression test for the
// onboarding hijack. authorizeUser accepted the first-start admin claim from
// any chat: a bot added to a group before it was configured handed sole admin
// to whichever member typed /start first — in a public group that is anyone —
// and the operator lost the bot before ever opening it.
//
// The claim path now requires a private chat. An existing admin is never
// gated on chat type, so management from a group still works as before.
func TestFirstStartClaimRefusedFromGroupChat(t *testing.T) {
	// Fresh, unconfigured bot: FirstStartClaimsAdmin defaults to true and the
	// admin allow-list is empty — exactly the state a fresh install is in.
	h := newHarness(t, nil, 2)

	// A group member typing /start must not claim the bot.
	h.bot.handleMessage(&tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 999},
		Chat:      &tgbotapi.Chat{ID: -1001234567890, Type: "group"},
		Text:      "/start",
		Entities:  []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 6}},
	})

	if len(h.cfg.Telegram.Admins) != 0 {
		t.Fatalf("group chat claimed admin: admins=%v (the bot was hijacked)", h.cfg.Telegram.Admins)
	}
	if !strings.Contains(h.api.lastSent().text, "Access denied") {
		t.Fatalf("group /start: expected a denial, got %q", h.api.lastSent().text)
	}
	if !strings.Contains(h.api.lastSent().text, "private chat") {
		t.Fatalf("group /start: the denial should point the operator at a private chat, got %q", h.api.lastSent().text)
	}

	// The same user, from a private chat, claims the bot as designed.
	h.message(999, "/start")
	if len(h.cfg.Telegram.Admins) != 1 || h.cfg.Telegram.Admins[0] != 999 {
		t.Fatalf("private chat did not claim admin: %v", h.cfg.Telegram.Admins)
	}
	if !strings.Contains(h.api.allSentText(), "You are now the admin") {
		t.Fatalf("private claim: expected the admin notice, got %q", h.api.lastSent().text)
	}
}

// TestCallbackClaimRefusedFromGroupChat covers the callback entry point,
// which is the other surface that can reach the claim.
func TestCallbackClaimRefusedFromGroupChat(t *testing.T) {
	h := newHarness(t, nil, 2)

	h.bot.handleCallback(context.Background(), &tgbotapi.CallbackQuery{
		ID:   "cb1",
		From: &tgbotapi.User{ID: 999},
		Message: &tgbotapi.Message{
			MessageID: 10,
			Chat:      &tgbotapi.Chat{ID: -1001234567890, Type: "group"},
		},
		Data: "menu:new",
	})
	if len(h.cfg.Telegram.Admins) != 0 {
		t.Fatalf("group callback claimed admin: admins=%v", h.cfg.Telegram.Admins)
	}
}

// TestExistingAdminWorksFromGroupChat pins the other half of the rule: once
// the operator is configured, the chat type must not gate them — the bot
// keeps working in a group exactly as before.
func TestExistingAdminWorksFromGroupChat(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)

	h.bot.handleMessage(&tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: 42},
		Chat:      &tgbotapi.Chat{ID: -1001234567890, Type: "group"},
		Text:      "/start",
		Entities:  []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 6}},
	})
	if !strings.Contains(h.api.allSentText(), "Active sessions") {
		t.Fatalf("existing admin gated out of a group: %q", h.api.allSentText())
	}
}
