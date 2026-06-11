// image_params.go — translates user-neutral flags (`--aspect`, `--size`,
// `--count`, `--quality`, `--background`, `--format`) into the
// model-specific request shape the broker forwards to each provider.
//
// Why this exists: each image model takes a different parameter
// vocabulary. Gemini 3 Pro uses `aspect_ratio` + `resolution` (1K/2K/4K),
// Gemini Flash variants use `aspect_ratio` only (no resolution param),
// OpenAI uses a single `size` enum that bakes aspect into pixel dims
// (1024x1024 / 1024x1536 / 1536x1024 / auto), plus `quality` +
// `background` + `output_format`. The CLI surface used to send one
// generic `size` field and let the broker figure it out — it didn't,
// and Gemini 3 Pro silently ignored size, defaulting to 2K.
//
// This file replaces that with model-aware routing. Unsupported flags
// for the target model are dropped with a one-line stderr warning so
// batch invocations that swap models keep working without surprising
// errors, but the user always knows what was discarded.

package conduct

import (
	"fmt"
	"os"
	"strings"
)

// ─── families ──────────────────────────────────────────────────────────────

type imageFamily int

const (
	famUnknown imageFamily = iota
	famGeminiPro            // gemini-3-pro-image-preview — aspect + resolution(1K/2K/4K)
	famGeminiFlash          // 2.5-flash-image, 3.1-flash-image-preview — aspect only
	famOpenAI               // gpt-image-1, gpt-image-2 — size enum + quality + bg + format
	famGrokImage            // grok-imagine-image, grok-imagine-image-quality — n only (today)
)

// classifyImageModel maps a CANONICAL model id (after resolveImageModel)
// to its capability family. Unknown ids fall through to famUnknown,
// which makes the body builder pass user flags verbatim — safer than
// guessing for new models the broker may add before we update this map.
func classifyImageModel(model string) imageFamily {
	switch strings.ToLower(model) {
	case "gemini-3-pro-image-preview":
		return famGeminiPro
	case "gemini-3.1-flash-image-preview", "gemini-2.5-flash-image":
		return famGeminiFlash
	case "gpt-image-1", "gpt-image-1-mini", "gpt-image-1.5", "gpt-image-2", "chatgpt-image-latest":
		return famOpenAI
	case "grok-imagine-image", "grok-imagine-image-quality", "grok-imagine-image-quality-latest":
		return famGrokImage
	}
	return famUnknown
}

// ─── flag bundle ───────────────────────────────────────────────────────────

// imageFlags is the parsed, provider-neutral form of every image CLI
// knob. Each is "unset" sentinel-empty (or 0 for ints) so the body
// builder can distinguish "user didn't pass this" from "user explicitly
// asked for the default" — only set fields make it into the request.
type imageFlags struct {
	model      string // canonical id (already resolved by resolveImageModel)
	prompt     string
	count      int    // --count N, 0 = unset
	aspect     string // --aspect 16:9
	size       string // --size — overloaded: tier (1K/2K/4K) OR pixels (1024x1024)
	quality    string // --quality low|medium|high|auto
	background string // --background transparent|opaque|auto
	format     string // --format png|jpeg|webp
}

// ─── body builder ──────────────────────────────────────────────────────────

// buildImageBody applies family-specific translation rules and returns
// the request body for the broker. Unsupported flags trigger one-line
// stderr warnings ("dropping --X for model Y"); see warnDrop. Always
// includes `model` + `prompt`; everything else only appears when the
// user passed it AND the target family supports it.
func buildImageBody(f imageFlags) map[string]any {
	body := map[string]any{"model": f.model}
	if f.prompt != "" {
		body["prompt"] = f.prompt
	}

	switch classifyImageModel(f.model) {
	case famGeminiPro:
		applyGeminiPro(&f, body)
	case famGeminiFlash:
		applyGeminiFlash(&f, body)
	case famOpenAI:
		applyOpenAI(&f, body)
	case famGrokImage:
		applyGrokImage(&f, body)
	default:
		// Unknown model — pass user flags through under their CLI names
		// so a new broker model can be exercised without a qai release.
		// The broker either consumes them or errors clearly.
		applyPassthrough(&f, body)
	}
	return body
}

