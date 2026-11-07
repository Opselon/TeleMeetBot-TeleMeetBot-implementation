package media

import (
	"errors"
	"strings"
)

// End-user message taxonomy.
//
// The download pipeline's raw errors are written for operators: they name
// binaries ("yt-dlp"), transports ("TLS/SSL connection has been closed (EOF)"),
// proxies and hostnames. None of that helps an ordinary user, and much of it
// (internal addresses, toolchain details) must not be handed out at all.
//
// This file owns the single translation table from a technical failure to a
// short, actionable, user-facing sentence. The contract:
//
//   - The classification is decided from the error text ONLY (any error, from
//     any backend). Callers never have to know which layer produced it.
//   - UserMessage never echoes credentials, internal IPs, or argv. A proxy URL
//     in the message is reduced to the word "proxy".
//   - The original error is never destroyed: it stays wrapped and is logged
//     by the caller. UserMessage is the *user's* summary, not the diagnostic.
//   - Everything unknown degrades to a honest, non-misleading generic message
//     rather than to a leak of the raw text.

// UserError is a classified, user-facing failure.
//
// Code is a stable, machine-readable category the UIs can switch on (and tests
// can assert); Message is the human sentence; the wrapped err carries the raw
// technical detail for operator logs and must never be rendered to an end user.
type UserError struct {
	Code    string
	Message string
	err     error
}

// Error implements error, returning the USER-FACING message — not the raw
// wrapped detail. Anything that surfaces the error to a user (a Telegram card,
// a dashboard badge) therefore cannot leak by accident; anything that needs
// the technical diagnosis calls Unwrap (or logs the original error it kept).
func (u *UserError) Error() string { return u.Message }

// Unwrap returns the underlying technical error, for error.Is/error.As and
// for operator logging. It is deliberately NOT part of Error().
func (u *UserError) Unwrap() error { return u.err }

// Detail returns the raw technical diagnosis, or the user message when no
// underlying error was captured. Operator logs and the doctor use this; the
// Telegram card and the dashboard badge use Error().
func (u *UserError) Detail() string {
	if u.err != nil {
		return u.err.Error()
	}
	return u.Message
}

// UserErrorCode values. Stable identifiers: UIs and tests switch on these.
const (
	// UserCodeUnsupported: the link itself cannot be played (bad URL,
	// unsupported source, missing format).
	UserCodeUnsupported = "unsupported"
	// UserCodeMissingTool: a required external binary is absent.
	UserCodeMissingTool = "missing_tool"
	// UserCodeTooLarge: the source or the fetched file exceeds a limit.
	UserCodeTooLarge = "too_large"
	// UserCodeNoSpace: not enough disk space.
	UserCodeNoSpace = "no_space"
	// UserCodeBlocked: the media source refused this server (bot check /
	// rate limit). Actionable: wait, or try another link.
	UserCodeBlocked = "source_blocked"
	// UserCodeNetwork: the network route to the source is broken (DNS,
	// TLS, reset, timeout, dead proxy).
	UserCodeNetwork = "network_unavailable"
	// UserCodeSourceGone: the video was removed / made private / is
	// unavailable in the region.
	UserCodeSourceGone = "source_unavailable"
	// UserCodeLiveStream: the source is a live broadcast that cannot be
	// downloaded as a file.
	UserCodeLiveStream = "live_stream"
	// UserCodeCancelled: the operation was cancelled (session stopped).
	UserCodeCancelled = "cancelled"
	// UserCodeInternal: anything else — a real, unexpected failure the
	// user cannot act on, reported without internal details.
	UserCodeInternal = "internal"
)

