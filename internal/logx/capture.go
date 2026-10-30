package logx

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// now is a tiny seam so tests can assert on entry times deterministically.
var now = time.Now

// panicDigest renders a one-line-per-frame digest of a panic. The full
// stack belongs in stderr (where slog already writes it when the caller
// re-panics); the recorder keeps the compressed form so the ring stays
// useful after one chatty crash loop.
func panicDigest(where string, r interface{}) string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	frames := strings.Split(strings.TrimSpace(string(buf[:n])), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "panic in %s: %v", where, r)
	// runtime.Stack format: first line "goroutine N [running]:", then
	// pairs of function + file:line. Keep up to 6 function frames.
	kept := 0
	for i := 1; i < len(frames) && kept < 6; i++ {
		line := strings.TrimSpace(frames[i])
		if line == "" || strings.HasPrefix(line, "goroutine") {
			continue
		}
		if !strings.Contains(line, "(") {
			continue // the file:line row; the function row above carries the name
		}
		if strings.Contains(line, "panicDigest") || strings.Contains(line, "CatchPanic") {
			continue
		}
		b.WriteString(" | ")
		b.WriteString(line)
		kept++
	}
	return b.String()
}
