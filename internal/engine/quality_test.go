package engine

// Video-quality tests: policy lifecycle on the session, per-item overrides
// across queue navigation, observed-quality reporting and the requested vs.
// selected vs. observed distinctions in the Status projection.

import (
	"context"
	"sync"
	"testing"
)

func TestQualityPolicyRoundTrip(t *testing.T) {
	s := newQueueSession(t, 2)
	if got := s.QualityPolicy(); got != QualityAuto {
		t.Fatalf("initial policy = %q, want auto", got)
	}
	if err := s.SetQualityPolicy(QualityFHD1080); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := s.QualityPolicy(); got != QualityFHD1080 {
		t.Fatalf("policy = %q", got)
	}
	// Unknown policies degrade to auto instead of poisoning the session.
	if err := s.SetQualityPolicy("8k-ultra"); err != nil {
		t.Fatalf("set bogus: %v", err)
	}
	if got := s.QualityPolicy(); got != QualityAuto {
		t.Fatalf("bogus policy = %q, want auto", got)
	}
	if st := s.Snapshot(); st.QualityPolicy != QualityAuto {
		t.Fatalf("status policy = %q", st.QualityPolicy)
	}
}

func TestStartRequestQualitySeedsSession(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, dummyLogger{})
	s, err := mgr.Start(context.Background(), StartRequest{
		MeetLink:  "https://meet.google.com/abc-defg-hij",
		MediaLink: "https://example.com/a.mp4",
		Quality:   "saver",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	if got := s.QualityPolicy(); got != QualitySaver {
		t.Fatalf("session policy = %q, want saver", got)
	}
	if st := s.Snapshot(); st.QualityPolicy != QualitySaver {
		t.Fatalf("status policy = %q", st.QualityPolicy)
	}
}

func TestQueueItemQualityOverrideAcrossNavigation(t *testing.T) {
	s := newQueueSessionWithPage(t, 4) // item-1..item-4
	s.SetQualityPolicy(QualityFHD1080)
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error { return nil })

	// Override item-2 to Data Saver.
	if err := s.SetQueueItemQuality("item-2", "saver"); err != nil {
		t.Fatalf("set item quality: %v", err)
	}
	if got := s.QueueItemQuality("item-2"); got != QualitySaver {
		t.Fatalf("item quality = %q", got)
	}

	// Advancing to item-2 applies the override…
	if err := s.Next(); err != nil {
		t.Fatalf("next: %v", err)
	}
	if got := s.QualityPolicy(); got != QualitySaver {
		t.Fatalf("policy while playing overridden item = %q, want saver", got)
	}
	// …and moving past it restores the session default.
	if err := s.Next(); err != nil {
		t.Fatalf("next: %v", err)
	}
	if got := s.QualityPolicy(); got != QualityFHD1080 {
		t.Fatalf("policy after override = %q, want the session default", got)
	}

	// Unknown items are reported, not silently ignored.
	if err := s.SetQueueItemQuality("item-999", "best"); err != ErrQueueItemNotFound {
		t.Fatalf("unknown item = %v, want ErrQueueItemNotFound", err)
	}

	// "auto" clears the override: the item falls back to the session policy.
	if err := s.SetQueueItemQuality("item-2", "auto"); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if got := s.QueueItemQuality("item-2"); got != "" {
		t.Fatalf("override not cleared: %q", got)
	}
}

func TestObservedQualityReportedNotInvented(t *testing.T) {
	o := observedQuality{Width: 1920, Height: 1080}
	if got := o.Describe(); got != "1920x1080" {
		t.Fatalf("Describe = %q", got)
	}
	// YouTube's own label is used when the element reports no size.
	o2 := observedQuality{YtQuality: "hd720"}
	if got := o2.Describe(); got != "YouTube: hd720" {
		t.Fatalf("Describe = %q", got)
	}
	// Nothing reported = nothing claimed.
	if got := (observedQuality{}).Describe(); got != "" {
		t.Fatalf("empty observed described as %q", got)
	}

	// A session without player telemetry reports it as unavailable.
	s := newQueueSession(t, 2)
	if st := s.Snapshot(); st.Observed.Describe() != "" {
		t.Fatalf("unplayed session reported observed = %q", st.Observed.Describe())
	}
}

// TestSelectedFormatOnlyFromDownload pins the requested/selected distinction:
// browser-mode sessions have no extraction ladder and must not present one.
func TestSelectedFormatOnlyFromDownload(t *testing.T) {
	s := newQueueSession(t, 2)
	if err := s.SetQualityPolicy(QualityBest); err != nil {
		t.Fatalf("set: %v", err)
	}
	st := s.Snapshot()
	if st.SelectedFormat != "" {
		t.Fatalf("browser session reported a selected format: %q", st.SelectedFormat)
	}
	if st.QualityPolicy != QualityBest {
		t.Fatalf("requested policy = %q", st.QualityPolicy)
	}
}

// TestQualityPolicyConcurrentAccess covers the same lock discipline the
// queue uses, with quality setters racing the snapshot readers.
func TestQualityPolicyConcurrentAccess(t *testing.T) {
	s := newQueueSession(t, 3)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			policies := []string{QualityAuto, QualitySaver, QualityBest, QualityFHD1080}
			for k := 0; k < 20; k++ {
				_ = s.SetQualityPolicy(policies[(n+k)%len(policies)])
				_ = s.SetQueueItemQuality("item-1", policies[(n+k)%len(policies)])
				_ = s.Snapshot()
			}
		}(i)
	}
	wg.Wait()
	// The final policy must be one of the known set, never a torn value.
	switch NormalizeQualityPolicy(s.QualityPolicy()) {
	case QualityAuto, QualitySaver, QualityBest, QualityFHD1080:
	default:
		t.Fatalf("policy corrupted under concurrency: %q", s.QualityPolicy())
	}
}
