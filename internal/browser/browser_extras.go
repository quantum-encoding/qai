// browser_extras.go — shared CDP helpers for the web-dev convenience
// flags: --wait <css-selector>, --theme <light|dark>, --selector <css>
// (element-scoped capture). Each helper takes the cdpClient and
// returns a Go error so the call sites keep their existing one-liner
// "report + os.Exit" error handling without duplicating CDP plumbing.

package browser

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// waitForSelector polls querySelector until the element appears or
// the timeout elapses. Reused by `wait`, `open --wait`, and
// `emulate --wait`. Returns nil on hit, error on timeout.
//
// 500ms poll interval mirrors browserWait's original behaviour;
// matches Playwright's default polling cadence closely enough that
// agents trained on the Playwright model see no surprise.
func waitForSelector(c *cdpClient, sel string, timeout time.Duration) error {
	if sel == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	expr := fmt.Sprintf("document.querySelector(%q) !== null", sel)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for selector %q", timeout, sel)
		}
		result, err := c.Call("Runtime.evaluate", map[string]any{
			"expression":    expr,
			"returnByValue": true,
		}, 5*time.Second)
		if err == nil {
			var val struct {
				Result struct {
					Value bool `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal(result, &val) == nil && val.Result.Value {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// setTheme forces prefers-color-scheme via Emulation.setEmulatedMedia.
// Accepts "light", "dark", or "" (no-op). Returns error on invalid
// value or CDP failure. Persists for the lifetime of the session
// (until the websocket detaches), so emulate's "do everything on one
// connection" pattern keeps the theme applied through navigation.
//
// CDP shape: Emulation.setEmulatedMedia({features: [{name, value}]}).
// The features list overrides ONE media feature at a time; we set
// prefers-color-scheme only. Passing the empty list clears overrides.
func setTheme(c *cdpClient, theme string) error {
	if theme == "" {
		return nil
	}
	switch theme {
	case "light", "dark":
		// ok
	default:
		return fmt.Errorf("--theme must be 'light' or 'dark' (got %q)", theme)
	}
	_, err := c.Call("Emulation.setEmulatedMedia", map[string]any{
		"features": []map[string]string{
			{"name": "prefers-color-scheme", "value": theme},
		},
	}, 5*time.Second)
	return err
}

// captureScreenshot wraps Page.captureScreenshot with optional
// element-scoped clipping. selector=="" → full-page (current viewport)
// shot at default scale. Non-empty selector → resolves the element's
// box model, captures only the bounding rect.
//
// Returns raw PNG bytes. The clip path uses scale=1 (CSS pixels) which
// is enough fidelity for element close-ups; for HiDPI element shots a
// future flag could surface clip-scale, but every consumer in tree
// today is happy with the CSS-pixel default.
func captureScreenshot(c *cdpClient, selector string) ([]byte, error) {
	params := map[string]any{"format": "png"}

	if selector != "" {
		clip, err := selectorClip(c, selector)
		if err != nil {
			return nil, err
		}
		params["clip"] = clip
		// captureBeyondViewport=true lets clip reach elements that
		// extend below the visible viewport without needing a manual
		// scrollIntoView. selectorClip() already calls scrollIntoView,
		// but this guards element-bottom-out-of-frame cases too.
		params["captureBeyondViewport"] = true
	}

	result, err := c.Call("Page.captureScreenshot", params, 15*time.Second)
	if err != nil {
		return nil, err
	}
	var ss struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(result, &ss); err != nil || ss.Data == "" {
		return nil, fmt.Errorf("no screenshot data")
	}
	return base64.StdEncoding.DecodeString(ss.Data)
}

// selectorClip resolves a CSS selector to a CDP clip rectangle. Walks
// the same box-model path as resolveSelector but returns the bounding
// box (top-left + width/height) instead of the center point.
//
// Includes a scrollIntoView before measuring so an off-screen element
// reports its on-screen box.
func selectorClip(c *cdpClient, sel string) (map[string]any, error) {
	// scrollIntoView first so the element is in the visual viewport.
	_, _ = c.Call("Runtime.evaluate", map[string]any{
		"expression": fmt.Sprintf(`document.querySelector(%q)?.scrollIntoView({block:"center"})`, sel),
	}, 5*time.Second)
	time.Sleep(120 * time.Millisecond)

	docResult, err := c.Call("DOM.getDocument", nil, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("DOM.getDocument: %w", err)
	}
	var doc struct {
		Root struct {
			NodeID int `json:"nodeId"`
		} `json:"root"`
	}
	if err := json.Unmarshal(docResult, &doc); err != nil {
		return nil, fmt.Errorf("parse document: %w", err)
	}

	qResult, err := c.Call("DOM.querySelector", map[string]any{
		"nodeId":   doc.Root.NodeID,
		"selector": sel,
	}, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("querySelector %q: %w", sel, err)
	}
	var qr struct {
		NodeID int `json:"nodeId"`
	}
	if err := json.Unmarshal(qResult, &qr); err != nil || qr.NodeID == 0 {
		return nil, fmt.Errorf("element not found: %s", sel)
	}

	boxResult, err := c.Call("DOM.getBoxModel", map[string]any{
		"nodeId": qr.NodeID,
	}, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("getBoxModel: %w", err)
	}
	var box struct {
		Model struct {
			Content []float64 `json:"content"` // [x1,y1, x2,y2, x3,y3, x4,y4]
			Width   float64   `json:"width"`
			Height  float64   `json:"height"`
		} `json:"model"`
	}
	if err := json.Unmarshal(boxResult, &box); err != nil || len(box.Model.Content) < 8 {
		return nil, fmt.Errorf("parse box model for %s", sel)
	}
	// Top-left of the content quad. Quad order: TL, TR, BR, BL.
	x := box.Model.Content[0]
	y := box.Model.Content[1]
	w := box.Model.Width
	h := box.Model.Height
	if w <= 0 || h <= 0 {
		// Fall back to right - left / bottom - top if width/height were
		// 0 (some pseudo-elements report this).
		w = box.Model.Content[2] - x
		h = box.Model.Content[5] - y
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("element %s has zero-area box (display:none or empty?)", sel)
	}
	return map[string]any{
		"x": x, "y": y, "width": w, "height": h, "scale": 1.0,
	}, nil
}

// parseWaitTimeout reads --timeout <seconds> (default 10s) from args.
// Shared by every command that takes --wait so the timeout semantics
// stay consistent. Invalid values silently fall back to default so a
// typoed timeout doesn't fail the whole capture.
func parseWaitTimeout(args []string) time.Duration {
	v := flagValue(args, "--timeout")
	if v == "" {
		return 10 * time.Second
	}
	// Accept "30" → 30s OR "30s"/"1m" → parsed duration.
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return 10 * time.Second
}
