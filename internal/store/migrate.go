package store

// One-shot migration of legacy user preferences from state.json into the
// SQLite user store.
//
// The original TeleMeetBot kept user_prefs inside the JSON history file.
// This migration lifts them into users.db exactly once:
//
//   1. the current state.json is copied to state.json.migrated-<unix>
//      (a plain file backup, kept because it is the only copy of the
//      legacy data outside the database);
//   2. every preference is inserted with INSERT OR IGNORE, so a row the
//      user already changed in SQLite is never overwritten by an older
//      JSON value — the migration can therefore re-run safely;
//   3. state.json is rewritten without user_prefs, keeping the session
//      history byte-for-byte in meaning.
//
// A missing or unreadable state.json is not an error: there is simply
// nothing to migrate. A malformed file is left alone for Load() to rename
// into its .corrupt-<unix> quarantine.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// MigrateLegacyPrefs imports user preferences from the legacy JSON state
// file into us and rewrites the JSON file without them. It returns the
// number of imported preferences. Safe to call on every startup.
func MigrateLegacyPrefs(ctx context.Context, us *UserStore, statePath string) (int, error) {
	if us == nil {
		return 0, fmt.Errorf("migrate legacy prefs: nil user store")
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // fresh installation
		}
		return 0, fmt.Errorf("read legacy state: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, nil
	}

	// Both historical shapes carry prefs: the object payload and (in the
	// oldest format, a bare records array) none. Parse the object shape and
	// stop silently for anything else — Load() owns the file's error paths.
	var payload persistedPayload
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return 0, nil
	}
	if len(payload.Prefs) == 0 {
		return 0, nil
	}

	// Backup before touching anything.
	backup := fmt.Sprintf("%s.migrated-%d", statePath, time.Now().Unix())
	if err := os.WriteFile(backup, raw, 0o600); err != nil {
		return 0, fmt.Errorf("back up legacy state: %w", err)
	}

	// INSERT OR IGNORE semantics: never downgrade a preference the user
	// already changed after the SQLite store went live. A row that exists
	// with a non-default audio language counts as already migrated.
	imported := 0
	for key, pref := range payload.Prefs {
		userID, err := strconv.ParseInt(key, 10, 64)
		if err != nil || userID <= 0 {
			continue
		}
		existing, has, err := us.GetRaw(ctx, userID)
		if err != nil {
			return imported, fmt.Errorf("migrate preference for %d: %w", userID, err)
		}
		if has && existing.AudioLang != DefaultAudioLang {
			continue
		}
		if _, err := us.upsertSettings(ctx, userID, func(u *UserSettings) {
			if strings.TrimSpace(pref.AudioLang) != "" {
				u.AudioLang = pref.AudioLang
			}
			if strings.TrimSpace(pref.MediaMode) != "" {
				u.MediaMode = pref.MediaMode
			}
		}); err != nil {
			return imported, fmt.Errorf("migrate preference for %d: %w", userID, err)
		}
		imported++
	}
	// Strip the prefs from the JSON file so this startup is the last one
	// that sees them, whether or not anything new was copied.
	if err := rewriteLegacyStateWithoutPrefs(statePath, payload); err != nil {
		return imported, fmt.Errorf("strip legacy prefs: %w", err)
	}
	return imported, nil
}

// rewriteLegacyStateWithoutPrefs atomically replaces the legacy state file
// with a copy that has no user_prefs. Records and formatting are preserved.
func rewriteLegacyStateWithoutPrefs(path string, payload persistedPayload) error {
	payload.Prefs = nil
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stripped state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write stripped state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace stripped state: %w", err)
	}
	return nil
}
