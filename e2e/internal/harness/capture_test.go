package harness

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestShellJoin(t *testing.T) {
	got := ShellJoin("plain", "it's", "", "$(bad)")
	want := `'plain' 'it'"'"'s' '' '$(bad)'`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCaptureFragmentedWrappedAndBinary(t *testing.T) {
	n := "0123456789abcdef01234567"
	raw := []byte{0, 'a', '\n', 255, 1}
	encoded := base64.StdEncoding.EncodeToString(raw)
	screen := "echoed PSQ_ not a frame\nPSQ_" + n + ":137:" + encoded[:4] + "\n" + encoded[4:] + "\n:END" + n + "\n"
	if !completionVisible(screen, n) {
		t.Fatal("completion not found")
	}
	r, ok, err := decodeCapture(screen, n)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Status != 137 || !bytes.Equal(r.Output, raw) {
		t.Fatalf("result %#v", r)
	}
}

func TestCaptureRejectsMalformedAndTruncated(t *testing.T) {
	n := "nonce"
	if _, ok, err := decodeCapture("PSQ_nonce:x:AAAA:ENDnonce", n); ok || err == nil {
		t.Fatalf("wanted malformed status, ok=%v err=%v", ok, err)
	}
	if _, ok, err := decodeCapture("PSQ_nonce:0:AAAA", n); ok || err != nil {
		t.Fatalf("truncated frame should remain incomplete")
	}
	if _, ok, err := decodeCapture("PSQ_nonce:0:%%%:ENDnonce", n); ok || err == nil {
		t.Fatalf("wanted invalid base64")
	}
}

func TestCaptureTornStatusRetriesInsteadOfFailing(t *testing.T) {
	n := "nonce"
	for _, screen := range []string{"PSQ_nonce:", "PSQ_nonce:\nAAAA\n:ENDnonce\n", "PSQ_nonce:13\nAAAA\n:ENDnonce\n"} {
		if _, ok, err := decodeCapture(screen, n); ok || err != nil {
			t.Fatalf("%q: partially painted status must be retried, ok=%v err=%v", screen, ok, err)
		}
	}
	for _, screen := range []string{"PSQ_nonce:1x\nAAAA\n:ENDnonce\n", "PSQ_nonce:1234:AAAA:ENDnonce", "PSQ_nonce:999:AAAA:ENDnonce"} {
		if _, ok, err := decodeCapture(screen, n); ok || err == nil {
			t.Fatalf("%q: status on the header line that can never complete must fail", screen)
		}
	}
	if !headerVisible("x PSQ_nonce:", n) || headerVisible("PSQ_other:", n) {
		t.Fatal("headerVisible must key on the exact nonce")
	}
}

func TestNonceIsolation(t *testing.T) {
	old := "PSQ_old:0::ENDold\n"
	if _, ok, err := decodeCapture(old, "new"); ok || err != nil {
		t.Fatal("old nonce satisfied new capture")
	}
}

func TestRedaction(t *testing.T) {
	SetSecrets("very-secret")
	defer SetSecrets()
	if got := Redact("x very-secret y"); strings.Contains(got, "very-secret") || !strings.Contains(got, "[REDACTED]") {
		t.Fatal(got)
	}
}

func TestTerminalRedrawAndWrap(t *testing.T) {
	term := newTerminal()
	defer term.Close()
	_, _ = term.Write([]byte("old\rnew\x1b[K\r\n" + strings.Repeat("x", 300)))
	s := term.Screen()
	if strings.Contains(s, "old") || !strings.Contains(s, "new") || !strings.Contains(s, strings.Repeat("x", 240)) {
		t.Fatalf("screen reconstruction failed: %q", s)
	}
}
