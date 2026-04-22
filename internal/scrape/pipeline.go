package scrape

import (
	"fmt"
	"os"
	"path/filepath"
)

// runOne runs the full scrape pipeline for a single URL and returns
// the resulting Brief. Non-fatal failures (e.g. no product hero found,
// parse didn't extract a title) show up as empty fields on the Brief;
// only hard failures (clip timeout, note not indexed) return an error.
func runOne(productURL string, preset *Preset, opts *flags) (*Brief, error) {
	id := preset.ExtractID(productURL)
	if id == "" {
		return nil, fmt.Errorf("preset %q could not extract ID from %s", preset.Name, productURL)
	}

	notebook := opts.notebook
	if notebook == "" {
		notebook = preset.DefaultNotebook
		if notebook == "" {
			notebook = "ProductResearch"
		}
	}

	noteTitle := fmt.Sprintf("%s %s", presetTitlePrefix(preset.Name), id)
	fmt.Fprintf(os.Stderr, "▶ [%s] clipping %s → Joplin[%s]\n", preset.Name, productURL, notebook)

	noteID, err := qaiClip(productURL, notebook, noteTitle)
	if err != nil {
		return nil, fmt.Errorf("clip failed: %w", err)
	}

	token, err := joplinToken()
	if err != nil {
		return nil, err
	}

	// Prefer direct fetch by note ID (no index lag); fall back to
	// title-search if clip didn't surface an ID for any reason.
	var note *joplinNote
	if noteID != "" {
		note, err = getNoteByID(token, noteID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ▶ direct fetch failed (%v), falling back to search\n", err)
		}
	}
	if note == nil {
		note, err = findNoteByTitle(token, noteTitle)
		if err != nil {
			return nil, err
		}
	}

	brief := &Brief{
		URL:          productURL,
		Preset:       preset.Name,
		ID:           id,
		JoplinNoteID: note.ID,
	}

	if preset.Parse != nil {
		brief.Data = preset.Parse(note.Body, id)
		if t, ok := brief.Data["title"].(string); ok {
			brief.Title = t
		}
	}

	// Image extraction (non-fatal).
	hero, err := pickHero(token, note.Body, preset.ImageFilters)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ▶ image extraction failed: %v\n", err)
	} else if hero == nil {
		fmt.Fprintf(os.Stderr, "  ▶ no viable hero image found for %s\n", id)
	} else {
		imageDir := opts.imageDir
		if !filepath.IsAbs(imageDir) {
			abs, err := filepath.Abs(imageDir)
			if err == nil {
				imageDir = abs
			}
		}
		if err := os.MkdirAll(imageDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "  ▶ mkdir %s: %v\n", imageDir, err)
		} else {
			dst := filepath.Join(imageDir, id+".jpg")
			if err := os.WriteFile(dst, hero, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "  ▶ write hero: %v\n", err)
			} else {
				brief.ImagePath = siteRelativePath(dst)
				fmt.Fprintf(os.Stderr, "  ▶ hero saved: %s\n", brief.ImagePath)
			}
		}
	}

	return brief, nil
}

// presetTitlePrefix returns the first-letter-capitalized preset name
// used in Joplin note titles ("Amazon B0BT9R5XNN", "Newegg N8241…").
func presetTitlePrefix(name string) string {
	if name == "" {
		return "Scrape"
	}
	return capFirst(name)
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 32
	}
	return string(b)
}
