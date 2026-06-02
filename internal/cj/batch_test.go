package cj

import (
	"testing"
	"time"
)

// TestLooksLikeThrottle pins the substring set the retry loop trips
// on. If `qai browser open` ever changes its timeout stderr format,
// this test fails loudly so retries don't silently degrade to
// fail-fast against a real throttle.
func TestLooksLikeThrottle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "real_open_timeout_stderr",
			in:   `qai browser open: timeout after 30s waiting for selector "a[href*=\"/product/\"]"` + "\n",
			want: true,
		},
		{
			name: "different_timeout_format_still_matches",
			in:   `something timeout after 1m waiting for selector "x"`,
			want: true,
		},
		{
			name: "security_gate_denial",
			in:   "qai browser: denied open on https://example.com (scheme_denied)",
			want: false,
		},
		{
			name: "browser_unreachable",
			in:   "cannot connect to browser on localhost:9222 (no debug port listening)",
			want: false,
		},
		{
			name: "empty",
			in:   "",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := looksLikeThrottle(c.in)
			if got != c.want {
				t.Errorf("looksLikeThrottle(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestBackoffBounds — backoff(N) must grow with N (with jitter
// absorbing small overlap), stay above a sane floor, and respect the
// 5-minute cap. Without bounds the loop could fire instantly on
// attempt 0 (no jitter floor) or block for hours on attempt 8+.
func TestBackoffBounds(t *testing.T) {
	const base = 1 * time.Second

	// attempt 0 → roughly [base/2, base*2)
	d0 := backoff(0, base)
	if d0 < base/2 || d0 > base*2 {
		t.Errorf("backoff(0, %v) = %v, want in [%v, %v)", base, d0, base/2, base*2)
	}

	// attempt 3 → roughly base*8 ± base/4 ≈ [7.75s, 8.25s], floor base/2
	d3 := backoff(3, base)
	if d3 < base/2 || d3 > 10*base {
		t.Errorf("backoff(3, %v) = %v, want in [%v, %v]", base, d3, base/2, 10*base)
	}

	// attempt 100 → capped at 5 minutes regardless of base
	d100 := backoff(100, base)
	if d100 > 5*time.Minute {
		t.Errorf("backoff(100, %v) = %v, exceeds 5min cap", base, d100)
	}
	if d100 < base/2 {
		t.Errorf("backoff(100, %v) = %v, below base/2 floor", base, d100)
	}

	// zero base falls back to the documented 30s default
	dZero := backoff(0, 0)
	if dZero < 15*time.Second || dZero > 60*time.Second {
		t.Errorf("backoff(0, 0) = %v, want around 30s default", dZero)
	}

	// negative attempt is clamped to 0
	dNeg := backoff(-5, base)
	if dNeg < base/2 || dNeg > base*2 {
		t.Errorf("backoff(-5, %v) = %v, want clamped to attempt-0 range", base, dNeg)
	}
}

// TestBackoffJitterNonZero — eyeballing that adjacent calls don't
// return identical values. Without jitter, every retry storm would
// synchronise on the same wall-clock — bad for any shared-target
// throttle that resets on quiet periods.
func TestBackoffJitterNonZero(t *testing.T) {
	const base = 100 * time.Millisecond
	values := map[time.Duration]int{}
	for i := 0; i < 20; i++ {
		values[backoff(2, base)]++
	}
	if len(values) < 2 {
		t.Errorf("20 backoff(2) calls produced %d unique values — jitter is broken", len(values))
	}
}
