// Package userstate is the authoritative, observable view of what every
// Telegram user is doing right now.
//
// The bot already keeps per-user wizard flows, pending files and live session
// bindings in several maps. This package does not replace them — it is the
// single tracking layer above them, so the web dashboard and diagnostics can
// answer "who is in which state" without reverse-engineering the bot's
// internals. Every transition is logged (with the PIPELINE:userstate tag, so
// the log recorder's component filter picks it up) and every state carries a
// timestamp, so a stuck user is visible in one glance.
package userstate

import (
	"sort"
	"sync"
	"time"
)

// State is the coarse per-user state shown on dashboards.
type State string

const (
	// StateIdle means the user has no open interaction (or is unknown).
	StateIdle State = "idle"
	// StateWizardMeet is the creation wizard waiting for a Meet link.
	StateWizardMeet State = "awaiting_meet_url"
	// StateWizardMedia is the creation wizard waiting for a media link.
	StateWizardMedia State = "awaiting_media_url"
	// StateReviewing is the creation wizard showing the review card.
	StateReviewing State = "reviewing_configuration"
	// StateSwitchingMedia is the switch flow waiting for the new media link.
	StateSwitchingMedia State = "awaiting_switch_media"
	// StateAwaitingText is any other text prompt (rename, guest name,
	// broadcast payload, queue title...). Detail carries the prompt.
	StateAwaitingText State = "awaiting_text"
	// StateStagingMedia means the user just forwarded Telegram media and it
	// is staged (pending card).
	StateStagingMedia State = "staging_media"
	// StateLive means the user owns at least one live session and no open
	// wizard; Detail carries the session ids.
	StateLive State = "live"
)

// Display is the human label for cards and dashboards.
func (s State) Display() string {
	switch s {
	case StateWizardMeet:
		return "Creating stream — awaiting Meet link"
	case StateWizardMedia:
		return "Creating stream — awaiting media link"
	case StateReviewing:
		return "Reviewing configuration"
	case StateSwitchingMedia:
		return "Switching media — awaiting new link"
	case StateAwaitingText:
		return "Awaiting text input"
	case StateStagingMedia:
		return "Staging media"
	case StateLive:
		return "Live session"
	default:
		return "Idle"
	}
}

// Info is one user's current state.
type Info struct {
	UserID      int64     `json:"user_id"`
	Username    string    `json:"username,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	State       State     `json:"state"`
	Detail      string    `json:"detail,omitempty"`
	Since       time.Time `json:"since"`   // when the user entered this state
	Updated     time.Time `json:"updated"` // last touch of any kind
}

// Tracker holds per-user state. It is safe for concurrent use and never
// blocks: the bot calls it on the update hot path.
type Tracker struct {
	mu    sync.RWMutex
	users map[int64]*Info
	// onTransition is called synchronously after every state change. The bot
	// uses it to feed the log recorder; the dashboard relies on the tracker
	// itself, not this hook.
	onTransition func(from, to Info)
	now          func() time.Time
}

// New builds a Tracker. onTransition may be nil.
func New(onTransition func(from, to Info)) *Tracker {
	return &Tracker{
		users:        map[int64]*Info{},
		onTransition: onTransition,
		now:          time.Now,
	}
}

// Set records a state transition. Setting StateIdle is equivalent to Clear.
// Username/DisplayName update the identity metadata without touching the
// state when the state itself is unchanged.
func (t *Tracker) Set(userID int64, st State, detail, username, displayName string) {
	if userID == 0 {
		return
	}
	t.mu.Lock()
	prev, existed := t.users[userID]
	from := Info{UserID: userID}
	if existed {
		from = *prev
	}
	if st == StateIdle {
		// Setting idle is Clear: idle users are not tracked.
		delete(t.users, userID)
		t.mu.Unlock()
		if t.onTransition != nil && existed && from.State != StateIdle {
			idle := from
			idle.State = StateIdle
			idle.Detail = ""
			t.onTransition(from, idle)
		}
		return
	}
	now := t.now()
	info := &Info{
		UserID:      userID,
		Username:    firstNonEmpty(username, from.Username),
		DisplayName: firstNonEmpty(displayName, from.DisplayName),
		State:       st,
		Detail:      detail,
		Since:       now,
		Updated:     now,
	}
	if existed && prev.State == st {
		// Same state: keep the original Since (it answers "how long has the
		// user been stuck here"), refresh only the detail.
		info.Since = prev.Since
		if info.Detail == "" {
			info.Detail = prev.Detail
		}
	}
	t.users[userID] = info
	t.mu.Unlock()

	if t.onTransition != nil && (!existed || from.State != st) {
		t.onTransition(from, *info)
	}
}

// Clear returns the user to idle and drops the entry (idle users are not
// tracked: the list only shows active interactions).
func (t *Tracker) Clear(userID int64) {
	t.mu.Lock()
	prev, existed := t.users[userID]
	delete(t.users, userID)
	t.mu.Unlock()
	if t.onTransition != nil && existed {
		idle := *prev
		idle.State = StateIdle
		idle.Detail = ""
		t.onTransition(*prev, idle)
	}
}

// Get returns the user's state (StateIdle when absent).
func (t *Tracker) Get(userID int64) Info {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if u, ok := t.users[userID]; ok {
		return *u
	}
	return Info{UserID: userID, State: StateIdle}
}

// List returns every tracked user, idle ones excluded, oldest interaction
// first (the user who has been stuck longest is on top).
func (t *Tracker) List() []Info {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Info, 0, len(t.users))
	for _, u := range t.users {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Count summarizes the tracker by state.
func (t *Tracker) Count() map[State]int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := map[State]int{}
	for _, u := range t.users {
		out[u.State]++
	}
	return out
}

// Prune drops entries untouched for longer than maxAge. Called by the bot's
// refresh loop; returns the number pruned.
func (t *Tracker) Prune(maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for id, u := range t.users {
		if t.now().Sub(u.Updated) > maxAge {
			delete(t.users, id)
			n++
		}
	}
	return n
}

// Len is the number of tracked users.
func (t *Tracker) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.users)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
