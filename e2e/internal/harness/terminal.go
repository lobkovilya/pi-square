package harness

import (
	"strings"
	"sync"

	vt "github.com/charmbracelet/x/vt"
)

// terminal is a concurrency-safe adapter around a real VT emulator. Query
// replies are exposed through Read and copied back to the PTY by Session.
type terminal struct {
	mu sync.RWMutex
	vt *vt.Emulator
}

func newTerminal() *terminal {
	emu := vt.NewEmulator(240, 80)
	emu.SetScrollbackSize(10_000)
	return &terminal{vt: emu}
}

func (t *terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.vt.Write(p)
}

func (t *terminal) Read(p []byte) (int, error) { return t.vt.Read(p) }
func (t *terminal) Close() error               { return t.vt.Close() }

func cellText(cell interface{ String() string }) string { return cell.String() }

func (t *terminal) Screen() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var b strings.Builder
	// Physical newlines are safe: capture base64 deliberately permits whitespace,
	// and fixed markers are shorter than the 240-column terminal.
	if sb := t.vt.Scrollback(); sb != nil {
		for _, line := range sb.Lines() {
			b.WriteByte('\n')
			for i := range line {
				b.WriteString(line[i].Content)
			}
		}
	}
	for y := 0; y < t.vt.Height(); y++ {
		b.WriteByte('\n')
		lineStart := b.Len()
		for x := 0; x < t.vt.Width(); x++ {
			if cell := t.vt.CellAt(x, y); cell != nil {
				b.WriteString(cell.Content)
			}
		}
		// Avoid huge trailing blank screens without changing interior spacing.
		line := strings.TrimRight(b.String()[lineStart:], " ")
		if len(line) != b.Len()-lineStart {
			text := b.String()[:lineStart] + line
			b.Reset()
			b.WriteString(text)
		}
	}
	return strings.TrimRight(b.String(), "\n ")
}
