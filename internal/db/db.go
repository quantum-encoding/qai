// db.go — Manage the local SurrealDB instance.
//
// Provides start/stop/status for the local knowledge base that backs
// qai search --local and qai ingest --local. Data persists in ~/.qai/surrealdb/.
//
// Usage:
//
//	qai db start       # start SurrealDB in the background
//	qai db stop        # stop the running instance
//	qai db status      # check if running + show stats
//	qai db info        # list providers and chunk counts
//	qai db shell       # open interactive SQL shell

package db

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/platform"
)

var Cfg *config.Config

const (
	dbNS   = "qai"
	dbName = "knowledge"
)





var dbDataDir = filepath.Join(config.Home, ".qai", "surrealdb", "data")
var dbPidFile = filepath.Join(config.Home, ".qai", "surrealdb", "surreal.pid")

func CmdDB(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	switch args[0] {
	case "start":
		Start()
	case "stop":
		Stop()
	case "status", "st":
		Status()
	case "info":
		Info()
	case "shell", "sql":
		Shell()
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "qai db: unknown action %q\n", args[0])
		usage()
		os.Exit(1)
	}
}

func Start() {
	// Check if already running
	if IsRunning() {
		fmt.Println("SurrealDB already running on localhost:" + strconv.Itoa(Cfg.GetDBPort()))
		return
	}

	// Ensure data dir exists
	os.MkdirAll(filepath.Dir(dbPidFile), 0755)
	os.MkdirAll(dbDataDir, 0755)

	// Find surreal binary
	surrealBin, err := exec.LookPath("surreal")
	if err != nil {
		fmt.Fprintln(os.Stderr, "qai db: surreal not found on PATH — install from https://surrealdb.com/install")
		os.Exit(1)
	}

	// Start in background
	cmd := exec.Command(surrealBin, "start",
		"--bind", fmt.Sprintf("127.0.0.1:%d", Cfg.GetDBPort()),
		"--user", Cfg.GetDBUser(),
		"--pass", Cfg.GetDBPass(),
		"--no-banner",
		"rocksdb://"+dbDataDir,
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	platform.DetachProcess(cmd)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "qai db: start failed: %v\n", err)
		os.Exit(1)
	}

	// Write PID file before releasing
	pid := cmd.Process.Pid
	if err := os.WriteFile(dbPidFile, []byte(strconv.Itoa(pid)), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write pid file: %v\n", err)
	}

	// Detach
	cmd.Process.Release()

	// Wait for ready
	fmt.Fprintf(os.Stderr, "starting SurrealDB on localhost:%d...", Cfg.GetDBPort())
	for range 30 {
		time.Sleep(200 * time.Millisecond)
		if IsRunning() {
			fmt.Fprintln(os.Stderr, " ready!")
			EnsureSchema()
			fmt.Printf("SurrealDB running (pid %d, port %d)\n", pid, Cfg.GetDBPort())
			fmt.Printf("data: %s\n", dbDataDir)
			return
		}
	}
	fmt.Fprintln(os.Stderr, " timeout")
	fmt.Fprintln(os.Stderr, "qai db: SurrealDB did not start in time")
	os.Exit(1)
}

func Stop() {
	pid := readPid()
	if pid == 0 {
		// Try to find by port
		if !IsRunning() {
			fmt.Println("SurrealDB is not running")
			return
		}
		fmt.Fprintln(os.Stderr, "qai db: no PID file, kill manually: lsof -ti :"+strconv.Itoa(Cfg.GetDBPort())+" | xargs kill")
		os.Exit(1)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Println("SurrealDB is not running")
		os.Remove(dbPidFile)
		return
	}

	if err := platform.SignalProcess(proc, "term"); err != nil {
		fmt.Println("SurrealDB is not running (stale PID)")
		os.Remove(dbPidFile)
		return
	}

	// Wait for exit
	for range 20 {
		time.Sleep(200 * time.Millisecond)
		if !IsRunning() {
			os.Remove(dbPidFile)
			fmt.Println("SurrealDB stopped")
			return
		}
	}

	// Force kill
	platform.SignalProcess(proc, "kill")
	os.Remove(dbPidFile)
	fmt.Println("SurrealDB killed")
}

func Status() {
	if !IsRunning() {
		fmt.Println("SurrealDB: not running")
		fmt.Printf("start with: qai db start\n")
		return
	}

	pid := readPid()
	fmt.Printf("SurrealDB: running (pid %d, port %d)\n", pid, Cfg.GetDBPort())
	fmt.Printf("data: %s\n", dbDataDir)

	// Show DB stats
	result := Query("SELECT count() AS total, provider, count() AS count FROM doc_chunk GROUP BY provider;")
	if result == "" {
		return
	}

	var stmts []struct {
		Result []struct {
			Provider string `json:"provider"`
			Count    int    `json:"count"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(result), &stmts) == nil && len(stmts) > 0 && len(stmts[0].Result) > 0 {
		fmt.Println("\nproviders:")
		total := 0
		for _, r := range stmts[0].Result {
			fmt.Printf("  %-20s %d chunks\n", r.Provider, r.Count)
			total += r.Count
		}
		fmt.Printf("  %-20s %d chunks\n", "TOTAL", total)
	}
}

func Info() {
	if !IsRunning() {
		fmt.Fprintln(os.Stderr, "qai db: not running — start with: qai db start")
		os.Exit(1)
	}

	result := Query("SELECT provider, dimension, count() AS chunks, math::sum(string::len(text)) AS total_chars FROM doc_chunk GROUP BY provider, dimension ORDER BY chunks DESC;")
	if result == "" {
		return
	}

	var stmts []struct {
		Status string `json:"status"`
		Result []struct {
			Provider   string `json:"provider"`
			Dimension  *int   `json:"dimension"`
			Chunks     int    `json:"chunks"`
			TotalChars int    `json:"total_chars"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result), &stmts); err != nil || len(stmts) == 0 {
		fmt.Fprintln(os.Stderr, "qai db: query failed")
		return
	}

	if len(stmts[0].Result) == 0 {
		fmt.Println("no data — ingest with: qai ingest --local <provider> <path>")
		return
	}

	fmt.Printf("%-24s %6s %8s %10s\n", "Provider", "Dim", "Chunks", "Size")
	fmt.Println(strings.Repeat("─", 52))
	totalChunks := 0
	totalChars := 0
	for _, r := range stmts[0].Result {
		sizeStr := FormatSize(r.TotalChars)
		dimStr := "—"
		if r.Dimension != nil {
			dimStr = strconv.Itoa(*r.Dimension)
		}
		fmt.Printf("%-24s %6s %8d %10s\n", r.Provider, dimStr, r.Chunks, sizeStr)
		totalChunks += r.Chunks
		totalChars += r.TotalChars
	}
	fmt.Println(strings.Repeat("─", 52))
	fmt.Printf("%-24s %6s %8d %10s\n", "TOTAL", "", totalChunks, FormatSize(totalChars))
}

