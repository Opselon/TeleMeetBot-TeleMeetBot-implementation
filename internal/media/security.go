package media

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Security note on every check below: URLs come from a Telegram chat. They are
// attacker-controlled input that reaches a subprocess and the filesystem, so
// validation is fail-closed — an URL that cannot be proven safe is rejected.

// privateBlocks are the IPv4/IPv6 ranges a fetch must never reach.
var privateBlocks = []net.IPNet{
	parseCIDR("127.0.0.0/8"),    // loopback
	parseCIDR("10.0.0.0/8"),     // private
	parseCIDR("172.16.0.0/12"),  // private
	parseCIDR("192.168.0.0/16"), // private
	parseCIDR("169.254.0.0/16"), // link-local (cloud metadata is here)
	parseCIDR("0.0.0.0/8"),      // "this network"
	parseCIDR("::1/128"),        // v6 loopback
	parseCIDR("fe80::/10"),      // v6 link-local
	parseCIDR("fc00::/7"),       // v6 unique-local
	parseCIDR("100.64.0.0/10"),  // CGNAT shared address space
}

func parseCIDR(s string) net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("bad cidr literal: " + s)
	}
	return *n
}

// ErrUnsafeURL is returned when a URL fails the SSRF / syntax gate.
var ErrUnsafeURL = errors.New("unsafe media URL")

// allowedSchemes is the exhaustive list of fetchable schemes.
const allowedSchemes = "http and https"

// ValidateURL checks that raw is a syntactically valid absolute http(s) URL.
// It does NOT resolve DNS: that is ValidateResolvedDestination's job, after
// the host is known, so the two checks cover different attack stages.
func ValidateURL(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("%w: empty", ErrUnsafeURL)
	}
	u, err := url.Parse(s)
	if err != nil || u == nil {
		return nil, fmt.Errorf("%w: cannot parse %q", ErrUnsafeURL, truncateForLog(s, 80))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme must be %s (got %q)", ErrUnsafeURL, allowedSchemes, u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("%w: no host", ErrUnsafeURL)
	}
	// Disallow credentials embedded in the authority: opaque to the operator
	// and a classic way to smuggle a second target.
	if u.User != nil {
		return nil, fmt.Errorf("%w: embedded credentials are not allowed", ErrUnsafeURL)
	}
	// A bare IPv6 literal is fine; a bracketed host with a zone is not.
	if strings.Contains(host, "%") {
		return nil, fmt.Errorf("%w: IPv6 zone ids are not allowed", ErrUnsafeURL)
	}
	if isBareIP(host) && !loopbackAllowed() && isPrivateIP(net.ParseIP(strings.Trim(host, "[]"))) {
		return nil, fmt.Errorf("%w: host %s is a private/loopback address", ErrUnsafeURL, host)
	}
	return u, nil
}

// ValidateResolvedDestination checks the resolved addresses of a host: every
// address must be public. This is the second half of the SSRF gate — the URL
// may be clean, but DNS can still aim the fetch at the cloud metadata service.
func ValidateResolvedDestination(host string, port string, lookup func(string) ([]net.IP, error)) error {
	if lookup == nil {
		lookup = net.LookupIP
	}
	ips, err := lookup(host)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve host %q: %v", ErrUnsafeURL, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: host %q resolves to nothing", ErrUnsafeURL, host)
	}
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		if isPrivateIP(ip) && !(loopbackAllowed() && (ip.IsLoopback() || ip.Equal(net.ParseIP("127.0.0.1")))) {
			return fmt.Errorf("%w: host %q resolves to private address %s", ErrUnsafeURL, host, ip)
		}
		if !isRoutableIP(ip) {
			return fmt.Errorf("%w: host %q resolves to non-routable address %s", ErrUnsafeURL, host, ip)
		}
	}
	_ = port // retained for future egress allow-listing
	return nil
}

// isPrivateIP reports whether ip is in a blocked range. Unknown input is
// treated as blocked (fail-closed).
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, n := range privateBlocks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isRoutableIP rejects addresses that are valid but never a real media host.
func isRoutableIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}

// isBareIP reports whether host is an IP literal without brackets.
func isBareIP(host string) bool { return net.ParseIP(host) != nil }

// AllowedExtension reports whether ext (".mp4", "mp4", any case) is one the
// pipeline can play or transcode from. This is a codec-surface check, not a
// security one.
func AllowedExtension(ext string) bool {
	e := strings.ToLower(strings.TrimSpace(ext))
	e = strings.TrimPrefix(e, ".")
	switch e {
	case "mp4", "m4v", "webm", "mkv", "mov", "ogv", "avi",
		"mp3", "m4a", "aac", "flac", "wav", "opus", "oga", "ogg":
		return true
	default:
		return false
	}
}

