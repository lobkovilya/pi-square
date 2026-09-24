package harness

import (
	"bytes"
	"strings"
	"sync"
)

var (
	secretsMu sync.RWMutex
	secrets   []string
)

func SetSecrets(values ...string) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	secrets = nil
	for _, value := range values {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
}

func Redact(value string) string {
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

func RedactBytes(value []byte) []byte {
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	for _, secret := range secrets {
		value = bytes.ReplaceAll(value, []byte(secret), []byte("[REDACTED]"))
	}
	return value
}
