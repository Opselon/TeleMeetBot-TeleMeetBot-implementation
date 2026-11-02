package dashboard

import (
	"strings"
	"testing"
)

// TestLoginPageDefinesOnTelegramAuth is the regression test for the dead web
// login. The login page rendered the Telegram Login Widget with
// data-onauth="onTelegramAuth(user)" but the page never defined that handler,
// so the widget rendered and clicked but threw "onTelegramAuth is not
// defined" the moment a user completed Telegram authorization — with a
// fully-implemented HMAC backend that never received the payload.
//
// The handler must appear in the page BEFORE the widget script so the
// iframe's callback always finds it.
func TestLoginPageDefinesOnTelegramAuth(t *testing.T) {
	html := indexHTML("MyTestBot", "")

	if !strings.Contains(html, "telegram-widget.js") {
		t.Fatalf("page does not load the Telegram Login Widget")
	}
	if !strings.Contains(html, `data-onauth="onTelegramAuth(user)"`) {
		t.Fatalf("page does not wire data-onauth to onTelegramAuth")
	}

	handlerAt := strings.Index(html, "window.onTelegramAuth")
	widgetAt := strings.Index(html, "telegram-widget.js")
	if handlerAt < 0 {
		t.Fatalf("page references onTelegramAuth but never defines window.onTelegramAuth — web login is dead code")
	}
	if widgetAt < 0 {
		t.Fatalf("missing widget script")
	}
	if handlerAt > widgetAt {
		t.Fatalf("window.onTelegramAuth is defined after the widget script; it must be defined before it so the callback is ready when the iframe looks it up")
	}

	// The handler must post the signed fields to the backend endpoint.
	if !strings.Contains(html, "/api/auth/telegram") {
		t.Fatalf("handler does not POST to /api/auth/telegram")
	}
	for _, field := range []string{"id", "auth_date", "hash"} {
		if !strings.Contains(html, `"`+field+`"`) {
			t.Fatalf("handler does not forward the %q field the backend verifies", field)
		}
	}

	// The bot username appears in exactly two places, both inert: the
	// widget's data-telegram-login attribute and the setup hint. No user
	// data is embedded into the page — the handler only ever forwards the
	// Telegram-signed payload.
	if strings.Count(html, "MyTestBot") != 2 {
		t.Fatalf("unexpected number of MyTestBot occurrences: %d", strings.Count(html, "MyTestBot"))
	}
}

// TestLoginPageWithoutBotUsername keeps the disabled-dashboards path honest:
// no bot username means no widget, and then the handler must not be emitted
// either (there is nothing to call it).
func TestLoginPageWithoutBotUsername(t *testing.T) {
	html := indexHTML("", "")
	if strings.Contains(html, "telegram-widget.js") {
		t.Fatalf("widget rendered without a bot username")
	}
	if strings.Contains(html, "onTelegramAuth") {
		t.Fatalf("handler emitted without a widget to call it")
	}
}
