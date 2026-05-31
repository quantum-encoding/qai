package media

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// compressionThreshold is the file size above which we'll auto-compress
// before upload. 20 MB sits below the broker's 100 MiB hard cap with
// room to spare and matches the inflection point where re-encoding
// pays off — a 5 MB file compresses to ~3 MB which isn't worth the
// CPU; a 50 MB file compresses to ~3 MB and saves the user 90 seconds
// of upload time on a 5 Mbps connection.
const compressionThreshold = 20 * 1024 * 1024

// videoCompressArgs is the ffmpeg argument set used to shrink lecture-
// /podcast-shaped video down to something Gemini can ingest fast. The
// values match what gemini's video sampler actually consumes:
//
//   - scale=-2:480     480p tall, width derived to keep aspect ratio
//   - fps=1            Gemini samples 1 frame/sec anyway; sending more
//                      is bandwidth waste
//   - libx264 / crf 32 cheap encode, file size dominated by frame count
//   - aac mono 64kbps  speech-quality audio; stereo on a lecture is
//                      noise
//   - +faststart       moov atom up front so the server's MIME / metadata
//                      probe can return without buffering the whole file
//
// Net: a 150 MB 1080p/60fps lecture comes out at ~5 MB with full
// intelligibility for narration-heavy content.
var videoCompressArgs = []string{
	"-vf", "scale=-2:480,fps=1",
	"-vcodec", "libx264",
	"-crf", "32",
	"-acodec", "aac",
	"-ac", "1",
	"-b:a", "64k",
	"-movflags", "+faststart",
}

// shouldCompress returns true when the file is over the threshold AND
// the MIME type looks like something we know how to re-encode. Audio
// and PDFs pass through unchanged — re-encoding audio loses quality
// for no bandwidth win, and PDFs compress on the broker side.
func shouldCompress(path, mimeType string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.Size() < compressionThreshold {
		return false
	}
	switch mimeType {
	case "video/mp4", "video/webm", "video/quicktime":
		return true
	}
	return false
}