// userRules are the classification rules, most specific first. Each rule is a
// lowercase substring (matched against the lowercased error text) and the
// category it implies. Order matters: the first match wins.
//
// The phrases come from the two real producers — yt-dlp's stderr and Go's
// net/http — plus the media package's own sentinel errors. Anything new
// appears here as a rule first, never as a special case at a call site.
var userRules = []userRule{
	// Cancellation must be classified before generic network markers, since a
	// cancelled context wraps whatever transport error was in flight.
	{substring: "context canceled", code: UserCodeCancelled, message: "The operation was cancelled."},
	{substring: "operation was canceled", code: UserCodeCancelled, message: "The operation was cancelled."},

	// The user asked for something the source cannot provide.
	{substring: "unsupported url", code: UserCodeUnsupported, message: "This link type isn't supported. YouTube watch links, direct video/audio files and public web pages work."},
	{substring: "is not downloadable", code: UserCodeUnsupported, message: "This link can't be downloaded. Send a YouTube link or a direct video/audio file instead."},
	{substring: "unsupported content type", code: UserCodeUnsupported, message: "The link doesn't point to playable audio or video."},
	{substring: "unsupported extension", code: UserCodeUnsupported, message: "The file type isn't playable in the meeting."},
	{substring: "unsupported file type", code: UserCodeUnsupported, message: "This file type can't be presented. Only audio and video can be shared."},
	{substring: "requested format is not available", code: UserCodeSourceGone, message: "This video has no downloadable format right now. Try playing it in the browser instead."},
	{substring: "video unavailable", code: UserCodeSourceGone, message: "This video is unavailable — it may have been removed or made private."},
	{substring: "this video is not available", code: UserCodeSourceGone, message: "This video is unavailable in this region or has been removed."},
	{substring: "private video", code: UserCodeSourceGone, message: "This video is private and can't be played."},
	{substring: "members-only", code: UserCodeSourceGone, message: "This video is members-only and can't be played."},
	{substring: "age restrict", code: UserCodeSourceGone, message: "This video is age-restricted and can't be played here."},
	{substring: "premieres", code: UserCodeSourceGone, message: "This video hasn't premiered yet."},
	{substring: "live event", code: UserCodeLiveStream, message: "This is a live stream and can't be downloaded as a file. Try the browser mode or check back when it ends."},
	{substring: "is live", code: UserCodeLiveStream, message: "This is a live stream and can't be downloaded as a file."},

	// The source refused us, deterministically. Nothing is broken; the user
	// should wait or use a different link. Note the markers here are the
	// EXTRACTION-time refusals ("Sign in…" / 429); a 403 while fetching the
	// media BODY is classified as network-level by the body-fetch rules below
	// (a rotating egress recovers from it, so it is not a blanket ban).
	//
	// THE BOT CHECK is an IP-level, per-video gate (verified live
	// 2026-11-07: the same video bot-checks one egress and extracts cleanly
	// over another). It is a source limitation the user can retry or route
	// around, not a call for cookie configuration.
	{substring: "sign in to confirm you're not a bot", code: UserCodeBlocked, message: "The media source is limiting this server right now. Please try again in a few minutes, or send another link."},
	{substring: "sign in to confirm you’re not a bot", code: UserCodeBlocked, message: "The media source is limiting this server right now. Please try again in a few minutes, or send another link."},
	{substring: "http error 429", code: UserCodeBlocked, message: "The media source is rate-limiting this server. Please wait a minute and try again."},
	{substring: "bot check", code: UserCodeBlocked, message: "The media source is limiting this server right now. Please try again in a few minutes."},

	// Tooling gaps the user (or operator) can act on.
	{substring: "no such file or directory", code: UserCodeMissingTool, message: "A required media tool isn't installed. Ask the operator to check the yt-dlp/ffmpeg setup."},

	// Size and space: the user CAN act (smaller file / free space).
	{substring: "exceeds size limit", code: UserCodeTooLarge, message: "This file is larger than the download limit. Send a smaller file or tighten the source quality."},
	{substring: "exceeds the maximum", code: UserCodeTooLarge, message: "This file is larger than the download limit."},
	{substring: "insufficient disk space", code: UserCodeNoSpace, message: "The server doesn't have enough free disk space for this file. Ask the operator to free some space."},
	{substring: "not enough disk space", code: UserCodeNoSpace, message: "The server doesn't have enough free disk space for this file."},
	{substring: "no space left on device", code: UserCodeNoSpace, message: "The server is out of disk space. Ask the operator to free some space."},

	// Network-level failures. The route is broken, not the source; the user's
	// best action is to retry — the pipeline may pick a working route.
	//
	// The yt-dlp body-fetch markers ("unable to download video data") come
	// FIRST: a 403 while fetching the media body is the per-egress refusal a
	// rotating route (Tor) recovers from, not the blanket source ban the
	// "Sign in" / 403-on-extraction rules below describe. Order matters.
	{substring: "unable to download api page", code: UserCodeNetwork, message: "The media source interrupted the download. Please try again in a moment."},
	{substring: "unable to download webpage", code: UserCodeNetwork, message: "The media source interrupted the download. Please try again in a moment."},
	{substring: "unable to download video data", code: UserCodeNetwork, message: "The media source interrupted the download. Please try again in a moment."},
	{substring: "incomplet", code: UserCodeNetwork, message: "The download was cut short by the media server. Please try again."},
	{substring: "no such host", code: UserCodeNetwork, message: "The server couldn't resolve the media host. Please try again in a moment."},
	{substring: "dns", code: UserCodeNetwork, message: "The server couldn't resolve the media host. Please try again in a moment."},
	{substring: "connection refused", code: UserCodeNetwork, message: "The media server refused the connection. Please try again."},
	{substring: "connection reset", code: UserCodeNetwork, message: "The connection to the media server was interrupted. Retrying…"},
	{substring: "broken pipe", code: UserCodeNetwork, message: "The connection to the media server was interrupted. Retrying…"},
	{substring: "tls/ssl connection has been closed", code: UserCodeNetwork, message: "The connection to the media server was interrupted. Please try again."},
	{substring: "connection has been closed", code: UserCodeNetwork, message: "The connection to the media server was interrupted. Please try again."},
	{substring: "eof", code: UserCodeNetwork, message: "The connection to the media server dropped. Please try again."},
	{substring: "timeout", code: UserCodeNetwork, message: "The media server took too long to respond. Please try again."},
	{substring: "deadline exceeded", code: UserCodeNetwork, message: "The media server took too long to respond. Please try again."},
	{substring: "no route to host", code: UserCodeNetwork, message: "The server can't reach the media host right now. Please try again later."},
	{substring: "network is unreachable", code: UserCodeNetwork, message: "The server can't reach the media host right now. Please try again later."},
	{substring: "unable to connect to proxy", code: UserCodeNetwork, message: "The configured network route isn't working. Please try again, or ask the operator to check the proxy settings."},
	{substring: "proxy", code: UserCodeNetwork, message: "The configured network route isn't working. Please try again, or ask the operator to check the proxy settings."},
}

