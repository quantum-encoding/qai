// browser_emulate.go — Mobile device emulation over CDP.
//
// Drives the Emulation.* domain — the same machinery behind DevTools'
// device toolbar — to override viewport metrics, device-pixel-ratio,
// touch, and user-agent so a page renders as it would on a phone/tablet.
// Used for debugging Android-vs-iOS layout and UA-sniffing differences.
//
// IMPORTANT — this is Blink rendering a mobile *viewport*, not WebKit.
// It reproduces responsive layout, viewport sizing, touch events, and
// any UA/platform-sniffing branch the site takes. It does NOT reproduce
// genuine iOS Safari / WebKit rendering quirks (-webkit-only CSS, 100vh
// address-bar behaviour, scroll momentum). For true iOS rendering use a
// real iOS Simulator + Safari. For everything UA- and layout-driven,
// this matches Chrome DevTools device mode exactly.
//
// Usage:
//
//	qai browser emulate list                       # show device presets
//	qai browser emulate <device> [url] [-o f.png]  # emulate + screenshot
//	qai browser emulate iphone15 https://site.com -o ios.png
//	qai browser emulate pixel7   https://site.com -o android.png

package browser

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// devicePreset describes a viewport + fingerprint to emulate.
type devicePreset struct {
	label    string  // human display name
	width    int     // CSS pixels
	height   int     // CSS pixels
	dpr      float64 // device-pixel-ratio
	ua       string  // navigator.userAgent override
	platform string  // navigator.platform override
	os       string  // "ios" | "android" — drives client-hint metadata
}

// devicePresets maps lowercase aliases → preset. Multiple aliases may
// point at the same device.
var devicePresets = map[string]devicePreset{
	"iphone-se": {
		label: "iPhone SE (2nd/3rd gen)", width: 375, height: 667, dpr: 2,
		ua:       "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
		platform: "iPhone", os: "ios",
	},
	"iphone15": {
		label: "iPhone 15 / 15 Pro", width: 393, height: 852, dpr: 3,
		ua:       "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
		platform: "iPhone", os: "ios",
	},
	"iphone15-max": {
		label: "iPhone 15 Pro Max", width: 430, height: 932, dpr: 3,
		ua:       "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
		platform: "iPhone", os: "ios",
	},
	"ipad": {
		label: "iPad (10.2\")", width: 810, height: 1080, dpr: 2,
		ua:       "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
		platform: "iPad", os: "ios",
	},
	"ipad-pro": {
		label: "iPad Pro 11\"", width: 834, height: 1194, dpr: 2,
		ua:       "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
		platform: "iPad", os: "ios",
	},
	"pixel7": {
		label: "Google Pixel 7", width: 412, height: 915, dpr: 2.625,
		ua:       "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
		platform: "Linux armv81", os: "android",
	},
	"pixel8": {
		label: "Google Pixel 8 Pro", width: 448, height: 998, dpr: 2.625,
		ua:       "Mozilla/5.0 (Linux; Android 14; Pixel 8 Pro) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
		platform: "Linux armv81", os: "android",
	},
	"galaxy-s23": {
		label: "Samsung Galaxy S23", width: 360, height: 780, dpr: 3,
		ua:       "Mozilla/5.0 (Linux; Android 14; SM-S911B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
		platform: "Linux armv81", os: "android",
	},
	"galaxy-tab": {
		label: "Samsung Galaxy Tab S8", width: 753, height: 1205, dpr: 2,
		ua:       "Mozilla/5.0 (Linux; Android 14; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		platform: "Linux armv81", os: "android",
	},
}

// presetAliases maps friendly alternates onto canonical keys.
var presetAliases = map[string]string{
	"iphone":     "iphone15",
	"iphone-pro": "iphone15",
	"ios":        "iphone15",
	"pixel":      "pixel7",
	"android":    "pixel7",
	"galaxy":     "galaxy-s23",
	"samsung":    "galaxy-s23",
}

func lookupPreset(name string) (devicePreset, string, bool) {
	key := strings.ToLower(name)
	if canon, ok := presetAliases[key]; ok {
		key = canon
	}
	p, ok := devicePresets[key]
	return p, key, ok
}

// applyEmulation installs the device override on an open CDP connection.
// Overrides live only for the lifetime of this session — they clear when
// the websocket detaches, which is why emulate must navigate + capture in
// one shot rather than leaving state for a later command to read.
func applyEmulation(c *cdpClient, p devicePreset) error {
	if _, err := c.Call("Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             p.width,
		"height":            p.height,
		"deviceScaleFactor": p.dpr,
		"mobile":            true,
		"screenOrientation": map[string]any{"type": "portraitPrimary", "angle": 0},
	}, 5*time.Second); err != nil {
		return fmt.Errorf("setDeviceMetricsOverride: %w", err)
	}

	uaParams := map[string]any{
		"userAgent": p.ua,
		"platform":  p.platform,
	}
	// Android Chrome sends UA Client Hints; iOS Safari does not. Supplying
	// metadata only for Android keeps Sec-CH-UA-* headers honest so
	// server-side device detection branches the same way a real phone does.
	if p.os == "android" {
		uaParams["userAgentMetadata"] = map[string]any{
			"platform":        "Android",
			"platformVersion": "14.0.0",
			"architecture":    "",
			"model":           p.label,
			"mobile":          true,
		}
	}
	if _, err := c.Call("Emulation.setUserAgentOverride", uaParams, 5*time.Second); err != nil {
		return fmt.Errorf("setUserAgentOverride: %w", err)
	}

	if _, err := c.Call("Emulation.setTouchEmulationEnabled", map[string]any{
		"enabled":        true,
		"maxTouchPoints": 5,
	}, 5*time.Second); err != nil {
		return fmt.Errorf("setTouchEmulationEnabled: %w", err)
	}
	return nil
}

