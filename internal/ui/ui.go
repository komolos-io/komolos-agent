// Package ui renders komolos-agent's terminal output.
//
// The layout is deliberately sparse: a banner, a short header, ruled sections,
// and one aligned row per step with a status mark. Long-running steps show a
// single live line (spinner, bytes, rate) that is replaced by its final row.
// Detail that is not needed on screen goes to the report file instead.
//
// Colour and live lines are used only on an interactive terminal. Pipes, CI
// logs, NO_COLOR and TERM=dumb get the same layout as plain text.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	esc    = "\x1b["
	reset  = esc + "0m"
	bold   = esc + "1m"
	dim    = esc + "2m"
	teal   = esc + "38;5;37m"
	aqua   = esc + "38;5;44m"
	green  = esc + "38;5;42m"
	amber  = esc + "38;5;214m"
	red    = esc + "38;5;203m"
	clearL = "\r" + esc + "2K"
)

var gradient = []string{esc + "38;5;23m", esc + "38;5;30m", esc + "38;5;37m", esc + "38;5;44m", esc + "38;5;51m"}

const banner = `    __ ______  __  _______  __    ____  _____
   / //_/ __ \/  |/  / __ \/ /   / __ \/ ___/
  / ,< / / / / /|_/ / / / / /   / / / /\__ \
 / /| / /_/ / /  / / /_/ / /___/ /_/ /___/ /
/_/ |_\____/_/  /_/\____/_____/\____//____/`

// Width is the width of section rules and the header line.
const Width = 50

// labelWidth aligns the value column of rows and key-value lines.
const labelWidth = 20

type UI struct {
	w     io.Writer
	color bool
	live  bool
	mu    sync.Mutex
}

// New styles output only if w is an interactive terminal and the user has not
// opted out with NO_COLOR or TERM=dumb.
func New(w io.Writer) *UI {
	u := &UI{w: w}
	if f, ok := w.(*os.File); ok && isTerminal(f) && os.Getenv("TERM") != "dumb" {
		u.live = true
		u.color = os.Getenv("NO_COLOR") == "" && enableVT(f)
	}
	return u
}

// Plain never styles: for tests and redirected output.
func Plain(w io.Writer) *UI { return &UI{w: w} }

func (u *UI) paint(code, s string) string {
	if !u.color || s == "" {
		return s
	}
	return code + s + reset
}

func (u *UI) println(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintln(u.w, s)
}

func pad(s string, n int) string {
	if c := utf8.RuneCountInString(s); c < n {
		return s + strings.Repeat(" ", n-c)
	}
	return s + " "
}

// Banner prints the logo, then the title on the left and the version on the
// right. The logo is shown only on a terminal, and not when KOMOLOS_NO_BANNER
// is set; the title line always is.
func (u *UI) Banner(title, version string) {
	if u.live && os.Getenv("KOMOLOS_NO_BANNER") == "" {
		u.println("")
		for i, line := range strings.Split(banner, "\n") {
			u.println("  " + u.paint(gradient[i%len(gradient)]+bold, line))
		}
	}
	gap := Width - utf8.RuneCountInString(title) - utf8.RuneCountInString(version)
	if gap < 2 {
		gap = 2
	}
	u.println("")
	u.println("  " + u.paint(bold, title) + strings.Repeat(" ", gap) + u.paint(dim, version))
	u.println("")
}

// KV prints an aligned "label  value" header line with an optional dim note.
func (u *UI) KV(label, value, note string) {
	line := "  " + u.paint(dim, pad(label, 11)) + u.paint(aqua, value)
	if note != "" {
		line += "  " + u.paint(dim, note)
	}
	u.println(line)
}

// Section prints a blank line and a ruled title: "── Plan ───────".
func (u *UI) Section(title string) {
	rule := Width - utf8.RuneCountInString(title) - 4
	if rule < 3 {
		rule = 3
	}
	u.println("")
	u.println("  " + u.paint(dim, "──") + " " + u.paint(teal+bold, title) + " " + u.paint(dim, strings.Repeat("─", rule)))
	u.println("")
}

// Row is one step's result: a mark, a label and a value.
func (u *UI) Row(mark Mark, label, value string) {
	u.println("  " + u.mark(mark) + "  " + pad(label, labelWidth) + u.valueColor(mark, value))
}

