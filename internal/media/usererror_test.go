package media

import (
	"errors"
	"strings"
	"testing"
)

// usererror_test.go — regression tests for the end-user message taxonomy.
//
// The contract under test (Acceptance Criteria "End-User Experience"):
//
//  1. Every raw technical failure maps to a stable, actionable user message.
//  2. The raw diagnostic is never destroyed: it stays wrapped for the logs.
//  3. No user message ever leaks credentials, internal addresses, argv,
//     binary names or transport jargon.
//  4. Unknown failures degrade to a honest generic message, never to a leak.
//  5. Recoverable classes (network, source throttling) are identifiable so
//     the retry policy and the "please try again" phrasing stay in sync.

// TestUserErrorClassification covers the taxonomy mapping: each real producer
// string (yt-dlp stderr, Go net/http, the package's own sentinels) must land
// on the documented category with a user-facing message that names the action.
func TestUserErrorClassification(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		// wantContains must appear in the USER message (not the detail).
		wantContains string
	}{
		{"unsupported url yt-dlp", errors.New("ERROR: [youtube] x: Unsupported URL"), UserCodeUnsupported, "isn't supported"},
		{"unsupported sentinel", errors.New("media kind is not downloadable: page links are not downloadable"), UserCodeUnsupported, "can't be downloaded"},
		{"unsupported content type", errors.New(`unsupported content type "text/html; charset=utf-8"`), UserCodeUnsupported, "doesn't point to playable"},
		{"format unavailable", errors.New("ERROR: [youtube] x: Requested format is not available"), UserCodeSourceGone, "no downloadable format"},
		{"video unavailable", errors.New("ERROR: [youtube] x: Video unavailable"), UserCodeSourceGone, "unavailable"},
		{"private video", errors.New("ERROR: [youtube] x: Private video"), UserCodeSourceGone, "private"},
		{"bot check ascii", errors.New("ERROR: [youtube] x: Sign in to confirm you're not a bot"), UserCodeBlocked, "limiting this server"},
		{"bot check curly quote", errors.New("ERROR: [youtube] x: Sign in to confirm you’re not a bot"), UserCodeBlocked, "limiting this server"},
		{"http 429", errors.New("ERROR: [youtube] x: HTTP Error 429: Too Many Requests"), UserCodeBlocked, "rate-limiting"},
		{"dns failure", errors.New("dial tcp: lookup x.example.com: no such host"), UserCodeNetwork, "couldn't resolve"},
		{"tls eof", errors.New("TLS/SSL connection has been closed (EOF)"), UserCodeNetwork, "interrupted"},
		{"connection reset", errors.New("read tcp: connection reset by peer"), UserCodeNetwork, "interrupted"},
		{"timeout", errors.New("context deadline exceeded (Client.Timeout exceeded)"), UserCodeNetwork, "too long to respond"},
		{"incomplete read", errors.New("<IncompleteRead: 15036 bytes read>"), UserCodeNetwork, "cut short"},
		{"unable to download api page", errors.New("ERROR: [youtube] x: Unable to download API page: 15036 bytes read"), UserCodeNetwork, "interrupted"},
		{"unable to download video data", errors.New("ERROR: [youtube] x: Unable to download video data: HTTP Error 403"), UserCodeNetwork, "interrupted"},
		{"body http 403 alone", errors.New("ERROR: [youtube] x: unable to download video data: HTTP Error 403: Forbidden"), UserCodeNetwork, "interrupted"},
		{"proxy unreachable", errors.New("unable to connect to proxy"), UserCodeNetwork, "network route"},
		{"size limit", errors.New("download exceeds size limit: server reports 3.0 GiB, limit is 2.0 GiB"), UserCodeTooLarge, "larger than the download limit"},
		{"no space", errors.New("insufficient disk space: 100 MiB free in /data/downloads (minimum 256 MiB required)"), UserCodeNoSpace, "disk space"},
		{"missing tool", errors.New("no such file or directory: yt-dlp not found"), UserCodeMissingTool, "isn't installed"},
		{"cancelled", errors.New("context canceled"), UserCodeCancelled, "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := ClassifyUserError(tc.err)
			if u == nil {
				t.Fatal("classification returned nil for a non-nil error")
			}
			if u.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", u.Code, tc.wantCode)
			}
			if !strings.Contains(u.Message, tc.wantContains) {
				t.Fatalf("user message %q must contain %q", u.Message, tc.wantContains)
			}
			// Error() returns the USER message — the raw text must never be
			// reachable through the error interface itself.
			if got := u.Error(); got != u.Message {
				t.Errorf("Error() must return the user message, got %q", got)
			}
			// The raw diagnostic survives for the operator log.
			if !strings.Contains(u.Detail(), tc.err.Error()) {
				t.Errorf("Detail() must carry the raw error, got %q", u.Detail())
			}
			// And the wrap chain keeps error.Is/error.As working.
			if !errors.Is(u, tc.err) {
				t.Errorf("the wrapped chain must expose the original error")
			}
		})
	}
}

