package googleauth

import (
	"os"
	"testing"
	"time"
)

type testLogger struct{}

func (l *testLogger) Infof(format string, args ...any)  {}
func (l *testLogger) Warnf(format string, args ...any)  {}
func (l *testLogger) Errorf(format string, args ...any) {}
func (l *testLogger) Debugf(format string, args ...any) {}

const sampleNetscapeCookies = `# Netscape HTTP Cookie File
# http://curl.haxx.se/rfc/cookie_spec.html
# This is a generated file!  Do not edit.

.google.com	TRUE	/	TRUE	1893456000	SID	test-sid-token-value
#HttpOnly_.google.com	TRUE	/	TRUE	1893456000	HSID	test-hsid-token-value
.google.com	TRUE	/	TRUE	1893456000	SAPISID	test-sapisid-token-value
.google.com	TRUE	/	TRUE	1893456000	__Secure-1PSID	test-sec1psid-token-value
.youtube.com	TRUE	/	TRUE	1893456000	LOGIN_INFO	test-login-info-value
.example.com	TRUE	/	FALSE	1893456000	TRACK	other-domain-track
`

const sampleJSONCookies = `[
  {
    "domain": ".google.com",
    "name": "SID",
    "value": "json-sid-token",
    "path": "/",
    "secure": true,
    "httpOnly": false,
    "expirationDate": 1893456000
  },
  {
    "domain": ".google.com",
    "name": "SAPISID",
    "value": "json-sapisid-token",
    "path": "/",
    "secure": true,
    "httpOnly": true,
    "expirationDate": 1893456000
  }
]`

func TestParseNetscapeCookies(t *testing.T) {
	cookies, err := ParseCookies([]byte(sampleNetscapeCookies))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	if len(cookies) != 5 {
		t.Fatalf("expected 5 google/youtube cookies (filtering example.com), got %d", len(cookies))
	}

	foundHSID := false
	for _, c := range cookies {
		if c.Name == "HSID" {
			foundHSID = true
			if !c.HTTPOnly {
				t.Errorf("expected HSID to be HTTPOnly from #HttpOnly_ line")
			}
			if !c.Secure {
				t.Errorf("expected HSID to be Secure")
			}
		}
	}
	if !foundHSID {
		t.Errorf("HSID cookie not found in parsed result")
	}
}

func TestParseJSONCookies(t *testing.T) {
	cookies, err := ParseCookies([]byte(sampleJSONCookies))
	if err != nil {
		t.Fatalf("unexpected JSON parse error: %v", err)
	}

	if len(cookies) != 2 {
		t.Fatalf("expected 2 cookies, got %d", len(cookies))
	}
	if cookies[0].Name != "SID" || cookies[0].Value != "json-sid-token" {
		t.Errorf("unexpected cookie 0: %+v", cookies[0])
	}
}

func TestParseRejectsInvalidData(t *testing.T) {
	// Empty
	if _, err := ParseCookies([]byte("")); err == nil {
		t.Errorf("expected error on empty data")
	}

	// Non-Google domain only
	nonGoogle := `.other.com	TRUE	/	TRUE	1893456000	SID	val`
	if _, err := ParseCookies([]byte(nonGoogle)); err == nil {
		t.Errorf("expected error on non-google domain")
	}

	// Google domain but missing auth session markers
	analyticsOnly := `.google.com	TRUE	/	FALSE	1893456000	NID	random-nid`
	if _, err := ParseCookies([]byte(analyticsOnly)); err == nil {
		t.Errorf("expected error on missing session markers")
	}
}

func TestManagerMultiTenantIsolation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "googleauth-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr := New(tmpDir, &testLogger{})

	userA := int64(1001)
	userB := int64(1002)

	// Initially neither has an account
	sumA := mgr.Summary(userA)
	if sumA.Connected {
		t.Errorf("userA should not be connected initially")
	}

	// Save for userA
	sumAafter, err := mgr.Save(userA, []byte(sampleNetscapeCookies))
	if err != nil {
		t.Fatalf("save userA failed: %v", err)
	}
	if !sumAafter.Connected || sumAafter.CookieCount != 5 {
		t.Errorf("unexpected summary for userA: %+v", sumAafter)
	}

	// Verify userB is still isolated and not connected
	sumB := mgr.Summary(userB)
	if sumB.Connected {
		t.Errorf("userB should not be connected (leakage from userA!)")
	}

	paramsB, err := mgr.GetCookieParams(userB)
	if err != nil {
		t.Fatalf("get params userB error: %v", err)
	}
	if len(paramsB) != 0 {
		t.Errorf("userB got %d cookie params, expected 0", len(paramsB))
	}

	// UserA has params
	paramsA, err := mgr.GetCookieParams(userA)
	if err != nil {
		t.Fatalf("get params userA error: %v", err)
	}
	if len(paramsA) != 5 {
		t.Errorf("userA expected 5 params, got %d", len(paramsA))
	}

	// Delete userA
	if err := mgr.Delete(userA); err != nil {
		t.Fatalf("delete userA error: %v", err)
	}

	sumAfinally := mgr.Summary(userA)
	if sumAfinally.Connected {
		t.Errorf("userA should be disconnected after deletion")
	}
}

func TestAccountSummaryExpiry(t *testing.T) {
	pastEpoch := time.Now().Add(-1 * time.Hour).Unix()
	expiredCookie := []Cookie{
		{
			Name:    "SID",
			Value:   "exp-val",
			Domain:  ".google.com",
			Path:    "/",
			Expires: float64(pastEpoch),
		},
	}
	prof := UserProfile{
		UserID:    123,
		UpdatedAt: time.Now(),
		Cookies:   expiredCookie,
	}

	s := summaryFromProfile(prof)
	if !s.Connected {
		t.Errorf("expected connected")
	}
	if !s.HasExpired {
		t.Errorf("expected cookie to report expired")
	}
}