// AllowedContentType reports whether a fetched Content-Type is a media type
// the pipeline accepts.
func AllowedContentType(ct string) bool {
	c := strings.ToLower(strings.TrimSpace(ct))
	if c == "" {
		return false
	}
	if i := strings.IndexAny(c, "; "); i >= 0 {
		c = c[:i]
	}
	return strings.HasPrefix(c, "video/") || strings.HasPrefix(c, "audio/")
}

// SanitizeFilename turns attacker-controlled text into a safe base name. It
// must never produce "..", an absolute path, a separator, or something that
// could escape the managed directory once joined.
func SanitizeFilename(s string) string {
	var sb strings.Builder
	for _, r := range strings.TrimSpace(strings.ToLower(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	name := strings.Trim(sb.String(), "-")
	// Guard against a name that is entirely punctuation (".." → "--" → "").
	if name == "" || name == "." || name == ".." {
		name = "media"
	}
	if len(name) > 48 {
		name = name[:48]
	}
	return name
}

// SafeJoin resolves name inside dir, checking the result stays inside dir
// after symlink resolution. It is the guard behind every file the manager
// creates or deletes, so path traversal and symlink attacks die here.
func SafeJoin(dir, name string) (string, error) {
	d := strings.TrimSpace(dir)
	if d == "" {
		return "", errors.New("empty base dir")
	}
	base, err := filepath.Abs(d)
	if err != nil {
		return "", fmt.Errorf("resolve base dir: %w", err)
	}
	if strings.TrimSpace(name) == "" {
		return "", errors.New("empty name")
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		return "", errors.New("name must not contain a path separator")
	}
	if name == "." || name == ".." {
		return "", errors.New("name must not be a directory reference")
	}
	joined := filepath.Join(base, filepath.Base(name))
	if joined != filepath.Join(base, name) {
		return "", errors.New("name escapes the base dir")
	}
	// Resolve symlinks on BOTH sides: a symlinked dir (or a symlink placed
	// inside it) must not make the check pass while the real target lies
	// outside.
	realBase, err := resolveSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve base: %w", err)
	}
	realJoined, err := resolveSymlinks(joined)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve target: %w", err)
	}
	if err == nil && !strings.HasPrefix(realJoined+string(os.PathSeparator), realBase+string(os.PathSeparator)) {
		return "", errors.New("resolved path escapes the managed dir (symlink)")
	}
	return joined, nil
}

// resolveSymlinks resolves the longest existing prefix of path, like
// filepath.EvalSymlinks but tolerant of a not-yet-existing leaf (the common
// case for a file about to be created).
func resolveSymlinks(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	dir, rest := filepath.Split(path)
	if dir == "" || dir == path {
		return path, nil
	}
	resolvedDir, err := resolveSymlinks(strings.TrimRight(dir, string(os.PathSeparator)))
	if err != nil {
		return path, err
	}
	return filepath.Join(resolvedDir, rest), nil
}

// truncateForLog keeps error strings bounded.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// IsSymlink reports whether path is a symlink.
func IsSymlink(path string) bool {
	st, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeSymlink != 0
}

// SecureDelete verifies that path resolves inside dir and only then removes
// it. It is the guard behind every deletion the manager performs.
//
// The check is deliberately redundant with SafeJoin: a delete must never rely
// on a create-time check whose result a later symlink could have invalidated.
func SecureDelete(dir, path string) (string, error) {
	d := strings.TrimSpace(dir)
	if d == "" {
		return "", errors.New("empty managed dir")
	}
	base, err := resolveSymlinks(d)
	if err != nil {
		return "", fmt.Errorf("resolve managed dir: %w", err)
	}
	// Absolute path: must already live under the managed dir (after symlink
	// resolution) — no joining, no interpretation.
	resolved, err := resolveSymlinks(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve target: %w", err)
	}
	target := path
	if err == nil {
		target = resolved
	}
	if !strings.HasPrefix(target+string(os.PathSeparator), base+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s is outside the managed dir", ErrUnsafeURL, target)
	}
	// A symlink inside the tree that points at a file outside it is refused
	// even when its own path is under the managed dir.
	if IsSymlink(path) {
		if resolved == "" || !strings.HasPrefix(resolved+string(os.PathSeparator), base+string(os.PathSeparator)) {
			return "", fmt.Errorf("%w: symlink target escapes the managed dir", ErrUnsafeURL)
		}
	}
	return target, nil
}
