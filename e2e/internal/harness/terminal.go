package harness

import (
	"io"
	"strings"
	"sync"

	vt "github.com/charmbracelet/x/vt"
)

// terminal serialises emulator access. Read stays outside the lock because it
// parks on the emulator's reply pipe; Close ends that pipe directly instead of
// racing the emulator's unsynchronised closed flag against a parked Read.
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

func (t *terminal) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if pw, ok := t.vt.InputPipe().(*io.PipeWriter); ok {
		return pw.CloseWithError(io.EOF)
	}
	return t.vt.Close()
}

// Screen joins scrollback and the visible screen with physical newlines, which
// is safe because capture base64 permits whitespace and markers fit in a row.
func (t *terminal) Screen() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var b strings.Builder
	if sb := t.vt.Scrollback(); sb != nil {
		for _, line := range sb.Lines() {
			b.WriteByte('\n')
			b.WriteString(line.String())
		}
	}
	b.WriteByte('\n')
	b.WriteString(t.vt.String())
	return strings.TrimRight(b.String(), "\n ")
}
