package web

import (
	"testing"
	"time"
)

// Several model fields are optional *time.Time. Templates dereference
// pointers automatically, so a nil one must never reach the helper as a bare
// time.Time or the page panics.
func TestTimeHelpersAcceptNilPointers(t *testing.T) {
	var missing *time.Time

	if got := formatDate(missing); got != "never" {
		t.Errorf("formatDate(nil) = %q, want %q", got, "never")
	}
	if got := humanizeAgo(missing); got != "never" {
		t.Errorf("humanizeAgo(nil) = %q, want %q", got, "never")
	}
	if got := formatDate(time.Time{}); got != "never" {
		t.Errorf("formatDate(zero) = %q, want %q", got, "never")
	}

	when := time.Date(2026, 3, 4, 15, 30, 0, 0, time.UTC)
	if got := formatDate(&when); got == "never" {
		t.Error("formatDate(*time.Time) must render the timestamp")
	}
	if got := formatDate(when); got == "never" {
		t.Error("formatDate(time.Time) must render the timestamp")
	}
	if got := humanizeAgo(&when); got == "never" {
		t.Error("humanizeAgo(*time.Time) must render a relative time")
	}
	// Anything unexpected degrades to "never" rather than panicking.
	if got := formatDate("not a time"); got != "never" {
		t.Errorf("formatDate(string) = %q, want %q", got, "never")
	}
}

func TestHumanizeAgoBuckets(t *testing.T) {
	now := time.Now()
	cases := []struct {
		when time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-2 * 24 * time.Hour), "2d ago"},
	}
	for _, c := range cases {
		if got := humanizeAgo(c.when); got != c.want {
			t.Errorf("humanizeAgo(%v) = %q, want %q", c.when, got, c.want)
		}
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 1048576: "1.0 MiB"}
	for in, want := range cases {
		if got := humanizeBytes(in); got != want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanizeNumber(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := humanizeNumber(in); got != want {
			t.Errorf("humanizeNumber(%d) = %q, want %q", in, got, want)
		}
	}
}

// The login flow must not be usable as an open redirector.
func TestSafeNext(t *testing.T) {
	for _, in := range []string{"//evil.example.com", "https://evil.example.com", "", "javascript:alert(1)"} {
		if got := safeNext(in); got != "/" {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, "/")
		}
	}
	if got := safeNext("/backends/3"); got != "/backends/3" {
		t.Errorf("safeNext kept a same-site path wrong: %q", got)
	}
}
