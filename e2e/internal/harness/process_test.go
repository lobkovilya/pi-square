//go:build linux

package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunProcessCancellationKillsDescendant(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	r := RunProcess(ctx, "bash", []string{"-c", fmt.Sprintf("sleep 30 & echo $! > %s; wait", ShellJoin(pidfile))}, "", CleanEnvironment(""))
	if !r.TimedOut {
		t.Fatalf("expected timeout: %s", FormatResult(r))
	}
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// A killed orphan can briefly remain a zombie; /proc stat must not show a running child.
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil && !strings.Contains(string(stat), ") Z ") {
		t.Fatalf("descendant %d survived cancellation", pid)
	}
}
