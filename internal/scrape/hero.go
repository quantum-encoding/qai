package scrape

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"regexp"

	// Register PNG/JPEG decoders so image.DecodeConfig can read either
	// without the caller knowing the format in advance. Importing for
	// side effect — these init() calls register with image package.
	_ "image/png"
)

// resourceRefRe matches Joplin markdown image references, which look
// like ![alt](:/abcdef0123...32hex).
var resourceRefRe = regexp.MustCompile(`!\[[^\]]*\]\(:/([a-f0-9]{32})\)`)

// pickHero walks the note body's image references in order, fetches
// each via the Joplin Data API, and returns the first image that passes
// the preset's filters.
//
// The body-order-first strategy works because on marketplace listings
// the real hero is the first meaningful image in the page body — promo
// banners precede it in the DOM but fail the aspect-ratio check, so
// they get naturally filtered out before the hero is reached.
func pickHero(token, noteBody string, f ImageFilters) ([]byte, error) {
	debug := os.Getenv("QAI_SCRAPE_DEBUG") != ""
	matches := resourceRefRe.FindAllStringSubmatch(noteBody, -1)
	if debug {
		fmt.Fprintf(os.Stderr, "  ▶ pickHero: %d refs in body, filters=%+v\n", len(matches), f)
	}
	seen := map[string]bool{}
	for i, m := range matches {
		resID := m[1]
		if seen[resID] {
			continue
		}
		seen[resID] = true

		meta, err := getResourceMeta(token, resID)
		if err != nil {
			if debug {
				fmt.Fprintf(os.Stderr, "    [%d] %s: meta err: %v\n", i, resID, err)
			}
			continue
		}
		if !extensionAllowed(meta, f) {
			if debug {
				fmt.Fprintf(os.Stderr, "    [%d] %s ext=%s mime=%s sz=%d: ext rejected\n", i, resID, meta.FileExt, meta.Mime, meta.Size)
			}
			continue
		}
		// Pre-skip: if size is below the byte floor AND below the
		// expected hero dims (a 350×350 JPEG is ~30KB minimum), we
		// can avoid downloading. For undecodable formats this is the
		// primary signal anyway.
		if f.MinSizeBytes > 0 && meta.Size < int64(f.MinSizeBytes) {
			if debug {
				fmt.Fprintf(os.Stderr, "    [%d] %s ext=%s sz=%d: below MinSizeBytes %d\n", i, resID, meta.FileExt, meta.Size, f.MinSizeBytes)
			}
			continue
		}

		data, err := getResourceFile(token, resID)
		if err != nil {
			if debug {
				fmt.Fprintf(os.Stderr, "    [%d] %s: file err: %v\n", i, resID, err)
			}
			continue
		}

		w, h, ok := decodeImageDims(data)
		if ok {
			if !dimsOK(w, h, f) {
				if debug {
					fmt.Fprintf(os.Stderr, "    [%d] %s ext=%s sz=%d dims=%dx%d: dims rejected\n", i, resID, meta.FileExt, meta.Size, w, h)
				}
				continue
			}
			if debug {
				fmt.Fprintf(os.Stderr, "    [%d] %s ext=%s sz=%d dims=%dx%d: ACCEPTED\n", i, resID, meta.FileExt, meta.Size, w, h)
			}
			return data, nil
		}
		// Dim decode failed (typically AVIF — Go stdlib doesn't decode
		// it). The size-floor pre-check above already passed; accept
		// this image as the hero.
		if f.MinSizeBytes > 0 && len(data) >= f.MinSizeBytes {
			if debug {
				fmt.Fprintf(os.Stderr, "    [%d] %s ext=%s sz=%d: undecodable but size>=floor, ACCEPTED\n", i, resID, meta.FileExt, meta.Size)
			}
			return data, nil
		}
		if debug {
			fmt.Fprintf(os.Stderr, "    [%d] %s ext=%s sz=%d: undecodable, no size floor\n", i, resID, meta.FileExt, meta.Size)
		}
	}
	if debug {
		fmt.Fprintf(os.Stderr, "  ▶ pickHero: walked all refs, nothing accepted\n")
	}
	return nil, nil
}

func extensionAllowed(r *joplinResource, f ImageFilters) bool {
	ext := lower(r.FileExt)
	if f.JPEGOnly {
		return ext == "jpg" || ext == "jpeg" || r.Mime == "image/jpeg" || r.Mime == "image/jpg"
	}
	// Permissive: allow jpg/png/webp/avif. AVIF is added for AliExpress
	// which serves heroes as AVIF; the dim-decode falls back to byte-
	// size filtering since Go stdlib can't decode AVIF natively.
	switch ext {
	case "jpg", "jpeg", "png", "webp", "avif":
		return true
	}
	return false
}

func dimsOK(w, h int, f ImageFilters) bool {
	if f.MinW > 0 && w < f.MinW {
		return false
	}
	if f.MinH > 0 && h < f.MinH {
		return false
	}
	if f.MaxLongSide > 0 {
		long := w
		if h > long {
			long = h
		}
		if long > f.MaxLongSide {
			return false
		}
	}
	if f.MaxAspectRatio > 0 {
		ratio := float64(w) / float64(h)
		if h > w {
			ratio = float64(h) / float64(w)
		}
		if ratio > f.MaxAspectRatio {
			return false
		}
	}
	return true
}

// jpegDims is kept for the strict JPEGOnly callers — reads the JPEG
// header without decoding pixels. New callers should use
// decodeImageDims which tries every registered format.
func jpegDims(data []byte) (w, h int, ok bool) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// decodeImageDims uses the standard image package's DecodeConfig
// dispatcher to read dimensions from JPEG or PNG headers (anything
// else registered via blank-import). AVIF / WebP fall through with
// ok=false; callers can fall back to byte-size filtering.
func decodeImageDims(data []byte) (w, h int, ok bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

func lower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return string(out)
}
