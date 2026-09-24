package harness

import (
	"fmt"
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

func FormatResult(r ProcessResult) string {
	return Redact(fmt.Sprintf("mode=%s status=%d signal=%s elapsed=%s timedOut=%t completed=%t\ncommand=%s\noutput:\n%s\nstderr:\n%s",
		r.Mode, r.Status, r.Signal, r.Elapsed, r.TimedOut, r.Completed, r.Command, r.Output, r.Stderr))
}

func RequireSuccess(label string, r ProcessResult) error {
	if r.Completed && r.Status == 0 && !r.TimedOut {
		return nil
	}
	return fmt.Errorf("%s failed\n%s", label, FormatResult(r))
}
