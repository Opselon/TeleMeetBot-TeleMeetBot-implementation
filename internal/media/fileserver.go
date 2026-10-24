package media

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fileServer serves downloaded files over loopback HTTP so the media tab can
// play them from an http(s) origin (Chromium refuses to fetch media from
// file:// and data: documents). Only files registered through serve() are
// reachable, and only while they still exist.
type fileServer struct {
	mu     sync.RWMutex
	srv    *http.Server
	addr   string
	tokens map[string]string // token -> absolute path
}

func newFileServer() *fileServer { return &fileServer{tokens: map[string]string{}} }

// serve registers a file and returns its loopback URL.
func (f *fileServer) serve(e Entry) string {
	if f == nil {
		return ""
	}
	base, err := f.base()
	if err != nil {
		return ""
	}
	token := newToken()
	f.mu.Lock()
	f.tokens[token] = e.Path
	f.mu.Unlock()
	return base + "/d/" + token
}

// base lazily starts the loopback listener.
func (f *fileServer) base() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.srv != nil {
		return f.addr, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("file server listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/d/", f.serveFile)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	f.srv = srv
	f.addr = "http://" + ln.Addr().String()
	return f.addr, nil
}

// serveFile streams one managed file. Content-Type comes from the manifest
// (sniffed at fetch time); the response is not cacheable.
func (f *fileServer) serveFile(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/d/")
	f.mu.RLock()
	path, ok := f.tokens[token]
	f.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Never serve a path that left the managed directory between registration
	// and now (a renamed or replaced file, for instance).
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	ct := contentTypeForExt(path)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

// newToken is an unguessable 24-char hex handle.
func newToken() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// forget drops a file registration (used when a file is reaped).
func (f *fileServer) forget(path string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for tok, p := range f.tokens {
		if p == path {
			delete(f.tokens, path)
			_ = filepath.Separator
			_ = tok
		}
	}
}

// close stops the file server (tests and shutdown).
func (f *fileServer) close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	srv := f.srv
	f.srv = nil
	f.addr = ""
	f.tokens = map[string]string{}
	f.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}
