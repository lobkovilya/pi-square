//go:build linux

package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutLockRefusesConcurrentUseAndCanBeReacquired(t *testing.T) {
	target := filepath.Join(t.TempDir(), "checkout")
	first, second := &LiveFixture{}, &LiveFixture{}
	if err := first.acquireLock(target); err != nil {
		t.Fatal(err)
	}
	if err := second.acquireLock(target); err == nil || !strings.Contains(err.Error(), "concurrent live runs") {
		t.Fatalf("expected concurrent-use refusal, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.acquireLock(target); err != nil {
		t.Fatalf("reacquire stale lock file: %v", err)
	}
	defer second.Close()
}

func TestCheckoutValidationRefusesWrongOwnershipMarker(t *testing.T) {
	target := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+markerSuffix, []byte(`{"repository":"somebody/else"}`), 0600); err != nil {
		t.Fatal(err)
	}
	f := &LiveFixture{Repository: "owner/expected"}
	err := f.validateCheckout(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "invalid test-ownership marker") {
		t.Fatalf("expected ownership refusal, got %v", err)
	}
}
