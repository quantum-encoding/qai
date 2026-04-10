// chunker.go — Basic document chunking (replaces axiom binary).
//
// Splits markdown/text files into chunks by headings and word count.
// Not as sophisticated as axiom, but covers 90% of use cases with zero deps.

package ingest

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultChunkSize = 500  // target words per chunk
	minChunkSize     = 50   // minimum words to keep a chunk
)

// chunkDocsNative walks a path and splits all docs into chunks.
// Returns chunks with source file attribution.
func chunkDocsNative(path string) []chunk {
	var chunks []chunk

	info, err := os.Stat(path)
	if err != nil {
		return nil
	}

	if info.IsDir() {
		filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if ext == ".md" || ext == ".txt" || ext == ".mdx" || ext == ".rst" {
				rel, _ := filepath.Rel(path, p)
				fileChunks := chunkFileNative(p, rel)
				chunks = append(chunks, fileChunks...)
			}
			return nil
		})
	} else {
		chunks = chunkFileNative(path, filepath.Base(path))
	}

	return chunks
}

// chunkFileNative splits a single file into chunks by headings and word count.
func chunkFileNative(path, sourceFile string) []chunk {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	text := string(data)
	if strings.TrimSpace(text) == "" {
		return nil
	}

	// Split by markdown headings (# ## ### etc.)
	sections := splitByHeadings(text)

	var chunks []chunk
	for _, section := range sections {
		words := countWords(section)
		if words < minChunkSize {
			// Try to merge with previous chunk
			if len(chunks) > 0 {
				chunks[len(chunks)-1].text += "\n\n" + section
				continue
			}
		}

		if words <= defaultChunkSize {
			chunks = append(chunks, chunk{text: strings.TrimSpace(section), sourceFile: sourceFile})
		} else {
			// Split large sections by paragraphs
			subChunks := splitBySize(section, defaultChunkSize)
			for _, sc := range subChunks {
				if countWords(sc) >= minChunkSize {
					chunks = append(chunks, chunk{text: strings.TrimSpace(sc), sourceFile: sourceFile})
				}
			}
		}
	}

	return chunks
}

// splitByHeadings splits text at markdown heading boundaries.
func splitByHeadings(text string) []string {
	lines := strings.Split(text, "\n")
	var sections []string
	var current strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ") {
			if current.Len() > 0 {
				sections = append(sections, current.String())
				current.Reset()
			}
		}
		current.WriteString(line)
		current.WriteString("\n")
	}
	if current.Len() > 0 {
		sections = append(sections, current.String())
	}

	return sections
}

// splitBySize splits text into chunks of approximately targetWords words.
func splitBySize(text string, targetWords int) []string {
	paragraphs := strings.Split(text, "\n\n")
	var chunks []string
	var current strings.Builder
	currentWords := 0

	for _, para := range paragraphs {
		paraWords := countWords(para)

		if currentWords+paraWords > targetWords && currentWords > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
			currentWords = 0
		}

		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
		currentWords += paraWords
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

func countWords(s string) int {
	return len(strings.Fields(s))
}
