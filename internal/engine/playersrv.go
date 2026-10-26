package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// playerServer serves the embedded player document over loopback HTTP. Chrome
// treats file/data documents as opaque origins and some builds refuse to load
// http media from them (and Private Network Access rules are stricter for
// non-secure initiators), so the media tab is loaded from 127.0.0.1 instead.
type playerServer struct {
	mu    sync.RWMutex
	srv   *http.Server
	addr  string
	order []string
	docs  map[string]string
}

var players = &playerServer{docs: map[string]string{}}

// maxPlayerDocs bounds memory if a long-lived process restarts many sessions.
const maxPlayerDocs = 64

// playerURL registers an HTML document and returns its loopback URL.
func (ps *playerServer) playerURL(html string) (string, error) {
	base, err := ps.base()
	if err != nil {
		return "", err
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	key := hex.EncodeToString(id)

	ps.mu.Lock()
	ps.docs[key] = html
	ps.order = append(ps.order, key)
	for len(ps.order) > maxPlayerDocs {
		old := ps.order[0]
		ps.order = ps.order[1:]
		delete(ps.docs, old)
	}
	ps.mu.Unlock()

	return base + "/p/" + key, nil
}

// base lazily starts the server and returns its base URL.
func (ps *playerServer) base() (string, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.srv != nil {
		return ps.addr, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("player server listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/p/", ps.serveDoc)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	ps.srv = srv
	ps.addr = "http://" + ln.Addr().String()
	return ps.addr, nil
}

func (ps *playerServer) serveDoc(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path[len("/p/"):]
	ps.mu.RLock()
	doc, ok := ps.docs[key]
	ps.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = io.WriteString(w, doc)
}

// close stops the player server (tests and shutdown).
func (ps *playerServer) close() {
	ps.mu.Lock()
	srv := ps.srv
	ps.srv = nil
	ps.addr = ""
	ps.docs = map[string]string{}
	ps.order = nil
	ps.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}