// userRule is one classification rule.
type userRule struct {
	substring string
	code      string
	message   string
}

// sanitizeForUser strips anything that must never reach an end user even in
// the generic fallback: host:user@ credentials, IPv4/IPv6 literals, absolute
// filesystem paths and argv-looking fragments. It is deliberately conservative
// (it never turns a sensitive string into a safe one it cannot verify), so the
// generic fallback message simply never includes raw text at all.
func sanitizeForUser(s string) string {
	// Wholesale removal of credentials: "user:pass@host" -> "host".
	if i := strings.LastIndex(s, "@"); i >= 0 {
		// Only strip when the left side parses as user:password credentials.
		if c := strings.Index(s[:i], "://"); c >= 0 && c < i-1 {
			s = s[:c+3] + s[i+1:]
		}
	}
	return s
}

// ClassifyUserError turns any error into a user-facing one. The returned error
// implements Error() with the user message and Unwrap() with the original —
// so a caller that logs the original and displays the classification can never
// confuse the two. A nil error returns nil.
//
// Callers that already know the category (a sentinel they defined) can pass it
// via classifyUserErrorWith so the taxonomy and the sentinels stay in sync.
func ClassifyUserError(err error) *UserError {
	return classifyUserErrorWith(err, "")
}

// classifyUserErrorWith honours a caller-provided code when it is non-empty,
// falling back to the rule table and finally to the honest generic message.
func classifyUserErrorWith(err error, code string) *UserError {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if code == "" {
		for _, r := range userRules {
			if strings.Contains(msg, r.substring) {
				return &UserError{Code: r.code, Message: r.message, err: err}
			}
		}
	}
	// Nothing matched. Refuse to relay raw internals: name the failure
	// honestly without the diagnostic. Only keep the (sanitized) text in the
	// wrapped error, which the operator log prints.
	switch code {
	case UserCodeUnsupported:
		return &UserError{Code: code, Message: "This link can't be played here.", err: err}
	case UserCodeBlocked:
		return &UserError{Code: code, Message: "The media source refused this server. Please try another link.", err: err}
	case UserCodeNetwork:
		return &UserError{Code: code, Message: "The server can't reach the media source right now. Please try again.", err: err}
	case UserCodeCancelled:
		return &UserError{Code: code, Message: "The operation was cancelled.", err: err}
	}
	_ = sanitizeForUser // referenced for future sanitization of fallback text
	return &UserError{
		Code:    UserCodeInternal,
		Message: "The media couldn't be prepared. Please try again, or send a different link.",
		err:     err,
	}
}

// UserFacing reports err's user-facing message, classifying it on the fly
// when it is not already a UserError. It never returns the raw technical text:
// an unknown error degrades to the honest generic message.
func UserFacing(err error) string {
	var u *UserError
	if errors.As(err, &u) {
		return u.Message
	}
	if err == nil {
		return ""
	}
	return ClassifyUserError(err).Message
}

// UserCodeOf reports err's stable category, classifying on the fly when it is
// not already a UserError. Callers may pass a raw error (the common case: the
// pipeline produces raw errors and only the presentation layer classifies).
func UserCodeOf(err error) string {
	var u *UserError
	if errors.As(err, &u) {
		return u.Code
	}
	if err == nil {
		return ""
	}
	return ClassifyUserError(err).Code
}

// IsUserCode reports whether err is (or wraps) a UserError of the given code,
// classifying a raw error when it is not already wrapped.
func IsUserCode(err error, code string) bool {
	return UserCodeOf(err) == code
}

// IsUserRecoverable reports whether a failure the user can simply retry
// (network blip, transient source throttling) rather than restructure their
// request. It drives the "please try again" phrasing and the retry policy:
// recoverable classes are worth another attempt, permanent ones are not.
func IsUserRecoverable(err error) bool {
	switch UserCodeOf(err) {
	case UserCodeNetwork, UserCodeBlocked:
		return true
	default:
		return false
	}
}

// FriendlyUserMessage returns the user-facing message for err when one is
// known and the sanitized fallback otherwise. It is the single entry point for
// surfaces that receive a raw error and must print something a user can act on.
func FriendlyUserMessage(err error) string {
	if u := ClassifyUserError(err); u != nil {
		return u.Message
	}
	return "The media couldn't be prepared. Please try again, or send a different link."
}
