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

func decodeCapture(screen, nonce string) (ProcessResult, bool, error) {
	start := "PSQ_" + nonce + ":"
	end := ":END" + nonce
	i := strings.Index(screen, start)
	if i < 0 {
		return ProcessResult{}, false, nil
	}
	rest := screen[i+len(start):]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return ProcessResult{}, false, fmt.Errorf("malformed capture: missing status separator")
	}
	status, err := strconv.Atoi(strings.TrimSpace(rest[:colon]))
	if err != nil || status < 0 || status > 255 {
		return ProcessResult{}, false, fmt.Errorf("malformed capture status %q", rest[:colon])
	}
	payload := rest[colon+1:]
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