// Line prints an indented free line in the value column (for a second line of
// detail under a row or a key).
func (u *UI) Line(label, value string) {
	u.println("     " + u.paint(dim, pad(label, labelWidth)) + value)
}

// Note prints a short indented message with a mark and no label.
func (u *UI) Note(mark Mark, text string) {
	u.println("  " + u.mark(mark) + "  " + u.valueColor(mark, text))
}

// Text prints an indented dim sentence.
func (u *UI) Text(s string) { u.println("  " + u.paint(dim, s)) }

// Blank prints an empty line.
func (u *UI) Blank() { u.println("") }

// Prompt writes a question without a newline.
func (u *UI) Prompt(q string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprint(u.w, "\n  "+u.paint(bold, q)+"  "+u.paint(dim, "type yes ›")+" ")
}

type Mark int

const (
	OK Mark = iota
	Warn
	Fail
	Item // neutral bullet
)

func (u *UI) mark(m Mark) string {
	switch m {
	case OK:
		return u.paint(green+bold, "✓")
	case Warn:
		return u.paint(amber+bold, "!")
	case Fail:
		return u.paint(red+bold, "✗")
	}
	return u.paint(dim, "·")
}

func (u *UI) valueColor(m Mark, s string) string {
	switch m {
	case Warn:
		return u.paint(amber, s)
	case Fail:
		return u.paint(red, s)
	case Item:
		return s
	}
	return u.paint(dim, s)
}

// Live is a single in-place status line for a step that takes a while. Off a
// terminal it prints nothing until Done.
type Live struct {
	u      *UI
	label  string
	mu     sync.Mutex
	status string
	bytes  int64
	count  bool
	start  time.Time
	quit   chan struct{}
	done   chan struct{}
	once   sync.Once
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Start begins a live line with a label and a status text.
func (u *UI) Start(label, status string) *Live {
	l := &Live{u: u, label: label, status: status, start: time.Now(), quit: make(chan struct{}), done: make(chan struct{})}
	go l.loop()
	return l
}

// Set replaces the status text.
func (l *Live) Set(status string) {
	l.mu.Lock()
	l.status = status
	l.mu.Unlock()
}

// Phase switches the live line back to a status text after a byte count.
func (l *Live) Phase(status string) {
	l.mu.Lock()
	l.status, l.count = status, false
	l.mu.Unlock()
}

// Write counts bytes; the live line then shows the total and the rate.
func (l *Live) Write(b []byte) (int, error) {
	l.mu.Lock()
	l.bytes += int64(len(b))
	l.count = true
	l.mu.Unlock()
	return len(b), nil
}

// Bytes is the number of bytes written so far.
func (l *Live) Bytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bytes
}

func (l *Live) loop() {
	defer close(l.done)
	if !l.u.live {
		<-l.quit
		return
	}
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-l.quit:
			l.u.mu.Lock()
			fmt.Fprint(l.u.w, clearL)
			l.u.mu.Unlock()
			return
		case <-t.C:
			l.mu.Lock()
			status := l.status
			if l.count {
				rate := float64(l.bytes) / time.Since(l.start).Seconds()
				status = Bytes(l.bytes) + "  ·  " + Bytes(int64(rate)) + "/s"
			}
			l.mu.Unlock()
			l.u.mu.Lock()
			fmt.Fprint(l.u.w, clearL+"  "+l.u.paint(aqua, spinner[i%len(spinner)])+"  "+pad(l.label, labelWidth)+l.u.paint(dim, status))
			l.u.mu.Unlock()
		}
	}
}

func (l *Live) stop() {
	l.once.Do(func() { close(l.quit); <-l.done })
}

// Done replaces the live line with its final row.
func (l *Live) Done(mark Mark, value string) {
	l.stop()
	l.u.Row(mark, l.label, value)
}

// Clear removes the live line without printing a row.
func (l *Live) Clear() { l.stop() }

// Bytes formats a byte count for people: 1.2 MB.
func Bytes(n int64) string {
	const k = 1000
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	}
	return fmt.Sprintf("%d B", n)
}

// Duration formats an elapsed time compactly: 850ms, 12s, 3m 4s.
func Duration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// Muted and Accent colour a fragment of text for use inside other lines.
func (u *UI) Muted(s string) string  { return u.paint(dim, s) }
func (u *UI) Accent(s string) string { return u.paint(aqua, s) }
