package media

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cmdBatch handles `qai media batch ...`. Three input sources, all
// composed through the same per-file pipeline (upload → session →
// auto-walk → write markdown):
//
//   qai media batch <file>...                 — explicit list
//   qai media batch --folder <dir>            — every video in dir
//   qai media batch --csv <path>              — rows of {file[, prompt[, output]]}
//
// Plus the same chat options the single-file path takes: --prompt /
// -p, --auto (default ON for batch — the whole point is hands-off
// narration), --max-turns, --model/-m, --output-dir for where to write
// markdowns (default ./qai-media/), --parallel N for compression+upload
// workers, --continue-on-error, --skip-existing.
//
// One markdown file per source video. Stem of the source filename
// (with .md extension) lands in --output-dir.
func cmdBatch(args []string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Print(helpBatch)
			return
		}
	}

	defMax := int32(defaultMediaMaxTokens)
	bopts := batchOpts{
		maxTurns:        defaultMaxTurns,
		outputDir:       "qai-media",
		parallel:        2,
		// Single-turn by default. Auto-walk forces a structured-output
		// schema ({total_chunks, outline, content}) on turn 1 — useful
		// when the cap forces chunking, harmful when the video fits
		// in one reply (the schema constrains chunk-1's shape and the
		// model produces tighter, less-faithful prose). With max-tokens
		// at 65536 every typical course video fits single-turn. Pass
		// --auto explicitly for very long media (>1h) where the schema
		// pays for itself.
		auto:            false,
		continueOnError: true,
		maxTokens:       &defMax,
	}

	args, bopts.model = stripModelFlag(args)
	bopts.model = resolveModel(bopts.model)

	var explicitSystem, templateName, templateShort string
	args, explicitSystem, _ = stripFlag(args, "--system")
	args, templateName, _ = stripFlag(args, "--template")
	args, templateShort, _ = stripFlag(args, "-t")
	if templateName == "" {
		templateName = templateShort
	}
	bopts.system = resolveSystemInstruction(explicitSystem, templateName)

	args, bopts.mimeOverride, _ = stripFlag(args, "--mime")
	args, bopts.noCompress = stripBoolFlag(args, "--no-compress")

	var folder, csvPath, prompt, promptShort string
	var maxTurnsStr, parallelStr, maxTokensStr, ttlStr string
	args, folder, _ = stripFlag(args, "--folder")
	args, csvPath, _ = stripFlag(args, "--csv")
	args, prompt, _ = stripFlag(args, "--prompt")
	args, promptShort, _ = stripFlag(args, "-p")
	if prompt == "" {
		prompt = promptShort
	}
	args, bopts.outputDir, _ = stripFlag(args, "--output-dir")
	if bopts.outputDir == "" {
		bopts.outputDir = "qai-media"
	}
	args, parallelStr, _ = stripFlag(args, "--parallel")
	args, maxTurnsStr, _ = stripFlag(args, "--max-turns")
	args, maxTokensStr, _ = stripFlag(args, "--max-tokens")
	args, ttlStr, _ = stripFlag(args, "--ttl")

	var noAuto, skipExisting, failFast bool
	args, noAuto = stripBoolFlag(args, "--no-auto")
	args, skipExisting = stripBoolFlag(args, "--skip-existing")
	args, failFast = stripBoolFlag(args, "--fail-fast")
	if noAuto {
		bopts.auto = false
	}
	bopts.skipExisting = skipExisting
	if failFast {
		bopts.continueOnError = false
	}

	if parallelStr != "" {
		n, err := strconv.Atoi(parallelStr)
		if err != nil || n < 1 {
			diefatal("--parallel: must be a positive integer")
		}
		bopts.parallel = n
	}
	if maxTurnsStr != "" {
		n, err := strconv.Atoi(maxTurnsStr)
		if err != nil || n < 1 {
			diefatal("--max-turns: must be a positive integer")
		}
		bopts.maxTurns = n
	}
	if maxTokensStr != "" {
		n, err := strconv.Atoi(maxTokensStr)
		if err != nil {
			diefatal("--max-tokens: %v", err)
		}
		v := int32(n)
		bopts.maxTokens = &v
	}
	if ttlStr != "" {
		n, err := strconv.Atoi(ttlStr)
		if err != nil {
			diefatal("--ttl: %v", err)
		}
		bopts.ttl = n
	}

	// Collect job list. Each job is one (file, prompt, output) tuple.
	jobs, err := gatherBatchJobs(folder, csvPath, args, prompt)
	if err != nil {
		diefatal("%v", err)
	}
	if len(jobs) == 0 {
		diefatal("no input files — pass --folder <dir>, --csv <path>, or one or more file paths")
	}

	if err := os.MkdirAll(bopts.outputDir, 0o755); err != nil {
		diefatal("mkdir %s: %v", bopts.outputDir, err)
	}

	runBatch(jobs, bopts)
}

