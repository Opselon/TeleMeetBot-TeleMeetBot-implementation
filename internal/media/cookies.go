package media

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Netscape cookie jar loading for the yt-dlp --cookies path.
//
// YouTube's "Sign in to confirm you're not a bot" is an ACCOUNT gate: it
// follows the account, not the IP, so neither the egress ladder nor a
// different datacenter can answer it. The one supported remedy is a real
// authenticated session (SID/HSID/__Secure-1PSID cookies) exported from a
// logged-in browser.
//
// This file parses the Netscape format yt-dlp itself documents
// (https://everything.curl.dev/http/cookies) so the jar serves BOTH halves
// of the pipeline:
//   - yt-dlp gets it as --cookies FILE, unchanged;
//   - the engine's media tab gets the same cookies injected via CDP, so the
//     watch page clears the same gate instead of dying on LOGIN_REQUIRED.
//
// A connected Google Account is NOT this: it holds OAuth Bearer tokens,
// and Google prohibits converting OAuth tokens into first-party session
// cookies. The tokens authenticate the user to Google APIs; they do not
// log a browser into youtube.com.

// LoadCookiesFile parses a Netscape-format cookie jar. It returns the
// cookies in CDP-injectable form. A file that cannot be read or parsed
// yields an error — never a silent empty jar, which would look like
// "the bot check won" instead of "the config is wrong".
func LoadCookiesFile(path string) ([]*http.Cookie, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	return parseNetscapeCookies(path)
}

// parseNetscapeCookies reads and parses the jar.
func parseNetscapeCookies(path string) ([]*http.Cookie, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cookies_file: %w", err)
	}
	defer f.Close()

	var out []*http.Cookie
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// The format's own comments and the header line start with '#'.
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// domain<TAB>include_subdomains<TAB>path<TAB>secure<TAB>expires<TAB>name<TAB>value
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue // a malformed line is skipped, not fatal
		}
		domain, pathAttr, secure, name, value := fields[0], fields[2], strings.EqualFold(fields[3], "TRUE"), fields[5], fields[6]
		c := &http.Cookie{
			Name:   name,
			Value:  value,
			Domain: strings.TrimPrefix(domain, "."),
			Path:   pathAttr,
			Secure: secure,
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cookies_file: read %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cookies_file: %s contains no cookies (expected Netscape format: domain<TAB>subdomains<TAB>path<TAB>secure<TAB>expires<TAB>name<TAB>value)", path)
	}
	return out, nil
}
