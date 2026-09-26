//go:build linux

package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	defaultSessionTimeout = 75 * time.Second
	pollInterval          = 25 * time.Millisecond
)

var ErrSessionDeadline = errors.New("session deadline exceeded")

var isolationArgs = []string{
	"--no-session", "--no-extensions", "--no-skills", "--no-prompt-templates",
	"--no-themes", "--no-context-files", "--no-approve", "--offline",
}

type SessionError struct {
	Message, Screen string
	Cause           error
}

func (e *SessionError) Error() string { return Redact(e.Message + "\nTerminal:\n" + e.Screen) }
func (e *SessionError) Unwrap() error { return e.Cause }

type Session struct {
	cmd               *exec.Cmd
	pty               *os.File
	term              *terminal
	started, deadline time.Time
	profile           string
	writeMu           sync.Mutex
	stateMu           sync.RWMutex
	exited            bool
	exitCode          int
	waitDone          chan struct{}
	readDone          chan struct{}
	replyDone         chan struct{}
	cleanup           func()
	closeOnce         sync.Once
}

// OpenPi appends only the fixed isolation flags and model guard after the
// caller-visible --. Permission profile and shell input always remain in specs.
func OpenPi(ctx context.Context, cmd *exec.Cmd, options SessionOptions) (*Session, error) {
	if len(cmd.Args) == 0 || cmd.Args[len(cmd.Args)-1] != "--" {
		return nil, fmt.Errorf("pi-square command must visibly end in -- before OpenPi isolation arguments")
	}
	cmd.Args = append(cmd.Args, isolationArgs...)
	if options.Guard != "" {
		cmd.Args = append(cmd.Args, "--extension", options.Guard)
	}
	cmd.Dir, cmd.Env = options.Cwd, withEnv(options.Env, "TERM", "xterm-256color")
	started := time.Now()
	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultSessionTimeout
	}
	s := &Session{cmd: cmd, term: newTerminal(), started: started, deadline: started.Add(timeout), profile: options.Profile,
		waitDone: make(chan struct{}), readDone: make(chan struct{}), replyDone: make(chan struct{}), cleanup: options.Cleanup, exitCode: -1}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 240, Rows: 80})
	if err != nil {
		_ = s.term.Close()
		if s.cleanup != nil {
			s.cleanup()
		}
		return nil, fmt.Errorf("start %s: %w", s.Invocation(), err)
	}
	s.pty = ptmx
	go s.readPTY()
	go s.forwardReplies()
	go s.wait()
	if err := s.Ready(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func withEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}

