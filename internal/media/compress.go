package media

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// compressVideo runs the ffmpeg pipeline above and returns the path to
// the compressed sidecar. The output lives in ~/.qai/tmp/ so it's easy
// to clean up; caller is responsible for the cleanup (usually via a
// defer trash() in the calling subcommand).
//
// Status writes to stderr — the user sees "Compressing 149M → ~5M..."
// so a 90-second ffmpeg pass doesn't look like a hang.
func compressVideo(srcPath, mimeType string) (string, error) {
	// Resolve ffmpeg explicitly so the error is actionable when it's
	// missing.
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

	// Output filename: keep the original stem, force .mp4 because the
	// broker's allowlist is mp4/webm/quicktime and mp4 is the widest
	// fit for the H.264/AAC payload ffmpeg produces.
	stem := filenameStem(filepath.Base(srcPath))
	outPath := filepath.Join(tmpRoot, stem+".compressed.mp4")

	args := []string{"-y", "-loglevel", "error", "-stats", "-i", srcPath}
	args = append(args, videoCompressArgs...)
	args = append(args, outPath)

	fmt.Fprintf(os.Stderr,
		"qai media: compressing %s (%s → ~5MB) for fast upload — this can take ~30-90s...\n",
		filepath.Base(srcPath), humanBytes(srcInfo.Size()))

	cmd := exec.Command(ffmpeg, args...)
	cmd.Stderr = os.Stderr // surface ffmpeg progress live
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg failed: %w", err)
	}

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
