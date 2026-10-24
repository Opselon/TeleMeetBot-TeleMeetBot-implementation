package media

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ResolveDir must anchor a relative/empty dir on the base dir, keep an
// absolute dir absolute, and always create the directory 0700.
func TestResolveDir(t *testing.T) {
	base := t.TempDir()

	// Empty dir → <base>/downloads.
	o := DefaultOptions("")
	if err := o.ResolveDir(base); err != nil {
		t.Fatalf("ResolveDir(empty): %v", err)
	}
	want := filepath.Join(base, "downloads")
	if o.Dir != want {
		t.Fatalf("empty dir resolved to %q, want %q", o.Dir, want)
	}
	if st, err := os.Stat(o.Dir); err != nil || !st.IsDir() {
		t.Fatalf("download dir not created: %v", err)
	}

	// Relative dir → <base>/<dir>.
	o = DefaultOptions("media-cache")
	if err := o.ResolveDir(base); err != nil {
		t.Fatalf("ResolveDir(relative): %v", err)
	}
	if got, want := o.Dir, filepath.Join(base, "media-cache"); got != want {
		t.Fatalf("relative dir resolved to %q, want %q", got, want)
	}

	// Absolute dir is used verbatim.
	abs := filepath.Join(t.TempDir(), "elsewhere")
	o = DefaultOptions(abs)
	if err := o.ResolveDir(base); err != nil {
		t.Fatalf("ResolveDir(absolute): %v", err)
	}
	if o.Dir != abs {
		t.Fatalf("absolute dir rewritten to %q, want %q", o.Dir, abs)
	}

	// No base and an empty dir is a configuration error, not a guess.
	o = DefaultOptions("")
	if err := o.ResolveDir(""); err == nil {
		t.Fatal("ResolveDir with no dir and no base should fail")
	}
}

// Validate rejects a TTL outside the allowed envelope.
func TestOptionsValidate(t *testing.T) {
	o := DefaultOptions(t.TempDir())
	if err := o.Validate(); err != nil {
		t.Fatalf("default options should validate: %v", err)
	}
	o.TTL = 0
	if err := o.Validate(); err == nil {
		t.Fatal("a zero TTL should fail validation")
	}
	o.TTL = 25 * time.Hour
	if err := o.Validate(); err == nil {
		t.Fatal("a >24h TTL should fail validation")
	}
	o.TTL = time.Hour
	o.MaxBytes = -1
	if err := o.Validate(); err == nil {
		t.Fatal("a negative size limit should fail validation")
	}
}
