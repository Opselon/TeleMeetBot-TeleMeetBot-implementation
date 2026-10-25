package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Probe-based classification: the server's actual Content-Type must win over
// an extension guess, and a probe that cannot complete must fall back to the
// offline heuristics.

func TestClassifyWithProbeAudioMIMEWins(t *testing.T) {
	// A bare URL with no extension that serves audio/mpeg is direct media.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ID3"))
	}))
	defer srv.Close()

	got, err := ClassifyMediaWithProbe(srv.URL+"/track", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	if got.Kind != MediaDirect {
		t.Fatalf("kind = %q, want direct (Content-Type audio/mpeg)", got.Kind)
	}
}

func TestClassifyWithProbeHTMLOnMediaExtIsPage(t *testing.T) {
	// An .mp3 URL that actually serves HTML is a page, not direct media.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>not a song</body></html>"))
	}))
	defer srv.Close()

	got, err := ClassifyMediaWithProbe(srv.URL+"/song.mp3", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	if got.Kind != MediaPage {
		t.Fatalf("kind = %q, want page (server serves HTML)", got.Kind)
	}
}

func TestClassifyWithProbeVideoMP4NoExt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got, err := ClassifyMediaWithProbe(srv.URL+"/abcd/efgh", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	if got.Kind != MediaDirect {
		t.Fatalf("kind = %q, want direct (Content-Type video/mp4)", got.Kind)
	}
}

func TestClassifyWithProbeHTTPErrorFallsBack(t *testing.T) {
	// A 404 must not overturn a media extension: signed URLs and HEAD-averse
	// CDNs answer GET fine but may 404 on a HEAD probe.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, err := ClassifyMediaWithProbe(srv.URL+"/clip.mp4", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	if got.Kind != MediaDirect {
		t.Fatalf("kind = %q, want direct (404 keeps the extension guess)", got.Kind)
	}
}

func TestClassifyWithProbeRedirectToMedia(t *testing.T) {
	// A shortener that redirects to a media-serving path resolves to direct
	// media: redirects are followed by the probe client.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/webm")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/video", http.StatusFound)
	}))
	defer short.Close()

	got, err := ClassifyMediaWithProbe(short.URL+"/x", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	if got.Kind != MediaDirect {
		t.Fatalf("kind = %q, want direct (redirect followed to video/webm)", got.Kind)
	}
}

func TestClassifyWithProbeUnreachableFallsBack(t *testing.T) {
	// A dead host must not error: the offline heuristics decide.
	got, err := ClassifyMediaWithProbe("http://127.0.0.1:1/movie.mp4", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	if got.Kind != MediaDirect {
		t.Fatalf("kind = %q, want direct (offline fallback to extension)", got.Kind)
	}
}

func TestClassifyWithProbeHEADRejectedFallsBackToGET(t *testing.T) {
	// Servers that refuse HEAD (405) still classify via the bounded GET.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "audio/ogg")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got, err := ClassifyMediaWithProbe(srv.URL+"/audio", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	if got.Kind != MediaDirect {
		t.Fatalf("kind = %q, want direct (GET fallback saw audio/ogg)", got.Kind)
	}
}

func TestClassifyWithProbeYouTubeNeverOverridden(t *testing.T) {
	// youtube.com/watch serves text/html by design; the probe must not turn
	// it into a page. (No network: the YouTube branch returns before probing.)
	got, err := ClassifyMediaWithProbe("https://www.youtube.com/watch?v=dQw4w9WgXcQ", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	if got.Kind != MediaYouTube {
		t.Fatalf("kind = %q, want youtube", got.Kind)
	}
	if !strings.HasPrefix(got.URL, "https://www.youtube.com/watch?v=") {
		t.Fatalf("URL = %q, want normalized watch URL", got.URL)
	}
}

func TestClassifyWithProbeLoopbackNeverProbed(t *testing.T) {
	// The local file server's own URLs are direct by construction. A probe
	// would hit the loopback server (and a token that needs no validation).
	got, err := ClassifyMediaWithProbe("http://127.0.0.1:8080/d/abcdef", nil)
	if err != nil {
		t.Fatalf("ClassifyMediaWithProbe: %v", err)
	}
	// No extension and no probe: the offline classifier says page, and the
	// probe is skipped entirely (no panic, no network touch).
	if got.Kind != MediaPage {
		t.Fatalf("kind = %q, want page (loopback not probed)", got.Kind)
	}
}

func TestMediaMIMEOf(t *testing.T) {
	cases := map[string]bool{
		"audio/mpeg":            true,
		"audio/ogg":             true,
		"video/mp4":             true,
		"Video/WebM":            true,
		"audio/mpeg; charset=x": true,
		"text/html":             false,
		"application/pdf":       false,
		"":                      false,
		"application/json":      false,
	}
	for ct, want := range cases {
		if got := mediaMIMEOf(ct); got != want {
			t.Errorf("mediaMIMEOf(%q) = %v, want %v", ct, got, want)
		}
	}
}
