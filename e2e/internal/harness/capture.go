package harness

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const truncationNotice = "Output truncated. Full output:"

var statusField = regexp.MustCompile(`^(\d{1,3}):`)

func nonce() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func captureShell(command, nonce string) string {
	return `f=$(mktemp) || exit; bash -c ` + ShellJoin(command) + ` >"$f" 2>&1; r=$?; ` +
		`printf '\n%s%s:%s:' PSQ_ ` + nonce + ` "$r"; base64 -w0 "$f"; ` +
		`printf '\n%s%s\n' :END ` + nonce + `; rm -f "$f"; exit "$r"`
}

func completionVisible(screen, nonce string) bool {
	re := regexp.MustCompile(`:END` + regexp.QuoteMeta(nonce) + `[ \t]*(?:\n|$)`)
	return re.MatchString(screen)
}

func headerVisible(screen, nonce string) bool {
	return strings.Contains(screen, "PSQ_"+nonce+":")
}

// decodeCapture reports ok=false without an error while the frame is only
// partially painted; a redraw that stopped inside the status digits must be
// polled again, not treated as corrupt.
func decodeCapture(screen, nonce string) (ProcessResult, bool, error) {
	start := "PSQ_" + nonce + ":"
	end := ":END" + nonce
	i := strings.Index(screen, start)
	if i < 0 {
		return ProcessResult{}, false, nil
	}
	rest := screen[i+len(start):]
	head := rest
	if nl := strings.IndexByte(head, '\n'); nl >= 0 {
		head = head[:nl]
	}
	field := statusField.FindStringSubmatch(head)
	if field == nil {
		if len(head) <= 3 && strings.Trim(head, "0123456789") == "" {
			return ProcessResult{}, false, nil
		}
		return ProcessResult{}, false, fmt.Errorf("malformed capture status %q", head)
	}
	status, _ := strconv.Atoi(field[1])
	if status > 255 {
		return ProcessResult{}, false, fmt.Errorf("malformed capture status %q", field[1])
	}
	payload := rest[len(field[0]):]
	j := strings.Index(payload, end)
	if j < 0 {
		return ProcessResult{}, false, nil
	}
	payload = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, payload[:j])
	output, err := base64.StdEncoding.Strict().DecodeString(payload)
	if err != nil {
		return ProcessResult{}, false, fmt.Errorf("invalid base64 capture: %w", err)
	}
	return ProcessResult{Status: status, Output: output, Completed: true}, true, nil
}
