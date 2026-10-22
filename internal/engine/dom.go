package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// clickByText clicks the first visible button/link whose text or aria-label
// contains one of the (lower-case) patterns. It retries until timeout and
// returns the matched label.
func clickByText(p *rod.Page, patterns []string, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	jsPats, _ := json.Marshal(patterns)
	for time.Now().Before(deadline) {
		js := fmt.Sprintf(`() => {
  const pats = %s;
  const norm = s => (s || "").replace(/\s+/g, " ").trim().toLowerCase();
  const els = Array.from(document.querySelectorAll('button, [role=button], [role=menuitem], a, [aria-label]'));
  for (const el of els) {
    const label = norm((el.getAttribute('aria-label') || "") + " " + (el.innerText || el.textContent || ""));
    if (!label) continue;
    if (!pats.some(p => label.includes(p))) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    try { el.scrollIntoView({block: 'center'}); } catch (e) {}
    el.click();
    return label.slice(0, 80);
  }
  return "";
}`, jsPats)
		res, err := p.Timeout(3 * time.Second).Eval(js)
		if err == nil {
			if label := strings.TrimSpace(res.Value.Str()); label != "" {
				return true, label
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false, ""
}
