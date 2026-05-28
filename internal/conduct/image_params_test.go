package conduct

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// captureStderr returns whatever the body builder wrote to os.Stderr
// during fn. Used to assert warn-and-drop messages without coupling
// tests to the exact wording.
func captureStderr(fn func()) string {
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// keys returns the body's keys sorted — gives stable assertion strings
// without depending on Go's randomized map iteration.
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestGeminiPro_AspectAndTier(t *testing.T) {
	body := buildImageBody(imageFlags{
		model:  "gemini-3-pro-image-preview",
		prompt: "x",
		count:  3,
		aspect: "16:9",
		size:   "2K",
	})
	expect := map[string]any{
		"model":            "gemini-3-pro-image-preview",
		"prompt":           "x",
		"number_of_images": 3,
		"aspect_ratio":     "16:9",
		"resolution":       "2K",
	}
	if !reflect.DeepEqual(body, expect) {
		t.Fatalf("got %v, want %v", body, expect)
	}
}

func TestGeminiPro_PixelSizeSnapsToTier(t *testing.T) {
	var body map[string]any
	stderr := captureStderr(func() {
		body = buildImageBody(imageFlags{
			model: "gemini-3-pro-image-preview",
			size:  "1024x1024", // 1024 ≤ 1414 → 1K
		})
	})
	if body["resolution"] != "1K" {
		t.Errorf("1024x1024 should snap to 1K, got %v", body["resolution"])
	}
	if !strings.Contains(stderr, "snapped") && !strings.Contains(stderr, "isn't a Gemini tier") {
		t.Errorf("expected snap-to-tier warning, got: %q", stderr)
	}

	body = buildImageBody(imageFlags{
		model: "gemini-3-pro-image-preview",
		size:  "2048x2048", // > 1414 → 2K
	})
	if body["resolution"] != "2K" {
		t.Errorf("2048x2048 should snap to 2K, got %v", body["resolution"])
	}
}

func TestGeminiFlash_DropsResolution(t *testing.T) {
	var body map[string]any
	stderr := captureStderr(func() {
		body = buildImageBody(imageFlags{
			model:  "gemini-3.1-flash-image-preview",
			prompt: "x",
			count:  2,
			aspect: "9:16",
			size:   "2K", // should be dropped
		})
	})
	if _, has := body["resolution"]; has {
		t.Errorf("flash has no resolution param; body should not include it: %v", body)
	}
	if _, has := body["size"]; has {
		t.Errorf("flash has no size param; body should not include it: %v", body)
	}
	if body["aspect_ratio"] != "9:16" {
		t.Errorf("aspect_ratio should pass through: %v", body)
	}
	if body["number_of_images"] != 2 {
		t.Errorf("count should map to number_of_images: %v", body)
	}
	if !strings.Contains(stderr, "--size") || !strings.Contains(stderr, "dropping") {
		t.Errorf("expected --size dropped warning, got: %q", stderr)
	}
}

func TestOpenAI_AspectMapsToSize(t *testing.T) {
	cases := []struct {
		aspect string
		want   string
	}{
		{"1:1", "1024x1024"},
		{"16:9", "1536x1024"},
		{"3:2", "1536x1024"},
		{"9:16", "1024x1536"},
		{"2:3", "1024x1536"},
		{"3:4", "1024x1536"},
	}
	for _, c := range cases {
		body := buildImageBody(imageFlags{model: "gpt-image-1", aspect: c.aspect})
		if body["size"] != c.want {
			t.Errorf("aspect %s → got size=%v, want %s", c.aspect, body["size"], c.want)
		}
		if _, has := body["aspect_ratio"]; has {
			t.Errorf("OpenAI body should NOT include aspect_ratio (gets baked into size): %v", body)
		}
	}
}

func TestOpenAI_ExplicitSizeWins(t *testing.T) {
	stderr := captureStderr(func() {
		body := buildImageBody(imageFlags{
			model:  "gpt-image-1",
			aspect: "16:9",         // landscape → would imply 1536x1024
			size:   "1024x1024",    // but user explicitly said square
		})
		if body["size"] != "1024x1024" {
			t.Errorf("--size should win when both passed; got %v", body["size"])
		}
	})
	if !strings.Contains(stderr, "--size wins") {
		t.Errorf("expected note about size winning over aspect, got: %q", stderr)
	}
}

func TestOpenAI_OffSpecSizeSnaps(t *testing.T) {
	stderr := captureStderr(func() {
		body := buildImageBody(imageFlags{
			model: "gpt-image-1",
			size:  "1920x1080", // not a valid OpenAI size — snap to landscape
		})
		if body["size"] != "1536x1024" {
			t.Errorf("1920x1080 should snap to 1536x1024 (landscape), got %v", body["size"])
		}
	})
	if !strings.Contains(stderr, "snapped") {
		t.Errorf("expected snap warning, got: %q", stderr)
	}
}

func TestOpenAI_FullFlagSet(t *testing.T) {
	body := buildImageBody(imageFlags{
		model:      "gpt-image-1",
		prompt:     "x",
		count:      2,
		size:       "1024x1024",
		quality:    "high",
		background: "transparent",
		format:     "webp",
	})
	expect := map[string]any{
		"model":         "gpt-image-1",
		"prompt":        "x",
		"n":             2,
		"size":          "1024x1024",
		"quality":       "high",
		"background":    "transparent",
		"output_format": "webp",
	}
	if !reflect.DeepEqual(body, expect) {
		t.Fatalf("got %v\nwant %v", body, expect)
	}
}

func TestGeminiPro_DropsOpenAIOnlyFlags(t *testing.T) {
	var dropped []string
	stderr := captureStderr(func() {
		buildImageBody(imageFlags{
			model:      "gemini-3-pro-image-preview",
			quality:    "high",
			background: "transparent",
			format:     "webp",
		})
	})
	for _, flag := range []string{"--quality", "--background", "--format"} {
		if !strings.Contains(stderr, flag) {
			dropped = append(dropped, flag)
		}
	}
	if len(dropped) > 0 {
		t.Errorf("missing warnings for %v in stderr: %q", dropped, stderr)
	}
}

func TestUnknownModel_Passthrough(t *testing.T) {
	body := buildImageBody(imageFlags{
		model:  "some-future-model-id",
		prompt: "x",
		count:  1,
		aspect: "16:9",
		size:   "1024x1024",
	})
	// Unknown family uses CLI names verbatim so a new broker model can
	// be exercised before the family table is updated.
	if body["count"] != 1 || body["aspect_ratio"] != "16:9" || body["size"] != "1024x1024" {
		t.Errorf("passthrough should keep CLI-shape names: %v", body)
	}
}

func TestParseAspectRatio(t *testing.T) {
	cases := []struct {
		in       string
		a, b     float64
		wantOK   bool
	}{
		{"16:9", 16, 9, true},
		{"1:1", 1, 1, true},
		{"3:2", 3, 2, true},
		{"bad", 0, 0, false},
		{"3:", 0, 0, false},
	}
	for _, c := range cases {
		a, b, ok := parseAspectRatio(c.in)
		if ok != c.wantOK || a != c.a || b != c.b {
			t.Errorf("parseAspectRatio(%q) = (%v,%v,%v), want (%v,%v,%v)",
				c.in, a, b, ok, c.a, c.b, c.wantOK)
		}
	}
}

// Ensure no surprising body keys when ALL flags are unset — defaults
// shouldn't leak as zero-valued fields.
func TestMinimalBody_NoLeakedDefaults(t *testing.T) {
	body := buildImageBody(imageFlags{
		model:  "gemini-3-pro-image-preview",
		prompt: "x",
	})
	want := []string{"model", "prompt"}
	got := keys(body)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body should only contain %v with no flags set; got %v (full: %v)",
			want, got, body)
	}
}

// Smoke-test the example from the user's curl: Gemini 3 Pro at 1K with
// 16:9 aspect ratio.
func TestUserScenario_GeminiPro1K_16x9(t *testing.T) {
	body := buildImageBody(imageFlags{
		model:  "gemini-3-pro-image-preview",
		prompt: "test",
		aspect: "16:9",
		size:   "1K",
	})
	if body["aspect_ratio"] != "16:9" {
		t.Errorf("aspect_ratio missing/wrong: %v", body)
	}
	if body["resolution"] != "1K" {
		t.Errorf("resolution missing/wrong (this is the bug the user hit): %v", body)
	}
	fmt.Printf("user scenario body = %v\n", body)
}
