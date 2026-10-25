package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/media"
)

// Classification of a media source, decided from evidence gathered in this
// order: URL syntax, known hosts, file extensions, and finally a real HTTP
// probe (Content-Type + status). The probe is what keeps extension-only
// guesses honest — a "song.mp3" that actually serves text/html must not enter
// the direct-media player, and a bare CDN link that serves video/mp4 must not
// be played as a generic web page.

// probeTimeout bounds the classification HTTP probe. Classification must never
// block the wizard: a slow server falls back to extension-based guessing.
const probeTimeout = 6 * time.Second

// maxProbeBytes bounds how much of a response body the sniffer reads. The
// Content-Type header alone is enough for classification, so the body is only
// read as a fallback (a server that omits the header entirely).
const maxProbeBytes = 512

// probeUA matches the download manager's user agent so classification and
// download see the same server behaviour.
const probeUA = "TeleMeetBot/1.0 (+media classification)"

// sniffResult is the evidence an HTTP probe collected about a URL.
type sniffResult struct {
	// contentType is the raw Content-Type header, trimmed.
	contentType string
	// statusCode of the response (0 when the probe did not complete).
	statusCode int
	// err is non-nil when the probe failed entirely (network unreachable);
	// the caller then falls back to offline heuristics.
	err error
}

// mediaMIMEOf reports whether a Content-Type names playable audio or video.
// It mirrors what AllowedContentType accepts in the download path, kept here
// so engine classification never disagrees with what the downloader accepts.
func mediaMIMEOf(ct string) bool {
	c := strings.ToLower(strings.TrimSpace(ct))
	if c == "" {
		return false
	}
	if i := strings.IndexAny(c, "; \t"); i >= 0 {
		c = c[:i]
	}
	return strings.HasPrefix(c, "video/") || strings.HasPrefix(c, "audio/")
}

// looksLikeHTMLMIME reports whether a Content-Type names an HTML document.
func looksLikeHTMLMIME(ct string) bool {
	c := strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexAny(c, "; \t"); i >= 0 {
		c = c[:i]
	}
	return c == "text/html" || c == "application/xhtml+xml"
}

// sniffMedia probes the URL with a HEAD request (falling back to a bounded
// GET when the server refuses HEAD) to learn the real Content-Type. It
// follows redirects on a bounded client, so a shortener that points at a
// media file is resolved to the file while one that points at a metadata
// page is not.
//
// The probe never returns an error the caller must handle: a failed probe is
// evidence too (offline server, bad cert) and the caller falls back to
// extension/host guessing. Only the classification is affected.
func sniffMedia(rawURL string, client *http.Client) sniffResult {
	if client == nil {
		client = defaultProbeClient()
	}
	res := sniffResult{}

	req, err := http.NewRequest(http.MethodHead, rawURL, nil)
	if err != nil {
		res.err = err
		return res
	}
	req.Header.Set("User-Agent", probeUA)
	req.Header.Set("Accept", "audio/*,video/*,text/html;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		// A network-level failure (connection refused, TLS, timeout): try a
		// bounded GET before giving up entirely.
		res.err = err
		if !probeGet(&res, rawURL, client) {
			return res
		}
		return res
	}
	// Some servers answer HEAD with 405/501: the GET fallback still learns
	// the Content-Type without downloading the whole file.
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		resp.Body.Close()
		res.err = fmt.Errorf("HEAD rejected with %d", resp.StatusCode)
		if !probeGet(&res, rawURL, client) {
			// Keep the HEAD status so the caller can treat it as an HTTP
			// error rather than a network failure.
			res.err = nil
			res.statusCode = resp.StatusCode
		}
		return res
	}
	defer resp.Body.Close()

	res.statusCode = resp.StatusCode
	res.contentType = resp.Header.Get("Content-Type")
	return res
}

// probeGet performs the bounded GET fallback, filling res on success.
func probeGet(res *sniffResult, rawURL string, client *http.Client) bool {
	getReq, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	getReq.Header.Set("User-Agent", probeUA)
	getReq.Header.Set("Accept", "audio/*,video/*,*/*;q=0.9")
	getResp, err := client.Do(getReq)
	if err != nil {
		return false
	}
	defer getResp.Body.Close()
	_, _ = io.CopyN(io.Discard, getResp.Body, maxProbeBytes)
	res.statusCode = getResp.StatusCode
	res.contentType = getResp.Header.Get("Content-Type")
	res.err = nil
	return true
}

// defaultProbeClient builds the redirect-bounded, timeout-bounded client used
// when the caller does not supply one. The probe never downloads a body, so
// redirect following is cheap; the chain is capped anyway.
//
// The transport's dialer enforces the same private-range SSRF block as the
// download path, so the probe can never be used to reach a loopback or
// link-local address the downloader itself would refuse.
func defaultProbeClient() *http.Client {
	dialer := &net.Dialer{Timeout: probeTimeout}
	return &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if ip := net.ParseIP(host); ip != nil && media.IsBlockedIP(ip) {
					return nil, fmt.Errorf("probe refused: %s is a private address", net.JoinHostPort(host, port))
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

// ClassifyMediaWithProbe classifies a link using syntax, host, extension and
// a live HTTP probe. The probe is authoritative when it completes: a URL that
// serves audio/video is MediaDirect even without an extension, and a URL with
// a media extension that serves HTML is MediaPage. When the probe cannot
// complete (offline, timeout, TLS failure, HTTP error) the offline
// heuristics decide, so behaviour in a network-isolated environment is the
// same as before.
//
// client may be nil (a bounded default is built); tests pass a stub.
//
// The SSRF gate runs before any request is issued: classification must not be
// a way to reach private ranges the downloader itself would refuse, and a URL
// that cannot pass the gate can never be played anyway.
func ClassifyMediaWithProbe(rawURL string, client *http.Client) (MediaTarget, error) {
	target, err := ClassifyMedia(rawURL)
	if err != nil {
		return MediaTarget{}, err
	}
	if _, err := media.ValidateURL(target.URL); err != nil {
		return MediaTarget{}, err
	}
	// YouTube classification is host-shape based and is never overridden by
	// a probe: youtube.com/watch?v=... serves text/html by design while still
	// being the playable YouTube page.
	if target.Kind == MediaYouTube {
		return target, nil
	}
	sniff := sniffMedia(target.URL, client)

	// The probe's MIME only wins on a successful response: a 404 does not
	// contradict an extension that says otherwise (signed URLs, HEAD-averse
	// CDNs that answer GET fine).
	if sniff.err == nil && sniff.statusCode >= 200 && sniff.statusCode < 300 {
		if mediaMIMEOf(sniff.contentType) {
			return MediaTarget{URL: target.URL, Kind: MediaDirect}, nil
		}
		if looksLikeHTMLMIME(sniff.contentType) {
			return MediaTarget{URL: target.URL, Kind: MediaPage}, nil
		}
	}
	return target, nil
}
