package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// Session control: the same operations the Telegram card offers, over JSON.

// startRequest is the POST /api/session/start body.
type startRequest struct {
	MeetURL   string   `json:"meet_url"`
	MediaURL  string   `json:"media_url"`
	Mode      string   `json:"mode"`
	Queue     []string `json:"queue"`
	Volume    int      `json:"volume"`
	Speed     float64  `json:"speed"`
	GuestName string   `json:"guest_name"`
	AudioLang string   `json:"audio_lang"`
}

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Engine == nil {
		s.writeErr(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "invalid JSON body: %v", err)
		return
	}
	req.MeetURL = strings.TrimSpace(req.MeetURL)
	req.MediaURL = strings.TrimSpace(req.MediaURL)
	if req.MeetURL == "" || req.MediaURL == "" {
		s.writeErr(w, http.StatusBadRequest, "meet_url and media_url are both required")
		return
	}
	er := engine.StartRequest{
		MeetLink:  req.MeetURL,
		MediaLink: req.MediaURL,
		PlayMode:  req.Mode,
		Queue:     req.Queue,
		Volume:    req.Volume,
		Speed:     req.Speed,
		GuestName: req.GuestName,
		AudioLang: req.AudioLang,
	}
	sess, err := s.deps.Engine.Start(r.Context(), er)
	if err != nil {
		s.log.Warnf("[PIPELINE:dashboard] start failed: %v", err)
		s.writeErr(w, http.StatusInternalServerError, "start failed: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "session": viewOf(sess)})
}

func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Engine == nil {
		s.writeErr(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	id := engine.SessionID(strings.TrimSpace(r.URL.Query().Get("id")))
	if id == "" {
		s.writeErr(w, http.StatusBadRequest, "missing session id")
		return
	}
	if s.deps.Engine.Get(id) == nil {
		s.writeErr(w, http.StatusNotFound, "no such session: %s", id)
		return
	}
	if err := s.deps.Engine.Stop(id); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "stop failed: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// actionRequest is the POST /api/session/action body.
type actionRequest struct {
	Action string      `json:"action"`
	Arg    interface{} `json:"arg"`
}

// applyAction performs one named action on the session, mirroring the
// Telegram card buttons (and nothing else: the dashboard never widens the
// control surface beyond what the bot exposes).
func applyAction(sess *engine.Session, req actionRequest) error {
	switch req.Action {
	case "pause":
		return sess.Pause()
	case "resume":
		return sess.Resume()
	case "mute":
		return sess.SetMuted(true)
	case "unmute":
		return sess.SetMuted(false)
	case "volume":
		v, ok := toInt(req.Arg)
		if !ok || v < 0 || v > 100 {
			return errors.New("volume must be 0-100")
		}
		return sess.SetVolume(v)
	case "speed":
		f, ok := toFloat(req.Arg)
		if !ok || f < 0.25 || f > 4 {
			return errors.New("speed must be 0.25-4")
		}
		return sess.SetRate(f)
	case "seek_back_10":
		return sess.Seek(-10)
	case "seek_fwd_10":
		return sess.Seek(10)
	case "seek_back_30":
		return sess.Seek(-30)
	case "seek_fwd_30":
		return sess.Seek(30)
	case "restart":
		return sess.RestartMedia()
	case "next":
		return sess.Next()
	case "previous":
		return sess.Previous()
	default:
		return errors.New("unknown action: " + req.Action)
	}
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func (s *Server) handleSessionAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := engine.SessionID(strings.TrimSpace(r.URL.Query().Get("id")))
	if id == "" {
		s.writeErr(w, http.StatusBadRequest, "missing session id")
		return
	}
	var req actionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "invalid JSON body: %v", err)
		return
	}
	sess := s.deps.Engine.Get(id)
	if sess == nil {
		s.writeErr(w, http.StatusNotFound, "no such session: %s", id)
		return
	}
	if err := applyAction(sess, req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