// ─── per-family appliers ───────────────────────────────────────────────────

func applyGeminiPro(f *imageFlags, body map[string]any) {
	if f.count > 0 {
		body["number_of_images"] = f.count
	}
	if f.aspect != "" {
		body["aspect_ratio"] = f.aspect
	}
	if f.size != "" {
		if tier, ok := asResolutionTier(f.size); ok {
			body["resolution"] = tier
		} else {
			// Pixel-shaped --size on Gemini Pro: snap to the nearest
			// supported tier so the request still goes through, and
			// tell the user what we did.
			tier := pixelsToGeminiTier(f.size)
			body["resolution"] = tier
			fmt.Fprintf(os.Stderr,
				"qai image: --size %s isn't a Gemini tier; using resolution=%s "+
					"(Nano Banana Pro accepts 1K, 2K, or 4K)\n", f.size, tier)
		}
	}
	warnDrop(f.quality, "--quality", f.model)
	warnDrop(f.background, "--background", f.model)
	warnDrop(f.format, "--format", f.model)
}

func applyGeminiFlash(f *imageFlags, body map[string]any) {
	if f.count > 0 {
		body["number_of_images"] = f.count
	}
	if f.aspect != "" {
		body["aspect_ratio"] = f.aspect
	}
	warnDrop(f.size, "--size", f.model+" (no resolution param — use Nano Banana Pro for tier control)")
	warnDrop(f.quality, "--quality", f.model)
	warnDrop(f.background, "--background", f.model)
	warnDrop(f.format, "--format", f.model)
}

func applyOpenAI(f *imageFlags, body map[string]any) {
	if f.count > 0 {
		body["n"] = f.count
	}
	// OpenAI bakes aspect into the size enum; if user gave --aspect but
	// not --size, translate. If they gave --size, --size wins (explicit
	// beats inferred). If neither, broker defaults to "auto".
	size := f.size
	if size == "" && f.aspect != "" {
		size = aspectToOpenAISize(f.aspect)
	}
	if size != "" {
		body["size"] = normalizeOpenAISize(size, f.model)
	}
	if f.aspect != "" && f.size != "" {
		// User passed both — note that size is the binding one.
		fmt.Fprintf(os.Stderr,
			"qai image: both --aspect %s and --size %s given for %s; --size wins (OpenAI bakes aspect into size)\n",
			f.aspect, f.size, f.model)
	}
	if f.quality != "" {
		body["quality"] = f.quality
	}
	if f.background != "" {
		body["background"] = f.background
	}
	if f.format != "" {
		body["output_format"] = f.format
	}
}

func applyGrokImage(f *imageFlags, body map[string]any) {
	if f.count > 0 {
		body["n"] = f.count
	}
	// Grok Imagine doesn't advertise aspect/size/quality in the broker
	// schema today. Warn-and-drop so swapping in a Grok model in a batch
	// doesn't silently error.
	warnDrop(f.aspect, "--aspect", f.model)
	warnDrop(f.size, "--size", f.model)
	warnDrop(f.quality, "--quality", f.model)
	warnDrop(f.background, "--background", f.model)
	warnDrop(f.format, "--format", f.model)
}

func applyPassthrough(f *imageFlags, body map[string]any) {
	if f.count > 0 {
		body["count"] = f.count
	}
	if f.aspect != "" {
		body["aspect_ratio"] = f.aspect
	}
	if f.size != "" {
		body["size"] = f.size
	}
	if f.quality != "" {
		body["quality"] = f.quality
	}
	if f.background != "" {
		body["background"] = f.background
	}
	if f.format != "" {
		body["output_format"] = f.format
	}
}