// TestUserErrorNeverLeaksInfrastructure is the leak guard: no classification
// may surface credentials, internal addresses, argv, binary names or raw
// transport jargon in the USER-facing message.
func TestUserErrorNeverLeaksInfrastructure(t *testing.T) {
	raws := []string{
		"yt-dlp: exit status 1: ERROR: [youtube] dQw4w9WgXcQ: Sign in to confirm you're not a bot [egress ladder tried: direct → http://user:pass@172.16.0.1:3339 → socks5://172.16.0.1:9050]",
		"yt-dlp: exit status 1: ERROR: [youtube] x: Unable to download API page: TLS/SSL connection has been closed (EOF) (_ssl.c:992)",
		"fetch 172.16.0.1: dial tcp 10.0.0.2:443: connection refused",
		"insufficient disk space: 100 MiB free in /var/lib/telemeetbot/downloads",
		"download exceeds size limit: server reports 3298534883328 bytes",
	}
	for _, raw := range raws {
		u := ClassifyUserError(errors.New(raw))
		if u == nil {
			t.Fatalf("classification returned nil for %q", raw)
		}
		for _, leak := range []string{
			"172.16", "10.0.0", "user:pass", "exit status", "yt-dlp", "_ssl.c",
			"/var/lib", "egress ladder", "socks5://", "http://", "dQw4w9WgXcQ",
		} {
			if strings.Contains(u.Message, leak) {
				t.Errorf("user message leaks %q\n  message: %q\n  raw:     %q", leak, u.Message, raw)
			}
		}
		// The operator surface keeps everything.
		if !strings.Contains(u.Detail(), raw) {
			t.Errorf("Detail() lost the raw diagnostic for %q", raw)
		}
	}
}

// TestUserErrorUnknownDegradesHonestly proves an unmatched failure produces
// the honest generic message instead of relaying the raw text.
func TestUserErrorUnknownDegradesHonestly(t *testing.T) {
	raw := errors.New("some completely novel internal invariant violated at offset 0xdeadbeef")
	u := ClassifyUserError(raw)
	if u.Code != UserCodeInternal {
		t.Fatalf("code = %q, want %q", u.Code, UserCodeInternal)
	}
	if !strings.Contains(u.Message, "couldn't be prepared") {
		t.Fatalf("the generic fallback must be honest and actionable, got %q", u.Message)
	}
	for _, leak := range []string{"0xdeadbeef", "internal invariant"} {
		if strings.Contains(u.Message, leak) {
			t.Errorf("the generic fallback leaked raw text %q: %q", leak, u.Message)
		}
	}
}

// TestUserCodeOfAndIsUserCode proves the typed accessors used by the engine
// and the bot agree with the classification.
func TestUserCodeOfAndIsUserCode(t *testing.T) {
	err := errors.New("dial tcp: no such host")
	if code := UserCodeOf(err); code != UserCodeNetwork {
		t.Fatalf("UserCodeOf = %q, want %q", code, UserCodeNetwork)
	}
	if !IsUserCode(err, UserCodeNetwork) {
		t.Error("IsUserCode must report the network class")
	}
	if IsUserCode(err, UserCodeBlocked) {
		t.Error("IsUserCode must not report an unrelated class")
	}
	if UserFacing(err) == "" {
		t.Error("UserFacing must surface the classified message")
	}
	if UserFacing(errors.New("novel failure")) == "" {
		t.Error("UserFacing must surface the generic fallback for unknown errors")
	}
}

// TestIsUserRecoverable proves the retry policy classification: a network
// blip or a source throttle is worth another attempt, a permanent verdict is
// not. This is what keeps "please try again" phrasing honest.
func TestIsUserRecoverable(t *testing.T) {
	if !IsUserRecoverable(errors.New("connection reset by peer")) {
		t.Error("a connection reset is recoverable")
	}
	if !IsUserRecoverable(errors.New("Sign in to confirm you're not a bot")) {
		t.Error("source throttling is recoverable (retry later)")
	}
	if IsUserRecoverable(errors.New("Requested format is not available")) {
		t.Error("a format verdict is NOT recoverable")
	}
	if IsUserRecoverable(errors.New("Unsupported URL")) {
		t.Error("an unsupported URL is NOT recoverable")
	}
	if IsUserRecoverable(errors.New("download exceeds size limit")) {
		t.Error("a size verdict is NOT recoverable")
	}
}

// TestFriendlyUserMessageIsTheSingleEntry proves the surface entry point
// classifies an unknown error rather than returning the raw text.
func TestFriendlyUserMessageIsTheSingleEntry(t *testing.T) {
	got := FriendlyUserMessage(errors.New("dial tcp: no such host"))
	if !strings.Contains(got, "couldn't resolve") && !strings.Contains(got, "try again") {
		t.Fatalf("FriendlyUserMessage must classify, got %q", got)
	}
	// The generic fallback for an unknown error is honest, not empty.
	got = FriendlyUserMessage(errors.New("novel catastrophic thing"))
	if !strings.Contains(got, "couldn't be prepared") {
		t.Fatalf("FriendlyUserMessage must degrade honestly, got %q", got)
	}
	if FriendlyUserMessage(nil) == "" {
		t.Error("a nil error should still produce the honest generic message")
	}
}

// TestUserErrorNilIsNil proves the classifier never invents a failure.
func TestUserErrorNilIsNil(t *testing.T) {
	if got := ClassifyUserError(nil); got != nil {
		t.Fatalf("ClassifyUserError(nil) must be nil, got %+v", got)
	}
}