type batchOpts struct {
	model           string
	system          string
	mimeOverride    string
	noCompress      bool
	ttl             int
	maxTokens       *int32
	outputDir       string
	parallel        int
	auto            bool
	maxTurns        int
	skipExisting    bool
	continueOnError bool
}

type batchJob struct {
	file   string
	prompt string
	output string // resolved path (--output-dir + stem.md)
}

// gatherBatchJobs assembles the (file, prompt, output) list from any
// combination of --folder, --csv, and positional file paths. CSV
// columns (header row required when present): file[,prompt[,output]].
// Folder mode walks one level deep and picks up known video/audio
// extensions. Positional args are taken as bare file paths.
//
// When the user passes --prompt "...", it becomes the default for
// rows that don't supply their own.
func gatherBatchJobs(folder, csvPath string, positional []string, defaultPrompt string) ([]batchJob, error) {
	if defaultPrompt == "" {
		defaultPrompt = defaultMediaPrompt
	}
	var jobs []batchJob

	if csvPath != "" {
		f, err := os.Open(csvPath)
		if err != nil {
			return nil, fmt.Errorf("open csv: %w", err)
		}
		defer f.Close()
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1 // tolerate variable column counts
		rows, err := r.ReadAll()
		if err != nil {
			return nil, fmt.Errorf("parse csv: %w", err)
		}
		// Detect optional header row by looking for a "file" column name.
		start := 0
		if len(rows) > 0 && strings.EqualFold(strings.TrimSpace(rows[0][0]), "file") {
			start = 1
		}
		for i, row := range rows[start:] {
			if len(row) == 0 || strings.TrimSpace(row[0]) == "" {
				continue
			}
			file := strings.TrimSpace(row[0])
			prompt := defaultPrompt
			if len(row) > 1 && strings.TrimSpace(row[1]) != "" {
				prompt = strings.TrimSpace(row[1])
			}
			output := ""
			if len(row) > 2 && strings.TrimSpace(row[2]) != "" {
				output = strings.TrimSpace(row[2])
			}
			if !filepath.IsAbs(file) {
				// Resolve relative paths against the CSV's directory
				// so users don't have to absolutize their list.
				file = filepath.Join(filepath.Dir(csvPath), file)
			}
			if _, err := os.Stat(file); err != nil {
				fmt.Fprintf(os.Stderr, "qai media batch: csv row %d skipped — file not found: %s\n", i+start+1, file)
				continue
			}
			jobs = append(jobs, batchJob{file: file, prompt: prompt, output: output})
		}
	}

	if folder != "" {
		entries, err := os.ReadDir(folder)
		if err != nil {
			return nil, fmt.Errorf("read folder: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			path := filepath.Join(folder, e.Name())
			if !looksLikeMedia(path) {
				continue
			}
			jobs = append(jobs, batchJob{file: path, prompt: defaultPrompt})
		}
	}

	for _, p := range positional {
		if !isExistingFile(p) {
			fmt.Fprintf(os.Stderr, "qai media batch: skipping non-file arg: %s\n", p)
			continue
		}
		jobs = append(jobs, batchJob{file: p, prompt: defaultPrompt})
	}

	return jobs, nil
}

// looksLikeMedia is a fast extension check used to filter folder walks
// to things our MIME table knows about. Same table as upload.go; we
// avoid a full magic-bytes probe here (folder mode runs over many
// files, no need to open each one).
func looksLikeMedia(path string) bool {
	return guessMime(path) != ""
}

// runBatch executes the assembled job list. Workers pull from a job
// channel. We bound parallelism at bopts.parallel — beyond ~2 the
// limit becomes the broker's per-tenant chat rate, not the local
// upload pipe.
func runBatch(jobs []batchJob, bopts batchOpts) {
	fmt.Fprintf(os.Stderr,
		"qai media batch: %d job(s), output dir %s, parallel %d, auto=%v, max-turns %d\n",
		len(jobs), bopts.outputDir, bopts.parallel, bopts.auto, bopts.maxTurns)

	type result struct {
		idx   int
		job   batchJob
		err   error
		dur   time.Duration
		chunk int
	}
	jobCh := make(chan int, len(jobs))
	resCh := make(chan result, len(jobs))

	var wg sync.WaitGroup
	for w := 0; w < bopts.parallel; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for idx := range jobCh {
				job := jobs[idx]
				start := time.Now()
				outPath := resolveBatchOutputPath(job, bopts.outputDir)
				if bopts.skipExisting {
					if _, err := os.Stat(outPath); err == nil {
						fmt.Fprintf(os.Stderr,
							"  [%d/%d w%d] skip (exists): %s\n",
							idx+1, len(jobs), workerID, filepath.Base(outPath))
						resCh <- result{idx: idx, job: job}
						continue
					}
				}
				fmt.Fprintf(os.Stderr,
					"  [%d/%d w%d] %s — starting\n",
					idx+1, len(jobs), workerID, filepath.Base(job.file))
				chunks, err := runOneBatchJob(job, bopts, outPath)
				resCh <- result{idx: idx, job: job, err: err, dur: time.Since(start), chunk: chunks}
			}
		}(w)
	}
	for i := range jobs {
		jobCh <- i
	}
	close(jobCh)
	wg.Wait()
	close(resCh)

	var ok, failed int
	for r := range resCh {
		if r.err != nil {
			failed++
			fmt.Fprintf(os.Stderr,
				"  [%d/%d] %s — FAILED in %s: %v\n",
				r.idx+1, len(jobs), filepath.Base(r.job.file), roundShortDur(r.dur), r.err)
			if !bopts.continueOnError {
				diefatal("batch aborted at first failure (use --no-fail-fast / default to keep going)")
			}
			continue
		}
		ok++
		fmt.Fprintf(os.Stderr,
			"  [%d/%d] %s — done (%d chunks) in %s\n",
			r.idx+1, len(jobs), filepath.Base(r.job.file), r.chunk, roundShortDur(r.dur))
	}

	fmt.Fprintf(os.Stderr,
		"qai media batch: complete — %d ok, %d failed, %d total\n",
		ok, failed, len(jobs))
	if failed > 0 {
		os.Exit(2)
	}
}

