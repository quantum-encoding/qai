package media

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveModel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "gemini-3.1-flash-lite"},
		{"gemini", "gemini-3.1-flash-lite"},
		{"flash-lite", "gemini-3.1-flash-lite"},
		{"FLASH", "gemini-3.1-flash-lite"},
		{"gemini-pro", "gemini-3.1-pro-preview"},
		// unknown alias passes through verbatim so the broker can
		// reject with a useful "model not available"
		{"some-future-model", "some-future-model"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := resolveModel(tc.in); got != tc.want {
				t.Errorf("resolveModel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripModelFlag(t *testing.T) {
	cases := []struct {
		in        []string
		wantOut   []string
		wantModel string
	}{
		{[]string{"a", "b"}, []string{"a", "b"}, ""},
		{[]string{"--model", "gemini", "a"}, []string{"a"}, "gemini"},
		{[]string{"a", "--model", "gpt", "b"}, []string{"a", "b"}, "gpt"},
		{[]string{"a", "b", "-m", "claude"}, []string{"a", "b"}, "claude"},
		// trailing flag without value — strip the flag, leave model empty
		{[]string{"a", "--model"}, []string{"a"}, ""},
	}
	for i, tc := range cases {
		out, model := stripModelFlag(tc.in)
		if !reflect.DeepEqual(out, tc.wantOut) || model != tc.wantModel {
			t.Errorf("case %d: stripModelFlag(%v) = (%v, %q), want (%v, %q)",
				i, tc.in, out, model, tc.wantOut, tc.wantModel)
		}
	}
}

func TestStripBoolFlag(t *testing.T) {
	out, present := stripBoolFlag([]string{"--no-compress", "a"}, "--no-compress")
	if !present {
		t.Errorf("expected present=true")
	}
	if !reflect.DeepEqual(out, []string{"a"}) {
		t.Errorf("expected [a], got %v", out)
	}

	out, present = stripBoolFlag([]string{"a", "b"}, "--no-compress")
	if present {
		t.Errorf("expected present=false on absent flag")
	}
	if !reflect.DeepEqual(out, []string{"a", "b"}) {
		t.Errorf("expected unchanged, got %v", out)
	}
}

func TestJoinPrompt(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"hello"}, "hello"},
		{[]string{"explain", "part", "1-3"}, "explain part 1-3"},
		{[]string{"  trimmed  "}, "trimmed"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := joinPrompt(tc.in); got != tc.want {
			t.Errorf("joinPrompt(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGuessMime(t *testing.T) {
	cases := []struct {
		path, want string
	}{
		{"video.mp4", "video/mp4"},
		{"clip.MOV", "video/quicktime"},
		{"speech.mp3", "audio/mpeg"},
		{"doc.pdf", "application/pdf"},
		{"weird.xyz", ""},
		{"noext", ""},
	}
	for _, tc := range cases {
		if got := guessMime(tc.path); got != tc.want {
			t.Errorf("guessMime(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestShouldCompress_BelowThreshold(t *testing.T) {
	// Tiny temp file — should not trigger compression regardless of MIME.
	tmp := filepath.Join(t.TempDir(), "small.mp4")
	if err := os.WriteFile(tmp, []byte("not really mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	if shouldCompress(tmp, "video/mp4") {
		t.Errorf("expected false for sub-threshold file")
	}
}

func TestShouldCompress_AudioNeverCompressed(t *testing.T) {
	// Create a fake "30MB" file via Truncate — over threshold but
	// audio MIME, so we should skip compression.
	tmp := filepath.Join(t.TempDir(), "podcast.mp3")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(30 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if shouldCompress(tmp, "audio/mpeg") {
		t.Errorf("expected false for audio (we don't re-encode audio)")
	}
}

func TestShouldCompress_VideoOverThreshold(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "big.mp4")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(30 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if !shouldCompress(tmp, "video/mp4") {
		t.Errorf("expected true for video over threshold")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{50, "50B"},
		{2048, "2K"},
		{int64(5 << 20), "5M"},
		{int64(3) << 30, "3.0G"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abcdefgh1234"); got != "abcdefgh" {
		t.Errorf("got %q, want abcdefgh", got)
	}
	if got := shortID("short"); got != "short" {
		t.Errorf("got %q, want short", got)
	}
}

func TestActiveSessionRoundTrip(t *testing.T) {
	// Use a temp HOME so we don't trample on the user's real
	// ~/.qai/media-sessions/active during tests.
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := getActiveSession(); got != "" {
		t.Errorf("clean home should give empty active, got %q", got)
	}
	if err := setActiveSession("abc-123"); err != nil {
		t.Fatal(err)
	}
	if got := getActiveSession(); got != "abc-123" {
		t.Errorf("got %q, want abc-123", got)
	}
	clearActiveSession()
	if got := getActiveSession(); got != "" {
		t.Errorf("cleared active should give empty, got %q", got)
	}
}
