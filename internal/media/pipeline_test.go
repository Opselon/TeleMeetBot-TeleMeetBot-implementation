package media

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProgressPercentAndETA(t *testing.T) {
	if got := percentOf(50, 200); got != 25 {
		t.Errorf("percentOf = %v, want 25", got)
	}
	if got := percentOf(10, 0); got != -1 {
		t.Errorf("percentOf unknown total = %v, want -1", got)
	}
	if got := percentOf(300, 200); got != 100 {
		t.Errorf("percentOf overflow = %v, want 100", got)
	}
	if got := etaOf(100, 400, 100); got != 3 {
		t.Errorf("etaOf = %v, want 3", got)
	}
	if got := etaOf(100, 400, 0); got != -1 {
		t.Errorf("etaOf no speed = %v, want -1", got)
	}
}

func TestReportStageCarriesUnknownPercent(t *testing.T) {
	m := &Manager{progress: newProgressBus()}
	var got Progress
	unsub := m.SubscribeProgress(func(p Progress) { got = p })
	defer unsub()
	m.reportStage("http", "https://x/y.mp4", "id1", StageConnect)
	if got.Stage != StageConnect || got.Percent != -1 || got.Kind != "http" {
		t.Fatalf("stage event = %+v", got)
	}
}

func TestIsTransientErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"timeout", &netTimeout{}, true},
		{"connreset", syscall.ECONNRESET, true},
		{"eof", io.ErrUnexpectedEOF, true},
		{"dns", errors.New("lookup x: server misbehaving"), true},
		{"http5", errors.New("fetch cdn.example: HTTP 503"), true},
		{"http4", errors.New("fetch cdn.example: HTTP 404"), false},
		{"size", errSizeLimit, false},
		{"unsupported", ErrUnsupported, false},
		{"unsafe", ErrUnsafeURL, false},
		{"disabled", ErrDisabled, false},
		{"missing tool", &ErrMissingTool{Name: "yt-dlp"}, false},
		{"ctx canceled", context.Canceled, false},
		{"permanent", errors.New("unsupported content type"), false},
	}
	for _, tc := range cases {
		if got := isTransientErr(tc.err); got != tc.want {
			t.Errorf("%s: isTransientErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type netTimeout struct{}

func (netTimeout) Error() string   { return "i/o timeout" }
func (netTimeout) Timeout() bool   { return true }
func (netTimeout) Temporary() bool { return true }

// compile-time interface check
var _ net.Error = netTimeout{}

func TestRetryTransientBounded(t *testing.T) {
	m := &Manager{progress: newProgressBus(), log: noopLogger{}}
	attempts := 0
	err := m.retryTransient(context.Background(), 3, time.Millisecond, "t", "id", func(attempt int) error {
		attempts++
		return errors.New("fetch x: HTTP 503")
	})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryTransientPermanentFailsFast(t *testing.T) {
	m := &Manager{progress: newProgressBus(), log: noopLogger{}}
	attempts := 0
	_ = m.retryTransient(context.Background(), 3, time.Millisecond, "t", "id", func(attempt int) error {
		attempts++
		return errSizeLimit
	})
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (permanent verdict must fail fast)", attempts)
	}
}

func TestRetryTransientRecovers(t *testing.T) {
	m := &Manager{progress: newProgressBus(), log: noopLogger{}}
	attempts := 0
	err := m.retryTransient(context.Background(), 3, time.Millisecond, "t", "id", func(attempt int) error {
		attempts++
		if attempt == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestRetryTransientStopsOnContext(t *testing.T) {
	m := &Manager{progress: newProgressBus(), log: noopLogger{}}
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_ = m.retryTransient(ctx, 5, time.Millisecond, "t", "id", func(attempt int) error {
		attempts++
		cancel() // user gave up mid-retry
		return io.EOF
	})
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
