// init_cmd.go — Interactive setup wizard and embedding provider dispatch.
//
// qai init walks users through first-time configuration:
//   - Choose embedding provider (Ollama / Gemini / OpenAI / QAI managed)
//   - Validate API keys
//   - Start local SurrealDB
//   - Write ~/.qai/config.yaml

package initcmd

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/db"
	"github.com/quantum-encoding/qai-cli/internal/embedding"
)

// ─── qai init ─────────────────────────────────────────────────────────────

func CmdInit(args []string) {
	fmt.Println(`
  ╔══════════════════════════════════════════╗
  ║           qai — first-time setup        ║
  ╚══════════════════════════════════════════╝`)

	c := config.Default()

	// Step 1: Embedding provider
	fmt.Println(`
  Choose your embedding provider:

    1. Ollama        — fully local, no API key, zero cost
                       requires: ollama + nomic-embed-text model
    2. Google Gemini — free tier available (1500 req/day)
                       requires: API key from aistudio.google.com
    3. OpenAI        — text-embedding-3-small
                       requires: API key from platform.openai.com
    4. QAI Managed   — everything through api.quantumencoding.ai
                       requires: QAI API key`)

	choice := promptChoice("\n  Your choice", 4)

	switch choice {
	case 1: // Ollama
		c.Embeddings.Provider = "ollama"
		c.Embeddings.Endpoint = promptString("  Ollama endpoint", "http://localhost:11434")
		c.Embeddings.Model = promptString("  Embedding model", "nomic-embed-text")
		c.Embeddings.Dimensions = 768

		// Validate
		fmt.Fprintf(os.Stderr, "  checking Ollama...")
		resp, err := http.Get(c.Embeddings.Endpoint + "/api/tags")
		if err != nil {
			fmt.Fprintf(os.Stderr, " not reachable\n")
			fmt.Println("\n  Ollama not detected. Install from https://ollama.ai then run:")
			fmt.Printf("    ollama pull %s\n", c.Embeddings.Model)
			fmt.Println("  You can still save this config and set up Ollama later.")
		} else {
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, " connected!\n")
		}

	case 2: // Gemini
		c.Embeddings.Provider = "gemini"
		c.Embeddings.Model = "gemini-embedding-2-preview"
		c.Embeddings.Dimensions = 768
		c.Embeddings.APIKey = promptString("  Gemini API key (from aistudio.google.com)", "")
		if c.Embeddings.APIKey != "" {
			fmt.Fprintf(os.Stderr, "  validating key...")
			_, err := embedding.Gemini(c.Embeddings.APIKey, c.Embeddings.Model, c.Embeddings.Dimensions, []string{"test"})
			if err != nil {
				fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, " valid!\n")
			}
		}

	case 3: // OpenAI
		c.Embeddings.Provider = "openai"
		c.Embeddings.Model = "text-embedding-3-small"
		c.Embeddings.Dimensions = 768
		c.Embeddings.APIKey = promptString("  OpenAI API key (from platform.openai.com)", "")
		if c.Embeddings.APIKey != "" {
			fmt.Fprintf(os.Stderr, "  validating key...")
			_, err := embedding.OpenAI(c.Embeddings.APIKey, c.Embeddings.Model, c.Embeddings.Dimensions, []string{"test"})
			if err != nil {
				fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, " valid!\n")
			}
		}

	case 4: // QAI Managed
		c.Embeddings.Provider = "qai"
		c.Embeddings.Model = "gemini-embedding-2-preview"
		c.Embeddings.Dimensions = 768
		c.API.APIKey = promptString("  QAI API key (qai_k_...)", "")
		if c.API.APIKey != "" {
			fmt.Fprintf(os.Stderr, "  checking balance...")
			_, err := embedding.QAI(c.API.APIKey, c.API.BaseURL, c.Embeddings.Model, c.Embeddings.Dimensions, []string{"test"})
			if err != nil {
				fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, " valid!\n")
			}
		}
	}

	// Step 2: Local SurrealDB
	fmt.Println()
	if promptYN("  Start local SurrealDB for knowledge storage?", true) {
		if !db.IsRunning() {
			fmt.Println()
			db.Start()
		} else {
			fmt.Printf("  SurrealDB already running on port %d\n", c.Surreal.LocalPort)
		}
	}

	// Step 3: Save config
	fmt.Println()
	if err := config.Save(c); err != nil {
		fmt.Fprintf(os.Stderr, "  save config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Configuration saved to %s\n", config.ConfigPath())


	// Step 4: Try-it commands
	fmt.Println(`
  You're all set! Try:

    qai db status                             # check knowledge base
    qai ingest --local my-docs ~/Documents/   # embed + store documents
    qai search --local "how does X work"      # vector similarity search`)

	if c.API.APIKey != "" {
		fmt.Println(`    qai conduct models                        # list available AI models
    qai audit . --dry-run                     # preview code audit`)
	}
	fmt.Println()
}

// ─── input helpers ────────────────────────────────────────────────────────

var stdinReader = bufio.NewReader(os.Stdin)

func promptString(prompt, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", prompt, defaultVal)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal
	}
	return line
}

func promptChoice(prompt string, max int) int {
	for {
		fmt.Printf("%s [1-%d]: ", prompt, max)
		line, _ := stdinReader.ReadString('\n')
		line = strings.TrimSpace(line)
		n := 0
		if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= 1 && n <= max {
			return n
		}
		fmt.Printf("  please enter 1-%d\n", max)
	}
}

func promptYN(prompt string, defaultYes bool) bool {
	hint := "[Y/n]"
	if !defaultYes {
		hint = "[y/N]"
	}
	fmt.Printf("%s %s: ", prompt, hint)
	line, _ := stdinReader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return defaultYes
	}
	return line == "y" || line == "yes"
}