// runOneBatchJob drives one video end-to-end: upload, create session,
// auto-walk (or single turn), write markdown. Returns chunk count for
// the per-job summary line.
func runOneBatchJob(job batchJob, bopts batchOpts, outPath string) (int, error) {
	upload, cleanup, err := uploadFile(job.file, bopts.mimeOverride, bopts.noCompress)
	defer cleanup()
	if err != nil {
		return 0, fmt.Errorf("upload: %w", err)
	}

	createReq := sessionCreateRequest{
		FileURI:           upload.FileURI,
		MimeType:          upload.MimeType,
		Model:             bopts.model,
		SystemInstruction: bopts.system,
		DisplayName:       upload.Name,
		CacheTTLSeconds:   bopts.ttl,
	}
	session, err := CreateSession(createReq)
	if err != nil {
		return 0, fmt.Errorf("create session: %w", err)
	}

	sourceLabel := filepath.Base(job.file)

	if !bopts.auto {
		resp, err := SessionChat(session.ID, job.prompt, bopts.maxTokens, nil)
		if err != nil {
			return 0, fmt.Errorf("chat: %w", err)
		}
		md := buildSingleTurnMarkdown(sourceLabel, session.ID, job.prompt, resp.Answer)
		if err := writeOutputBatch(outPath, md); err != nil {
			return 0, err
		}
		return 1, nil
	}

	// --auto: plan turn + N continuation turns. Reuses the same
	// machinery as the single-file path but writes to outPath
	// directly (no stdout streaming — batch is hands-off by design,
	// the user wants the files at the end, not chunk-by-chunk
	// terminal output).
	planResp, err := SessionChatWithSchema(session.ID, job.prompt, bopts.maxTokens, autoPlanSchema)
	if err != nil {
		return 0, fmt.Errorf("plan: %w", err)
	}
	var plan struct {
		TotalChunks int      `json:"total_chunks_estimated"`
		Outline     []string `json:"outline"`
		Content     string   `json:"content"`
	}
	if err := json.Unmarshal([]byte(planResp.Answer), &plan); err != nil {
		return 0, fmt.Errorf("plan parse: %w", err)
	}
	executable := plan.TotalChunks
	if executable > bopts.maxTurns {
		executable = bopts.maxTurns
	}
	chunks := []string{plan.Content}
	for i := 2; i <= executable; i++ {
		contPrompt := fmt.Sprintf(
			"Continue with chunk %d of %d as plain text. Pick up where chunk %d left off — do not summarise, do not restate the plan, do not re-greet the reader. Just deliver the next chunk's content.",
			i, plan.TotalChunks, i-1)
		resp, err := SessionChat(session.ID, contPrompt, bopts.maxTokens, nil)
		if err != nil {
			// Partial result is better than zero — write what we have.
			break
		}
		chunks = append(chunks, resp.Answer)
	}
	md := buildAutoWalkMarkdown(sourceLabel, session.ID, bopts.model, job.prompt, plan.TotalChunks, plan.Outline, chunks)
	if err := writeOutputBatch(outPath, md); err != nil {
		return len(chunks), err
	}
	return len(chunks), nil
}

