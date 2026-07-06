// Package ui handles terminal output: colors, per-server line prefixes, and
// step headers, so parallel deploys stay readable.
package ui

import (
	"bytes"
	"fmt"
	"os"
	"sync"
)

var (
	colorEnabled = detectColor()
	palette      = []string{"36", "35", "33", "32", "34", "96", "95", "93"} // cyan, magenta, yellow, green, blue, bright variants
	paletteMu    sync.Mutex
	paletteNext  int
	// stdout is shared by every PrefixWriter; the mutex keeps lines whole.
	outMu sync.Mutex
)

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func paint(code, s string) string {
	if !colorEnabled || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func Bold(s string) string  { return paint("1", s) }
func Dim(s string) string   { return paint("2", s) }
func Red(s string) string   { return paint("31", s) }
func Green(s string) string { return paint("32", s) }
func Yell(s string) string  { return paint("33", s) }

// Printf writes a top-level (unprefixed) line to stdout.
func Printf(format string, a ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Fprintf(os.Stdout, format+"\n", a...)
}

func Errorf(format string, a ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Fprintf(os.Stderr, Red("error: ")+format+"\n", a...)
}

// PrefixWriter is an io.Writer that prepends a colored "[name] " tag to every
// line. Safe for concurrent use by parallel per-server goroutines.
type PrefixWriter struct {
	prefix string
	mu     sync.Mutex
	buf    bytes.Buffer
}

// NewPrefix returns a writer tagging lines with [name], cycling colors so
// different servers are visually distinct.
func NewPrefix(name string) *PrefixWriter {
	paletteMu.Lock()
	code := palette[paletteNext%len(palette)]
	paletteNext++
	paletteMu.Unlock()
	return &PrefixWriter{prefix: paint(code, "["+name+"]") + " "}
}

// NewBarePrefix returns a writer with no tag (single-target commands).
func NewBarePrefix() *PrefixWriter { return &PrefixWriter{} }

func (w *PrefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil { // no complete line yet; keep the partial
			w.buf.WriteString(line)
			break
		}
		outMu.Lock()
		fmt.Fprint(os.Stdout, w.prefix+line)
		outMu.Unlock()
	}
	return len(p), nil
}

// Flush writes any trailing partial line.
func (w *PrefixWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		outMu.Lock()
		fmt.Fprint(os.Stdout, w.prefix+w.buf.String()+"\n")
		outMu.Unlock()
		w.buf.Reset()
	}
}

// Step prints a step header line, e.g. "→ build (docker compose build)".
func (w *PrefixWriter) Step(format string, a ...any) {
	fmt.Fprintf(w, "%s %s\n", paint("36;1", "→"), fmt.Sprintf(format, a...))
}

func (w *PrefixWriter) Okf(format string, a ...any) {
	fmt.Fprintf(w, "%s %s\n", Green("✔"), fmt.Sprintf(format, a...))
}

func (w *PrefixWriter) Failf(format string, a ...any) {
	fmt.Fprintf(w, "%s %s\n", Red("✖"), fmt.Sprintf(format, a...))
}

func (w *PrefixWriter) Notef(format string, a ...any) {
	fmt.Fprintf(w, "%s\n", Dim(fmt.Sprintf(format, a...)))
}
