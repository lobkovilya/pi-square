//go:build unix

package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func CleanEnvironment(realToken string) []string {
	blocked := map[string]bool{
		"PI_SQUARE_E2E_GITHUB_TOKEN": true, "GH_TOKEN": true, "GITHUB_TOKEN": true,
		"GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true, "GH_HOST": true,
	}
	out := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !blocked[key] {
			out = append(out, item)
		}
	}
	if realToken != "" {
		out = append(out, "GH_TOKEN="+realToken)
	}
	return out
}

func exitStatus(state *os.ProcessState) (status int, signal string) {
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return -1, ""
	}
	if ws.Signaled() {
		return 128 + int(ws.Signal()), ws.Signal().String()
	}
	return ws.ExitStatus(), ""
}

// RunProcess runs a host command in its own process group and always reaps it.
func RunProcess(ctx context.Context, command string, args []string, cwd string, env []string) ProcessResult {
	started := time.Now()
	cmd := exec.Command(command, args...)
	cmd.Dir, cmd.Env = cwd, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r := ProcessResult{Command: ShellJoin(append([]string{command}, args...)...), Status: -1}
	if err := cmd.Start(); err != nil {
		r.Stderr, r.Elapsed = err.Error(), time.Since(started)
		return r
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		r.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	r.Elapsed, r.Output, r.Stderr = time.Since(started), RedactBytes(stdout.Bytes()), Redact(stderr.String())
	if cmd.ProcessState != nil {
		r.Completed = true
		r.Status, r.Signal = exitStatus(cmd.ProcessState)
	}
	if err != nil && r.Stderr == "" && !r.TimedOut {
		r.Stderr = err.Error()
	}
	return r
}
