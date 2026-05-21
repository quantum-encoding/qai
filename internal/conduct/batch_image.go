// batch_image.go — `qai image --batch <file>` runs N image generations
// from a single command. Prompts come one-per-line from a file (or
// stdin via `--batch -`). Common flags (--model, --count, --aspect,
// --size) apply to every prompt.
//
// The shape mirrors `qai edit --batch` but is simpler: image gen has
// no Vertex batch primitive on the broker, so this is parallel mode
// only — a bounded worker pool firing the same /qai/v1/images/generate
// endpoint that single-shot `qai image` uses.
//
// Output: as each generation completes, its file path is printed to
// stdout (one per line) so the caller can pipe into another tool.
// Per-prompt status (started / done / error) goes to stderr so
// stdout stays piping-clean.

package conduct

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// runImageBatch is dispatched from conductImage when --batch is set.
// `commonBody` is the per-prompt template (model, count, aspect, size)
// pre-populated by conductImage's flag parser; this function clones it
// per prompt and overrides the prompt field.
func runImageBatch(batchPath string, parallel int, commonBody map[string]any) {
	prompts, err := readPrompts(batchPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai image --batch: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: pass a path to a text file with one prompt per line, or `-` for stdin")
		os.Exit(1)
	}
	if len(prompts) == 0 {
		fmt.Fprintln(os.Stderr, "qai image --batch: no prompts found (file is empty or only comments)")
		fmt.Fprintln(os.Stderr, "  → fix: add at least one non-blank, non-#-prefixed line")
		os.Exit(1)
	}
	if parallel < 1 {
		parallel = 1
	}
	if parallel > len(prompts) {
		parallel = len(prompts)
	}

	fmt.Fprintf(os.Stderr, "▶ %d prompt(s), parallel=%d, model=%v\n", len(prompts), parallel, commonBody["model"])

	type result struct {
		idx    int
		prompt string
		paths  []string
		err    error
	}

	jobs := make(chan int, len(prompts))
	results := make(chan result, len(prompts))
	var wg sync.WaitGroup

	for w := 0; w < parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				body := cloneBody(commonBody)
				body["prompt"] = prompts[idx]
				fmt.Fprintf(os.Stderr, "  [%d/%d] starting: %s\n", idx+1, len(prompts), truncate(prompts[idx], 60))
				paths, err := generateOne(body)
				results <- result{idx: idx, prompt: prompts[idx], paths: paths, err: err}
			}
		}()
	}

	for i := range prompts {
		jobs <- i
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	// Drain results, print paths to stdout, status to stderr.
	// Don't preserve completion order on stderr — but path lines on
	// stdout are tagged with the prompt index so the caller can
	// reorder if they need to.
	var failures int
	for r := range results {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ [%d/%d] %v\n", r.idx+1, len(prompts), r.err)
			failures++
			continue
		}
		for _, p := range r.paths {
			fmt.Printf("%d\t%s\n", r.idx+1, p)
		}
		fmt.Fprintf(os.Stderr, "  ✓ [%d/%d] %d image(s)\n", r.idx+1, len(prompts), len(r.paths))
	}

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "qai image --batch: %d/%d prompt(s) failed\n", failures, len(prompts))
		os.Exit(1)
	}
}

// generateOne fires one image generation and returns the saved paths.
// Reused logic from conductImage's response-handling — kept in sync
// with the single-shot path's parsing.
func generateOne(body map[string]any) ([]string, error) {
	data, err := qaiAPI("POST", "/qai/v1/images/generate", body)
	if err != nil {
		// dieAPI would exit; we need to surface the error to the
		// batch loop instead so one failure doesn't kill the rest.
		return nil, err
	}
	var resp map[string]any
	if json.Unmarshal(data, &resp) != nil {
		return nil, fmt.Errorf("could not parse broker response (raw=%s)", truncate(string(data), 200))
	}
	var paths []string
	if images, ok := resp["images"].([]any); ok {
		for _, img := range images {
			if imgMap, ok := img.(map[string]any); ok {
				if b64, ok := imgMap["base64"].(string); ok {
					if p := saveBase64(b64, "Pictures/generated", ".png"); p != "" {
						paths = append(paths, p)
					}
				}
			}
		}
	} else if b64, ok := resp["base64"].(string); ok {
		if p := saveBase64(b64, "Pictures/generated", ".png"); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no images in response (raw=%s)", truncate(string(data), 200))
	}
	return paths, nil
}

// readPrompts loads prompts from a file path (or `-` for stdin),
// returning one entry per non-blank, non-#-prefixed line. Trailing
// whitespace is stripped; leading whitespace is preserved (a leading
// space might be intentional in a prompt).
func readPrompts(path string) ([]string, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		r = f
	}
	var out []string
	sc := bufio.NewScanner(r)
	// Allow long prompts.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r\n")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read prompts: %w", err)
	}
	return out, nil
}

// cloneBody returns a shallow copy of m. Sufficient because the request
// body's values are all primitives (strings, ints) — no nested maps to
// deep-copy.
func cloneBody(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
