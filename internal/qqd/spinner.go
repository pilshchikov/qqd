package qqd

import (
	"fmt"
	"io"
	"os"
	"time"
)

// spinner shows a braille animation on TTY writers while work is in progress.
type spinner struct {
	w       io.Writer
	msg     string
	done    chan struct{}
	stopped chan struct{}
	isTTY   bool
}

// startSpinner begins an animation with the given message.
// On non-TTY writers (pipes, io.Discard) it is a no-op.
func startSpinner(w io.Writer, msg string) *spinner {
	s := &spinner{
		w:       w,
		msg:     msg,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	if f, ok := w.(*os.File); ok && isTerminal(f.Fd()) {
		s.isTTY = true
	}
	go s.run()
	return s
}

func (s *spinner) stop() {
	close(s.done)
	<-s.stopped
}

func (s *spinner) run() {
	defer close(s.stopped)
	if !s.isTTY {
		<-s.done
		return
	}
	frames := [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	i := 0
	for {
		select {
		case <-s.done:
			fmt.Fprintf(s.w, "\r\033[K")
			return
		case <-ticker.C:
			fmt.Fprintf(s.w, "\r%s %s", frames[i%len(frames)], s.msg)
			i++
		}
	}
}