// resolveBatchOutputPath returns the final markdown destination for a
// job. Explicit per-row output (from CSV column 3) wins; otherwise it's
// <output-dir>/<source-stem>.md.
func resolveBatchOutputPath(job batchJob, dir string) string {
	if job.output != "" {
		if filepath.IsAbs(job.output) {
			return job.output
		}
		return filepath.Join(dir, job.output)
	}
	stem := filenameStem(filepath.Base(job.file))
	return filepath.Join(dir, stem+".md")
}

func writeOutputBatch(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// writeBatchManifest writes a CSV summary of the run to
// <output-dir>/manifest.csv — kept for future use (not wired into the
// runBatch summary yet to keep the first cut tight).
func writeBatchManifest(path string, jobs []batchJob) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	fmt.Fprintln(w, "file,prompt,output")
	for _, j := range jobs {
		fmt.Fprintf(w, "%q,%q,%q\n", j.file, j.prompt, j.output)
	}
}

const helpBatch = `qai media batch — process many videos, write one markdown each

Inputs (use any combination — they merge into one job list):
  --folder <dir>         every video file in <dir> (one level deep)
  --csv <path>           CSV of rows: file[,prompt[,output]]
                         Header row optional but recognised.
  <file>...              positional file paths

Flags:
  --prompt, -p "..."     default prompt applied to rows that don't
                         specify their own. Default: action-checklist
                         extraction (see defaultMediaPrompt).
  --output-dir <dir>     where to write the .md files (default:
                         ./qai-media). Per-row output column (CSV
                         col 3) overrides.

  --model, -m <id>       passed through to every job (default:
                         gemini-3.1-flash-lite)
  --template, -t <name>  named system-instruction profile
                         (default: media-narrate)
  --system "..."         explicit system prompt
  --ttl <seconds>        cache TTL per session (default 3600)
  --max-tokens <n>       per-turn cap (passed to every turn)

  --auto                 opt-in: use structured-output planning + auto-
                         walk to cover the whole video in N chunks. Use
                         this for very long media (>1h) where a single
                         reply would truncate.
  --no-auto              default: one reply per video. Higher fidelity
                         on typical lecture-length content because the
                         model uses its natural narrative shape instead
                         of being forced into a {chunks,outline,content}
                         schema on turn 1.
  --max-turns <n>        cap auto chunks per video (default 8)

  --parallel <n>         concurrent workers (default 2)
  --skip-existing        skip jobs whose output .md already exists
  --fail-fast            stop on first failure (default: continue)

  --mime <type>          override auto-detected MIME for all inputs
  --no-compress          skip auto-compress for video > 20 MB

Examples:
  # Whole course folder, auto-walk, two workers
  qai media batch --folder ~/Videos/dropshipping-accelerator/ \
                  --output-dir ~/Notes/dropship/ --parallel 2

  # CSV of explicit prompts per video
  qai media batch --csv research/videos.csv --output-dir ./narrations/

  # Re-run, skipping things already done
  qai media batch --folder ~/Videos/course --skip-existing

CSV format (header optional):
  file,prompt,output
  intro.mp4,"summarise the key takeaways",intro-summary.md
  ../module-2/lesson-3.mp4,"focus on the technique at 5:30",lesson3.md
`