// ─── translation helpers ───────────────────────────────────────────────────

// asResolutionTier returns ("1K"|"2K"|"4K", true) if the input is a tier
// string. Case-insensitive. Anything else returns false so the caller
// can decide whether to snap or pass through. Nano Banana Pro
// (gemini-3-pro-image-preview) supports all three tiers.
func asResolutionTier(s string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "1K":
		return "1K", true
	case "2K":
		return "2K", true
	case "4K":
		return "4K", true
	}
	return "", false
}

// pixelsToGeminiTier picks the closest supported Gemini tier (1K/2K/4K)
// for a WxH input. Breakpoints sit at the geometric means between
// adjacent tiers (≈1448 between 1K/2K, ≈2896 between 2K/4K), so a
// "1024x..." input → 1K, "2048x..." → 2K, "3840x..." → 4K. Anything
// unparsable defaults to "2K" — the sensible "give me the good one"
// fallback (not 4K, which is markedly more expensive).
func pixelsToGeminiTier(size string) string {
	w, _ := parseWxH(size)
	if w <= 0 {
		return "2K"
	}
	switch {
	case w <= 1448:
		return "1K"
	case w <= 2896:
		return "2K"
	default:
		return "4K"
	}
}

func parseWxH(s string) (w, h int) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(s)), "x", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	for i, p := range parts {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n <= 0 {
			return 0, 0
		}
		if i == 0 {
			w = n
		} else {
			h = n
		}
	}
	return
}

// aspectToOpenAISize maps a ratio expression to OpenAI's three-slot size
// enum: square (1024x1024), landscape (1536x1024), portrait (1024x1536).
// Any ratio is classified by width/height; ties go to square.
func aspectToOpenAISize(aspect string) string {
	a, b, ok := parseAspectRatio(aspect)
	if !ok || a <= 0 || b <= 0 {
		return "auto"
	}
	switch {
	case a == b:
		return "1024x1024"
	case a > b:
		return "1536x1024"
	default:
		return "1024x1536"
	}
}

func parseAspectRatio(s string) (a, b float64, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[0], "%f", &a); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%f", &b); err != nil {
		return 0, 0, false
	}
	return a, b, true
}

// normalizeOpenAISize maps "1024x1024" / "1024x1536" / "1536x1024" /
// "auto" pass-through, and snaps any other WxH to the nearest of those
// three. Off-spec inputs trigger a stderr note so the user knows we
// rounded.
func normalizeOpenAISize(size, model string) string {
	canonical := strings.ToLower(strings.TrimSpace(size))
	switch canonical {
	case "1024x1024", "1024x1536", "1536x1024", "auto":
		return canonical
	}
	w, h := parseWxH(canonical)
	if w <= 0 || h <= 0 {
		fmt.Fprintf(os.Stderr,
			"qai image: --size %q not understood for %s; falling back to auto "+
				"(OpenAI accepts 1024x1024 / 1024x1536 / 1536x1024 / auto)\n",
			size, model)
		return "auto"
	}
	var snapped string
	switch {
	case w == h:
		snapped = "1024x1024"
	case w > h:
		snapped = "1536x1024"
	default:
		snapped = "1024x1536"
	}
	fmt.Fprintf(os.Stderr,
		"qai image: --size %s snapped to %s for %s (OpenAI sizes are fixed)\n",
		size, snapped, model)
	return snapped
}

// warnDrop emits one stderr line when a user-set flag isn't supported
// by the target model. value is the user-supplied flag value — empty
// = user didn't pass it = no warning. This is the warn-and-drop branch
// of the misfit-flag policy; the alternative (hard error) was
// considered and rejected because it would break batches that swap
// models partway through.
func warnDrop(value, flagName, model string) {
	if value == "" {
		return
	}
	fmt.Fprintf(os.Stderr,
		"qai image: dropping %s=%s — not supported by %s\n",
		flagName, value, model)
}