// compressVideo runs the ffmpeg pipeline above with a live progress
// bar (when stderr is a TTY) and returns the path to the compressed
// sidecar. The output lives in ~/.qai/tmp/ so it's easy to clean up;
// caller is responsible for the cleanup (usually via a defer trash()
// in the calling subcommand).
//
// Progress: we probe the source's duration with ffprobe up front, then
// run ffmpeg with -progress pipe:1 (machine-parseable key=value stream)
// AND -v error (silences the default verbose frame-by-frame log). The
// out_time_ms field on each progress record / total duration gives us
// the percentage; the speed + bitrate fields populate the suffix line.
func compressVideo(srcPath, mimeType string) (string, error) {
	_ = mimeType // reserved for per-mime tuning (audio bitrate, etc.)

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg not found on PATH — install ffmpeg (`brew install ffmpeg`) or pass --no-compress to upload the file as-is")
	}

	tmpRoot, err := tmpDir()
	if err != nil {
		return "", err
	}

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", srcPath, err)
	}

	stem := filenameStem(filepath.Base(srcPath))
	outPath := filepath.Join(tmpRoot, stem+".compressed.mp4")

	// Total work for the progress bar is the source duration in
	// milliseconds. ffprobe failure isn't fatal — the bar just renders
	// as indeterminate (bouncing) instead of percentage.
	totalMs := probeDurationMs(srcPath)

	prefix := fmt.Sprintf("compressing %s (%s → ~5M)",
		filepath.Base(srcPath), humanBytes(srcInfo.Size()))

	// Suffix renders the right side of the bar. ffmpeg progress records
	// give us speed + bitrate; we compute ETA from elapsed / pct.
	var lastSpeed, lastBitrate string
	bar := NewBar(prefix, totalMs, func(b *Bar) string {
		parts := []string{}
		if lastBitrate != "" {
			parts = append(parts, lastBitrate)
		}
		if lastSpeed != "" {
			parts = append(parts, lastSpeed)
		}
		if b.Pct() > 0 && b.Pct() < 100 && totalMs > 0 {
			eta := time.Duration(float64(b.Elapsed()) * (100 - float64(b.Pct())) / float64(b.Pct()))
			parts = append(parts, "~"+roundShortDur(eta)+" left")
		}
		return strings.Join(parts, " • ")
	})

	// -v error: silence the default verbose log
	// -progress pipe:1: structured progress records to stdout
	args := []string{"-y", "-v", "error", "-progress", "pipe:1", "-i", srcPath}
	args = append(args, videoCompressArgs...)
	args = append(args, outPath)

	cmd := exec.Command(ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		bar.Finish()
		return "", fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	// Keep stderr — if ffmpeg fails, -v error gives us a real error
	// line; we capture so we can surface it on failure but DON'T
	// stream live (that would interleave with the bar).
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		bar.Finish()
		return "", fmt.Errorf("ffmpeg start: %w", err)
	}

	// Parse the progress stream in this goroutine; ffmpeg emits a
	// block every ~0.5s with several key=value lines ending in
	// "progress=continue" (or "=end" at finish).
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "out_time_ms":
			// out_time_ms is actually MICROSECONDS in ffmpeg despite
			// the name — divide by 1000 for ms. Verified against
			// out_time wall-clock string.
			if us, err := strconv.ParseInt(val, 10, 64); err == nil {
				bar.Update(us / 1000)
			}
		case "speed":
			if val != "N/A" && val != "" {
				lastSpeed = strings.TrimSpace(val) + " speed"
			}
		case "bitrate":
			if val != "N/A" && val != "" {
				lastBitrate = strings.TrimSpace(val)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		bar.Finish()
		stderrTail := strings.TrimSpace(stderrBuf.String())
		if len(stderrTail) > 400 {
			stderrTail = "..." + stderrTail[len(stderrTail)-400:]
		}
		return "", fmt.Errorf("ffmpeg failed: %w%s", err, formatStderr(stderrTail))
	}

	bar.Finish()

	outInfo, err := os.Stat(outPath)
	if err != nil {
		return "", fmt.Errorf("stat compressed output: %w", err)
	}
	if outInfo.Size() == 0 {
		return "", fmt.Errorf("compressed output is empty — ffmpeg produced no data")
	}
	fmt.Fprintf(os.Stderr,
		"qai media: compressed to %s (%.0fx smaller)\n",
		humanBytes(outInfo.Size()),
		float64(srcInfo.Size())/float64(outInfo.Size()))
	return outPath, nil
}

// probeDurationMs returns the source's duration in milliseconds, or 0
// if ffprobe is unavailable or fails. 0 makes the bar render as
// indeterminate — degraded but not broken.
func probeDurationMs(path string) int64 {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0
	}
	out, err := exec.Command(ffprobe,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=nokey=1:noprint_wrappers=1",
		path,
	).Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	if s == "" || s == "N/A" {
		return 0
	}
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(secs * 1000)
}

// formatStderr returns a leading newline + indented stderr tail, or
// empty when stderr was empty — keeps the wrapped error compact when
// ffmpeg failed quietly.
func formatStderr(s string) string {
	if s == "" {
		return ""
	}
	return "\n  ffmpeg stderr: " + s
}

// Make io explicitly used so future enhancements (streaming the bar to
// a custom writer) compile without an unused-import warning.
var _ = io.Discard

// tmpDir returns ~/.qai/tmp/, creating it if needed. Compressed files
// land here; callers trash them after upload completes (or leave them
// for the next run to overwrite — Z_STREAM_END style).
func tmpDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".qai", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

func filenameStem(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)]
}

// humanBytes renders a byte count in the largest "round" unit. Not
// SI-correct (1024 vs 1000) — matches `ls -h` since that's the user's
// reference point.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// humanRate renders a bytes-per-second float in MB/s or KB/s. Used by
// the upload progress bar's suffix formatter.
func humanRate(bytesPerSec float64) string {
	switch {
	case bytesPerSec >= 1<<20:
		return fmt.Sprintf("%.1fMB/s", bytesPerSec/(1<<20))
	case bytesPerSec >= 1<<10:
		return fmt.Sprintf("%.0fKB/s", bytesPerSec/(1<<10))
	}
	return fmt.Sprintf("%.0fB/s", bytesPerSec)
}
