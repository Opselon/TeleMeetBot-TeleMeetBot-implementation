package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Telegram Login Widget verification, per
// https://core.telegram.org/widgets/login#checking-authorization
//
// The widget delivers: id, first_name, last_name, username, photo_url,
// auth_date, hash. data_check_string is every received field except hash,
// formatted "key=value" and joined with \n in alphabetical order.
// hash = HMAC-SHA256(data_check_string, SHA256(bot_token)), hex-encoded.
//
// authDateMaxAge bounds replay: Telegram recommends rejecting payloads older
// than a day.
const authDateMaxAge = 24 * time.Hour

// TelegramAuth is the parsed Login Widget payload.
type TelegramAuth struct {
	ID        int64
	FirstName string
	LastName  string
	Username  string
	PhotoURL  string
	AuthDate  int64 // unix seconds
	Hash      string
}

// VerifyTelegramAuth checks the payload signature and freshness. It returns
// the verified identity; err explains every rejection. Empty and malformed
// input is an error, never a panic.
func VerifyTelegramAuth(token string, q map[string]string, now time.Time) (TelegramAuth, error) {
	var a TelegramAuth
	if strings.TrimSpace(token) == "" {
		return a, fmt.Errorf("bot token is not configured")
	}
	a.Hash = q["hash"]
	if a.Hash == "" {
		return a, fmt.Errorf("payload is missing the hash field")
	}

	// data_check_string: every field except hash, sorted by key.
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "hash" || k == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(q[k])
	}

	key := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(b.String()))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(a.Hash))) {
		return a, fmt.Errorf("signature mismatch")
	}

	id, err := strconv.ParseInt(q["id"], 10, 64)
	if err != nil || id <= 0 {
		return a, fmt.Errorf("payload has no valid user id")
	}
	a.ID = id
	a.FirstName = q["first_name"]
	a.LastName = q["last_name"]
	a.Username = q["username"]
	a.PhotoURL = q["photo_url"]
	// auth_date is a signed field (it participates in the hash), so the
	// freshness check cannot be bypassed by resending an old payload: the
	// signature would no longer match the edited auth_date.
	ad, err := strconv.ParseInt(q["auth_date"], 10, 64)
	if err != nil || ad <= 0 {
		return a, fmt.Errorf("payload has no valid auth_date")
	}
	a.AuthDate = ad
	if now.Sub(time.Unix(ad, 0)) > authDateMaxAge {
		return a, fmt.Errorf("login payload expired (auth_date is older than 24h)")
	}
	return a, nil
}

// DisplayName is the verified user's best-effort display name.
func (a TelegramAuth) DisplayName() string {
	name := strings.TrimSpace(a.FirstName + " " + a.LastName)
	if name == "" {
		name = a.Username
	}
	return name
}