func browserEmulate(args []string) {
	clean := stripFlags(args)

	if len(clean) == 0 || clean[0] == "list" || clean[0] == "ls" || clean[0] == "--help" || clean[0] == "-h" {
		emulateUsage()
		return
	}

	p, key, ok := lookupPreset(clean[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "qai browser emulate: unknown device %q\n", clean[0])
		fmt.Fprintln(os.Stderr, "  → fix: run 'qai browser emulate list' to see available presets")
		os.Exit(1)
	}

	// Optional positional URL after the device name.
	var url string
	if len(clean) > 1 {
		url = clean[1]
	}

	// Emulation overrides clear when the session detaches, so do everything
	// — apply, navigate, screenshot — on one connection. Stealth is skipped
	// (it would pin navigator.platform=MacIntel / maxTouchPoints=0 and
	// defeat the mobile fingerprint).
	client, tab, err := connectToTabOpts(args, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	// Gate on the navigation target when one is given (matching `open`),
	// otherwise on the current tab — so a banking/SSO surface can't be
	// silently screenshotted under a mobile UA.
	gateTab := tab
	if url != "" {
		gateTab = &cdpTab{URL: url, ID: tab.ID}
	}
	if err := securityGate("emulate", gateTab, key); err != nil {
		os.Exit(1)
	}

	if err := applyEmulation(client, p); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser emulate: %v\n", err)
		os.Exit(1)
	}
	if err := setTheme(client, flagValue(args, "--theme")); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser emulate: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "📱 emulating %s — %d×%d @%.3gx (%s)\n", p.label, p.width, p.height, p.dpr, p.os)

	if url != "" {
		if _, err := client.Call("Page.navigate", map[string]any{"url": url}, 10*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "qai browser emulate: navigate: %v\n", err)
			os.Exit(1)
		}
		if err := client.WaitEvent("Page.loadEventFired", 15*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "warning: page load timeout, capturing anyway\n")
		}
		time.Sleep(400 * time.Millisecond) // let layout/JS settle post-load
	}

	// --wait <selector> AFTER load gives JS-driven content time to
	// render before we capture. --selector limits the capture to one
	// element's box. Both compose: wait for one selector, capture
	// another, or capture the same one with both flags set.
	if waitSel := flagValue(args, "--wait"); waitSel != "" {
		if err := waitForSelector(client, waitSel, parseWaitTimeout(args)); err != nil {
			fmt.Fprintf(os.Stderr, "qai browser emulate: %v\n", err)
			os.Exit(1)
		}
	}

	selector := flagValue(args, "--selector")
	pngData, err := captureScreenshot(client, selector)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser emulate: screenshot: %v\n", err)
		os.Exit(1)
	}

	outFile := flagValue(args, "-o")
	if outFile == "" {
		outFile = fmt.Sprintf("emulate-%s-%s.png", key, time.Now().Format("20060102-150405"))
	}
	if err := os.WriteFile(outFile, pngData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser emulate: write %s: %v\n", outFile, err)
		os.Exit(1)
	}
	scope := fmt.Sprintf("%dpx wide", int(float64(p.width)*p.dpr))
	if selector != "" {
		scope = fmt.Sprintf("element %q", selector)
	}
	fmt.Printf("saved: %s (%d bytes, %s)\n", outFile, len(pngData), scope)
}

func emulateUsage() {
	// Sort canonical presets for stable display.
	keys := make([]string, 0, len(devicePresets))
	for k := range devicePresets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprint(os.Stderr, `qai browser emulate — render a page as a phone/tablet via CDP device mode

  qai browser emulate <device> [url] [-o file.png] [flags]

Applies a mobile viewport, device-pixel-ratio, touch, and user-agent, then
captures a screenshot. Navigate + capture happen on one connection because
the override clears when the connection closes.

Flags:
  --selector <css>      Capture only the matched element's bounding box,
                        not the full viewport. scrollIntoView first.
  --wait <css>          After load, poll until <css> is in the DOM (or
                        --timeout elapses) before capturing.
  --theme light|dark    Force prefers-color-scheme via
                        Emulation.setEmulatedMedia. Applied pre-load
                        so first-paint sees the right media query.
  --timeout <dur>       Cap for --wait (default 10s; "5", "5s", "1m" all parse).
  -o <file>             Output filename (default emulate-<device>-<ts>.png).

NOTE: this is Blink rendering a mobile viewport (Chrome DevTools device mode),
not WebKit. It reproduces responsive layout, viewport sizing, touch, and any
UA/platform-sniffing branch — but NOT genuine iOS Safari rendering quirks.
For true iOS rendering use a real iOS Simulator + Safari.

Devices:
`)
	for _, k := range keys {
		p := devicePresets[k]
		fmt.Fprintf(os.Stderr, "  %-14s %-22s %d×%d @%.3gx  (%s)\n", k, p.label, p.width, p.height, p.dpr, p.os)
	}
	fmt.Fprint(os.Stderr, `
Aliases: iphone→iphone15, ios→iphone15, pixel/android→pixel7, galaxy→galaxy-s23

Examples:
  qai browser emulate iphone15 https://example.com -o ios.png
  qai browser emulate pixel7   https://example.com -o android.png
  qai browser emulate galaxy-s23                                 # current tab, auto-named
  qai browser emulate iphone15 https://x.com --selector header   # just the header element
  qai browser emulate iphone15 https://x.com --theme dark        # forced dark mode
  qai browser emulate iphone15 https://x.com --wait "main.loaded" # wait for JS-driven content
`)
}