// Resize changes both the PTY and the screen emulator for footer layout checks.
func (s *Session) Resize(cols, rows int) error {
	s.term.mu.Lock()
	s.term.vt.Resize(cols, rows)
	s.term.mu.Unlock()
	return pty.Setsize(s.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (s *Session) Invocation() string     { return ShellJoin(s.cmd.Args...) }
func (s *Session) Screen() string         { return Redact(s.term.Screen()) }
func (s *Session) Elapsed() time.Duration { return time.Since(s.started) }
func (s *Session) Error(message string) error {
	return s.fail(nil, message)
}

func (s *Session) fail(cause error, message string) error {
	return &SessionError{Message: message + "\ninvocation=" + s.Invocation() + "\ncwd=" + s.cmd.Dir, Screen: s.Screen(), Cause: cause}
}

func (s *Session) readPTY() {
	defer close(s.readDone)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			_, _ = s.term.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// forwardReplies keeps draining after the PTY is gone: the emulator writes a
// query reply synchronously inside Write, so an idle reader would park readPTY
// while it holds the terminal lock.
func (s *Session) forwardReplies() {
	defer close(s.replyDone)
	buf := make([]byte, 4096)
	for {
		n, err := s.term.Read(buf)
		if n > 0 {
			_, _ = s.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) wait() {
	_ = s.cmd.Wait()
	code := -1
	if state := s.cmd.ProcessState; state != nil {
		code, _ = exitStatus(state)
	}
	s.stateMu.Lock()
	s.exited, s.exitCode = true, code
	s.stateMu.Unlock()
	close(s.waitDone)
}

func (s *Session) write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.pty.Write(p)
}

func count(text, needle string) int { return strings.Count(text, needle) }
func (s *Session) isExited() (bool, int) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.exited, s.exitCode
}

func (s *Session) waitFor(ctx context.Context, condition func() (bool, error), label string) error {
	for {
		ok, err := condition()
		if err != nil {
			return s.fail(err, err.Error())
		}
		if ok {
			return nil
		}
		if exited, code := s.isExited(); exited {
			return s.Error(fmt.Sprintf("pi exited during %s (status %d)", label, code))
		}
		if time.Now().After(s.deadline) {
			return s.fail(ErrSessionDeadline, ErrSessionDeadline.Error()+" during "+label)
		}
		select {
		case <-ctx.Done():
			return s.fail(ctx.Err(), ctx.Err().Error()+" during "+label)
		case <-time.After(pollInterval):
		}
	}
}

func (s *Session) Ready(ctx context.Context) error {
	profile := regexp.MustCompile(`\b` + regexp.QuoteMeta(s.profile) + `\b`)
	if err := s.waitFor(ctx, func() (bool, error) {
		text := s.Screen()
		return strings.Contains(text, "no-model-guard") && profile.MatchString(text), nil
	}, "startup and model tripwire"); err != nil {
		return err
	}
	return s.Slash(ctx, "/pi-square-profile status", "pi-square: profile "+s.profile)
}

func (s *Session) Slash(ctx context.Context, command, expected string) error {
	before := count(s.Screen(), expected)
	if _, err := s.write([]byte(command + "\r")); err != nil {
		return s.Error("write " + command + ": " + err.Error())
	}
	return s.waitFor(ctx, func() (bool, error) { return count(s.Screen(), expected) > before, nil }, command)
}

type RunningCapture struct {
	result    <-chan captureOutcome
	outcome   captureOutcome
	once      sync.Once
	completed func() bool
}
type captureOutcome struct {
	result ProcessResult
	err    error
}

func (r *RunningCapture) Completed() bool { return r.completed() }

// Wait needs no context: the capture goroutine is already bounded by the
// StartBash context and the session deadline, and its result carries the
// timeout diagnostics a caller would otherwise lose.
func (r *RunningCapture) Wait() (ProcessResult, error) {
	r.once.Do(func() { r.outcome = <-r.result })
	return r.outcome.result, r.outcome.err
}

func (s *Session) StartBash(ctx context.Context, prefix, command string) (*RunningCapture, error) {
	if prefix != "!" && prefix != "!!" {
		return nil, fmt.Errorf("prefix must be explicit ! or !!")
	}
	n, err := nonce()
	if err != nil {
		return nil, err
	}
	truncations := count(s.Screen(), truncationNotice)
	if _, err := s.write([]byte("\x1b[200~" + prefix + " " + captureShell(command, n) + "\x1b[201~\r")); err != nil {
		return nil, s.Error("write bash command: " + err.Error())
	}
	out := make(chan captureOutcome, 1)
	completed := func() bool { return completionVisible(s.Screen(), n) }
	go func() {
		result := ProcessResult{Status: -1, Profile: s.profile, Command: prefix + " " + command}
		var frame ProcessResult
		var ok bool
		err := s.waitFor(ctx, func() (bool, error) {
			text := s.Screen()
			if count(text, truncationNotice) > truncations {
				return false, errors.New("command output exceeded pi's truncation limits and cannot be captured")
			}
			if !completionVisible(text, n) {
				return false, nil
			}
			var err error
			frame, ok, err = decodeCapture(text, n)
			return ok || !headerVisible(text, n), err
		}, "bash output")
		for toggles := 0; err == nil && !ok && toggles < 2; toggles++ {
			before := toolOutputToggles(s.Screen())
			_, _ = s.write([]byte{0x0f})
			err = s.waitFor(ctx, func() (bool, error) {
				text := s.Screen()
				var err error
				frame, ok, err = decodeCapture(text, n)
				return ok || (toolOutputToggles(text) != before && !headerVisible(text, n)), err
			}, "bash completion")
		}
		if err == nil && !ok {
			err = errors.New("bash result frame is not visible after expanding tool output")
		}
		if err == nil {
			if exited, code := s.isExited(); exited {
				err = fmt.Errorf("pi exited with status %d after emitting a shell frame", code)
			}
		}
		if err == nil {
			result.Status, result.Output, result.Completed = frame.Status, RedactBytes(frame.Output), true
		} else {
			var sessionErr *SessionError
			if !errors.As(err, &sessionErr) {
				err = s.Error(err.Error())
			}
		}
		result.Elapsed = s.Elapsed()
		result.TimedOut = errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrSessionDeadline)
		out <- captureOutcome{result, err}
	}()
	return &RunningCapture{result: out, completed: completed}, nil
}

var toolStatus = regexp.MustCompile(`Tool output: (?:expanded|collapsed)`)

func toolOutputToggles(text string) string {
	return strings.Join(toolStatus.FindAllString(text, -1), ",")
}

func (s *Session) Bash(ctx context.Context, prefix, command string) (ProcessResult, error) {
	r, err := s.StartBash(ctx, prefix, command)
	if err != nil {
		return ProcessResult{}, err
	}
	return r.Wait()
}

func (s *Session) Quit(ctx context.Context) error {
	if _, err := s.write([]byte("/quit\r")); err != nil {
		return s.Error("write /quit: " + err.Error())
	}
	select {
	case <-s.waitDone:
	case <-ctx.Done():
		return s.fail(ctx.Err(), ctx.Err().Error()+" during clean exit")
	case <-time.After(time.Until(s.deadline)):
		return s.fail(ErrSessionDeadline, ErrSessionDeadline.Error()+" during clean exit")
	}
	if _, code := s.isExited(); code != 0 {
		return s.Error(fmt.Sprintf("pi exited with status %d during clean exit", code))
	}
	return nil
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		if exited, _ := s.isExited(); !exited && s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-s.waitDone:
		case <-time.After(5 * time.Second):
		}
		_ = s.pty.Close()
		select {
		case <-s.readDone:
		case <-time.After(time.Second):
		}
		_ = s.term.Close()
		<-s.replyDone
		if s.cleanup != nil {
			s.cleanup()
		}
	})
	return nil
}
