package media

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Bar is a tiny progress indicator used by compress + upload. Renders
// in-place via \r on TTY, falls back to a single one-shot line on a
// non-TTY (CI / log pipe) so output stays readable in both contexts.
//
// Lifecycle:
//
//	bar := NewBar("compressing", totalBytes)
//	defer bar.Finish()
//	// ... work loop ...
//	bar.Update(currentBytes)
//
// Suffix formatter lets each caller render its own units (kbits/s,
// MB/s, frame counts, ETA). Bar runs the formatter on every render.
type Bar struct {
	out      io.Writer
	prefix   string
	total    int64
	current  int64
	start    time.Time
	tty      bool
	width    int
	suffix   func(*Bar) string
	finished bool
}

// NewBar prints the opening line ("compressing ...") and starts the
// in-place bar on the next render. Total is the units of work; pass 0
// when the total isn't known upfront (renders an indeterminate spinner
// instead of a filled bar).
func NewBar(prefix string, total int64, suffix func(*Bar) string) *Bar {
	b := &Bar{
		out:    os.Stderr,
		prefix: prefix,
		total:  total,
		start:  time.Now(),
		tty:    isStderrTTY(),
		width:  30,
		suffix: suffix,
	}
	fmt.Fprintf(b.out, "qai media: %s...\n", prefix)
	if !b.tty {
		// Non-TTY: skip the in-place bar, the Finish line is enough.
		return b
	}
	b.render()
	return b
}

// Update records new progress and re-renders. Safe to call frequently
// (rate-limits itself to ~10Hz).
func (b *Bar) Update(current int64) {
	b.current = current
	b.render()
}

// Finish caps the bar at 100%, prints a newline, and emits a one-line
// summary. Safe to call once.
func (b *Bar) Finish() {
	if b.finished {
		return
	}
	b.finished = true
	if b.total > 0 {
		b.current = b.total
	}
	if b.tty {
		b.render()
		fmt.Fprintln(b.out)
	}
	// Final summary line — what the user actually wants in their
	// scrollback after the bar disappears.
	if b.suffix != nil {
		fmt.Fprintf(b.out, "qai media: %s done — %s\n", b.prefix, b.suffix(b))
	} else {
		fmt.Fprintf(b.out, "qai media: %s done in %s\n", b.prefix, roundShortDur(time.Since(b.start)))
	}
}

// Elapsed returns the time since the bar was created. Suffix formatters
// use this for ETA + throughput math without re-reading time.Now.
func (b *Bar) Elapsed() time.Duration { return time.Since(b.start) }

// Current / Total accessors for suffix formatters.
func (b *Bar) Current() int64 { return b.current }
func (b *Bar) Total() int64   { return b.total }

// Pct returns 0–100. 0 when total is unset.
func (b *Bar) Pct() int {
	if b.total <= 0 {
		return 0
	}
	pct := int(b.current * 100 / b.total)
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	return pct
}

// lastRender throttles to ~10Hz — render() returns early if the last
// frame was less than 100ms ago and we're not at 100%.
var lastRender time.Time

func (b *Bar) render() {
	if !b.tty {
		return
	}
	now := time.Now()
	if !b.finished && b.Pct() < 100 && now.Sub(lastRender) < 100*time.Millisecond {
		return
	}
	lastRender = now

	pct := b.Pct()
	var bar string
	if b.total > 0 {
		filled := pct * b.width / 100
		if filled > b.width {
			filled = b.width
		}
		// Use a single ">" head on the leading edge so the bar looks
		// like it's moving — matches the ffmpeg / wget aesthetic the
		// user asked for.
		barRunes := strings.Repeat("=", filled)
		if filled > 0 && filled < b.width {
			barRunes = barRunes[:len(barRunes)-1] + ">"
		}
		bar = barRunes + strings.Repeat(" ", b.width-filled)
	} else {
		// Indeterminate: bouncing block based on elapsed time.
		pos := int(now.UnixNano()/(int64(120*time.Millisecond))) % (b.width * 2)
		if pos >= b.width {
			pos = b.width*2 - pos - 1
		}
		runes := []byte(strings.Repeat(" ", b.width))
		if pos >= 0 && pos < b.width {
			runes[pos] = '='
		}
		bar = string(runes)
	}

	suffix := ""
	if b.suffix != nil {
		suffix = b.suffix(b)
	}
	if b.total > 0 {
		fmt.Fprintf(b.out, "\r  [%s] %3d%% • %s", bar, pct, suffix)
	} else {
		fmt.Fprintf(b.out, "\r  [%s] • %s", bar, suffix)
	}
}

// isStderrTTY checks whether stderr is a terminal — bars only render
// in-place when we have one. Falls back to false on any stat error so
// non-TTY output is the safer default.
func isStderrTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// roundShortDur prints e.g. "1m25s" or "12s" — used for ETA + elapsed
// in bar suffixes where wide units would push the line over the screen.
func roundShortDur(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
