package scrape

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// fetchHTML performs a plain HTTP GET with browser-like headers and
// returns the response body. Used for scout mode — search / listing
// pages are server-rendered on most marketplaces (Amazon, eBay,
// Best Buy), so a direct fetch extracts all the product IDs we need
// without the overhead of launching a real browser via qai clip.
//
// Product detail pages still need the clip path (they have stricter
// anti-bot and often JS-render the price / availability).
func fetchHTML(url string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	// A realistic desktop User-Agent; Amazon's listing endpoint serves
	// the full HTML to this profile without redirecting to an interstitial.
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
			"AppleWebKit/605.1.15 (KHTML, like Gecko) "+
			"Version/17.0 Safari/605.1.15")
	req.Header.Set("Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-GB,en;q=0.9")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GET %s: http %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
