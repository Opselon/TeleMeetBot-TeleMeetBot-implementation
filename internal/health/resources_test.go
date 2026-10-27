package health

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFormatServerResourcesExactTemplate(t *testing.T) {
	mock := ServerResources{
		CPUUsagePercent:     24,
		CPUScope:            "system-wide",
		CPUAvailable:        true,
		RAMUsedGiB:          2.4,
		RAMTotalGiB:         7.8,
		RAMUsagePercent:     31,
		RAMAvailable:        true,
		SwapUsedGiB:         0.2,
		SwapTotalGiB:        2.0,
		SwapUsagePercent:    10,
		SwapAvailable:       true,
		StorageUsedGiB:      18.6,
		StorageTotalGiB:     80.0,
		StorageUsagePercent: 23,
		StorageAvailable:    true,
	}

	got := FormatServerResourcesFrom(mock)
	wantLines := []string{
		"🖥 Server Resources",
		"⚙️ CPU: 24% (system-wide)",
		"🧠 RAM: 2.4 / 7.8 GiB (31%)",
		"🔄 Swap: 0.2 / 2.0 GiB (10%)",
		"💽 Storage: 18.6 / 80 GiB used (23%)",
	}
	want := strings.Join(wantLines, "\n")
	if got != want {
		t.Fatalf("output mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatServerResourcesContainerScope(t *testing.T) {
	mock := ServerResources{
		CPUUsagePercent:     18,
		CPUScope:            "container",
		CPUAvailable:        true,
		RAMUsedGiB:          1.5,
		RAMTotalGiB:         4.0,
		RAMUsagePercent:     38,
		RAMAvailable:        true,
		SwapUsedGiB:         0.0,
		SwapTotalGiB:        0.0,
		SwapUsagePercent:    0,
		SwapAvailable:       true,
		StorageUsedGiB:      5.2,
		StorageTotalGiB:     50.0,
		StorageUsagePercent: 10,
		StorageAvailable:    true,
	}

	got := FormatServerResourcesFrom(mock)
	if !strings.Contains(got, "⚙️ CPU: 18% (container)") {
		t.Errorf("expected container scope in CPU output, got:\n%s", got)
	}
	if !strings.Contains(got, "🔄 Swap: 0.0 / 0.0 GiB (0%)") {
		t.Errorf("expected zero swap line, got:\n%s", got)
	}
}

func TestFormatServerResourcesUnavailableGraceful(t *testing.T) {
	mock := ServerResources{
		CPUAvailable:     false,
		RAMAvailable:     false,
		SwapAvailable:    false,
		StorageAvailable: false,
	}

	got := FormatServerResourcesFrom(mock)
	wantLines := []string{
		"🖥 Server Resources",
		"⚙️ CPU: N/A",
		"🧠 RAM: N/A",
		"🔄 Swap: N/A",
		"💽 Storage: N/A",
	}
	want := strings.Join(wantLines, "\n")
	if got != want {
		t.Fatalf("output mismatch for unavailable resources:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatStorageTotal(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{80.0, "80"},
		{80.04, "80"},
		{80.5, "80.5"},
		{18.6, "18.6"},
		{0.0, "0"},
		{-1.0, "0"},
	}
	for _, tt := range tests {
		if got := formatStorageTotal(tt.in); got != tt.want {
			t.Errorf("formatStorageTotal(%f) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGetServerResourcesReal(t *testing.T) {
	SetResourceDataDir(t.TempDir())
	res := GetServerResources()

	// Storage should always succeed for a valid temp dir
	if !res.StorageAvailable {
		t.Error("expected StorageAvailable to be true for temp dir")
	}
	if res.StorageTotalGiB <= 0 {
		t.Errorf("expected positive StorageTotalGiB, got %f", res.StorageTotalGiB)
	}

	// FormatServerResources output must start with the header
	str := FormatServerResources()
	if !strings.HasPrefix(str, "🖥 Server Resources\n") {
		t.Fatalf("unexpected formatted output:\n%s", str)
	}
}

func TestResourceCollectorCaching(t *testing.T) {
	c := &resourceCollector{
		dataDir: t.TempDir(),
	}
	c.sampleLocked(true)
	firstSample := c.cached
	firstTime := c.lastSampled

	// An immediate second sample without force should be cached
	time.Sleep(10 * time.Millisecond)
	res := c.cached
	if res.CollectedAt != firstSample.CollectedAt {
		t.Fatalf("cached sample time changed unexpectedly: %v != %v", res.CollectedAt, firstSample.CollectedAt)
	}
	if c.lastSampled != firstTime {
		t.Fatal("lastSampled changed without new collection")
	}
}

func TestResourceCollectorConcurrentAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartResourceMonitor(ctx, t.TempDir())

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				res := GetServerResources()
				_ = FormatServerResourcesFrom(res)
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
}
