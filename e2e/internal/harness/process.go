package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ProcessResult keeps command output separate from harness diagnostics.
type ProcessResult struct {
	Mode      string
	Command   string
	Status    int
	Signal    string
	Output    []byte
	Stderr    string
	TimedOut  bool
	Elapsed   time.Duration
	Completed bool
}

var secrets []string

func SetSecrets(values ...string) {
	secrets = secrets[:0]
	for _, value := range values {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
}

func Redact(value string) string {
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

func FormatResult(r ProcessResult) string {
	return Redact(fmt.Sprintf("mode=%s status=%d signal=%s elapsed=%s timedOut=%t completed=%t\ncommand=%s\noutput:\n%s\nstderr:\n%s",
		r.Mode, r.Status, r.Signal, r.Elapsed, r.TimedOut, r.Completed, r.Command, r.Output, r.Stderr))
}

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
	r.Elapsed, r.Output, r.Stderr = time.Since(started), stdout.Bytes(), Redact(stderr.String())
	if cmd.ProcessState != nil {
		r.Completed = true
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				r.Signal = status.Signal().String()
				r.Status = 128 + int(status.Signal())
			} else {
				r.Status = status.ExitStatus()
			}
		}
	}
	if err != nil && r.Stderr == "" && !r.TimedOut {
		r.Stderr = err.Error()
	}
	r.Output = []byte(Redact(string(r.Output)))
	return r
}

func RequireSuccess(label string, r ProcessResult) error {
	if r.Completed && r.Status == 0 && !r.TimedOut {
		return nil
	}
	return fmt.Errorf("%s failed\n%s", label, FormatResult(r))
}
