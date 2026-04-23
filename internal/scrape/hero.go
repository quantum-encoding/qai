package scrape

import (
	"bytes"
	"image/jpeg"
	"regexp"
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
	for _, m := range resourceRefRe.FindAllStringSubmatch(noteBody, -1) {
		resID := m[1]

		meta, err := getResourceMeta(token, resID)
		if err != nil {
			continue
		}
		if !extensionAllowed(meta, f) {
			continue
		}

		data, err := getResourceFile(token, resID)
		if err != nil {
			continue
		}

		w, h, ok := jpegDims(data)
		if !ok {
			continue
		}
		if !dimsOK(w, h, f) {
			continue
		}
		return data, nil
	}
	return nil, nil
}

func extensionAllowed(r *joplinResource, f ImageFilters) bool {
	ext := lower(r.FileExt)
	if f.JPEGOnly {
		return ext == "jpg" || ext == "jpeg" || r.Mime == "image/jpeg" || r.Mime == "image/jpg"
	}
	// Permissive: allow jpg/png/webp.
	switch ext {
	case "jpg", "jpeg", "png", "webp":
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

// jpegDims reads the JPEG header only (no full decode) to get
// dimensions cheaply.
func jpegDims(data []byte) (w, h int, ok bool) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
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