func Shell() {
	if !IsRunning() {
		fmt.Fprintln(os.Stderr, "qai db: not running — start with: qai db start")
		os.Exit(1)
	}

	cmd := exec.Command("surreal", "sql",
		"--endpoint", fmt.Sprintf("http://127.0.0.1:%d", Cfg.GetDBPort()),
		"--username", Cfg.GetDBUser(),
		"--password", Cfg.GetDBPass(),
		"--namespace", dbNS,
		"--database", dbName,
	)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

// ─── helpers ──────────────────────────────────────────────────────────────

func IsRunning() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", Cfg.GetDBPort()))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func readPid() int {
	data, err := os.ReadFile(dbPidFile)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

func Query(sql string) string {
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/sql", Cfg.GetDBPort()), strings.NewReader(sql))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+Cfg.LocalSurrealAuth())
	req.Header.Set("Surreal-NS", dbNS)
	req.Header.Set("Surreal-DB", dbName)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func EnsureSchema() {
	// Dimension-agnostic schema: no HNSW index (it locks to one dimension).
	// Queries use brute-force KNN (<|K,COSINE|>) filtered by dimension field,
	// which is fast for local use (<100K vectors).
	schema := `
DEFINE NAMESPACE IF NOT EXISTS qai;
USE NS qai;
DEFINE DATABASE IF NOT EXISTS knowledge;
USE DB knowledge;
DEFINE TABLE IF NOT EXISTS doc_chunk SCHEMAFULL;
DEFINE FIELD IF NOT EXISTS provider ON doc_chunk TYPE string;
DEFINE FIELD IF NOT EXISTS source_file ON doc_chunk TYPE string;
DEFINE FIELD IF NOT EXISTS text ON doc_chunk TYPE string;
DEFINE FIELD IF NOT EXISTS embedding ON doc_chunk TYPE array<float>;
DEFINE FIELD IF NOT EXISTS dimension ON doc_chunk TYPE option<int>;
DEFINE FIELD IF NOT EXISTS title ON doc_chunk TYPE option<string>;
DEFINE FIELD IF NOT EXISTS chunk_id ON doc_chunk TYPE option<string>;
DEFINE FIELD IF NOT EXISTS clearance ON doc_chunk TYPE option<int>;
DEFINE FIELD IF NOT EXISTS created_at ON doc_chunk TYPE datetime DEFAULT time::now();
DEFINE INDEX IF NOT EXISTS idx_provider ON doc_chunk FIELDS provider;
DEFINE INDEX IF NOT EXISTS idx_dimension ON doc_chunk FIELDS dimension;
DEFINE INDEX IF NOT EXISTS idx_clearance ON doc_chunk FIELDS clearance;
`
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/sql", Cfg.GetDBPort()), strings.NewReader(schema))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+Cfg.LocalSurrealAuth())

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func FormatSize(chars int) string {
	if chars < 1024 {
		return fmt.Sprintf("%d B", chars)
	}
	if chars < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(chars)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(chars)/(1024*1024))
}

func usage() {
	fmt.Fprint(os.Stderr, `qai db — manage local SurrealDB knowledge base

Commands:
  qai db start         Start SurrealDB in background (port 9473)
  qai db stop          Stop the running instance
  qai db status        Check if running + show provider stats
  qai db info          List all providers with chunk counts and sizes
  qai db shell         Open interactive SurrealQL shell

Data stored in ~/.qai/surrealdb/data (persistent RocksDB).

Related:
  qai ingest --local <provider> <path>   Embed + store docs
  qai search --local "query" [provider]  Vector similarity search
`)
}

func Exec(conn config.SurrealConn, sql string) {
	req, _ := http.NewRequest("POST", conn.URL+"/sql", strings.NewReader(sql))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+conn.Auth)
	req.Header.Set("Surreal-NS", conn.NS)
	req.Header.Set("Surreal-DB", conn.DB)

	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "surreal error: %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var stmts []struct {
		Status string `json:"status"`
		Result any    `json:"result"`
	}
	if json.Unmarshal(body, &stmts) == nil {
		for _, s := range stmts {
			if s.Status == "ERR" {
				fmt.Fprintf(os.Stderr, "  surreal ERR: %v\n", s.Result)
			}
		}
	}
}
