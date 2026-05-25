package qqd

import (
	"fmt"
	"io"
	"os"
)

var colorEnabled bool

// InitColor enables ANSI colors if w is a terminal and NO_COLOR is not set.
func InitColor(w io.Writer) {
	if os.Getenv("NO_COLOR") != "" {
		colorEnabled = false
		return
	}
	if f, ok := w.(*os.File); ok {
		colorEnabled = isTerminal(f.Fd())
		return
	}
	colorEnabled = false
}

func ansi(code, s string) string {
	if !colorEnabled {
		return s
	}
	return fmt.Sprintf("\033[%sm%s\033[0m", code, s)
}

func bold(s string) string       { return ansi("1", s) }
func dim(s string) string        { return ansi("90", s) }
func green(s string) string      { return ansi("32", s) }
func yellow(s string) string     { return ansi("33", s) }
func red(s string) string        { return ansi("31", s) }
func cyan(s string) string       { return ansi("36", s) }
func boldCyan(s string) string   { return ansi("1;36", s) }
func boldGreen(s string) string  { return ansi("1;32", s) }
func boldRed(s string) string    { return ansi("1;31", s) }
func boldYellow(s string) string { return ansi("1;33", s) }
