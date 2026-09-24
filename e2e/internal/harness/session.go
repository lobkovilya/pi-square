//go:build linux

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
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

var isolationArgs = []string{
	"--no-session", "--no-extensions", "--no-skills", "--no-prompt-templates",
	"--no-themes", "--no-context-files", "--no-approve", "--offline",
}

type SessionOptions struct {
	Cwd, Mode, Guard string
	Env              []string
	Timeout          time.Duration
	Cleanup          func()
}

type SessionError struct{ Message, Screen string }

func (e *SessionError) Error() string { return Redact(e.Message + "\nTerminal:\n" + e.Screen) }

type Session struct {
	cmd               *exec.Cmd
	pty               *os.File
	term              *terminal
	started, deadline time.Time
	mode              string
	writeMu           sync.Mutex
	stateMu           sync.RWMutex
	exited            bool
	exitCode          int
	signal            string
	waitErr           error
	waitDone          chan struct{}
	readDone          chan struct{}
	cleanup           func()
	closeOnce         sync.Once
}

// OpenPi appends only the fixed isolation flags and model guard after the
// caller-visible --. Permission mode and shell input always remain in specs.
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
	s := &Session{cmd: cmd, term: newTerminal(), started: started, deadline: started.Add(timeout), mode: options.Mode,
		waitDone: make(chan struct{}), readDone: make(chan struct{}), cleanup: options.Cleanup, exitCode: -1}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 240, Rows: 80})
	if err != nil {
		s.term.Close()
		if s.cleanup != nil {
			s.cleanup()
		}
		return nil, fmt.Errorf("start %s: %w", s.Invocation(), err)
	}
	s.pty = ptmx
	go s.readPTY()
	go func() { _, _ = io.Copy(lockedWriter{s}, s.term) }()
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

type lockedWriter struct{ s *Session }

func (w lockedWriter) Write(p []byte) (int, error) { return w.s.write(p) }

func (s *Session) Invocation() string     { return ShellJoin(s.cmd.Args...) }
func (s *Session) Screen() string         { return Redact(s.term.Screen()) }
func (s *Session) Elapsed() time.Duration { return time.Since(s.started) }
func (s *Session) Error(message string) error {
	return &SessionError{message + "\ninvocation=" + s.Invocation() + "\ncwd=" + s.cmd.Dir, s.Screen()}
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

func (s *Session) wait() {
	err := s.cmd.Wait()
	s.stateMu.Lock()
	s.exited, s.waitErr = true, err
	if state := s.cmd.ProcessState; state != nil {
		if ws, ok := state.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				s.signal = ws.Signal().String()
				s.exitCode = 128 + int(ws.Signal())
			} else {
				s.exitCode = ws.ExitStatus()
			}
		}
	}
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
			return s.Error(err.Error())
		}
		if ok {
			return nil
		}
		if exited, code := s.isExited(); exited {
			return s.Error(fmt.Sprintf("pi exited during %s (status %d)", label, code))
		}
		if time.Now().After(s.deadline) {
			return s.Error("session deadline exceeded during " + label)
		}
		select {
		case <-ctx.Done():
			return s.Error(ctx.Err().Error() + " during " + label)
		case <-time.After(pollInterval):
		}
	}
}

func (s *Session) Ready(ctx context.Context) error {
	mode := regexp.MustCompile(`\b` + regexp.QuoteMeta(s.mode) + `\b`)
	if err := s.waitFor(ctx, func() (bool, error) {
		text := s.Screen()
		return strings.Contains(text, "no-model-guard") && mode.MatchString(text), nil
	}, "startup and model tripwire"); err != nil {
		return err
	}
	return s.Slash(ctx, "/gh-mode status", "GitHub mode: "+s.mode)
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
	completed func() bool
}
type captureOutcome struct {
	result ProcessResult
	err    error
}

func (r *RunningCapture) Completed() bool { return r.completed() }
func (r *RunningCapture) Wait(ctx context.Context) (ProcessResult, error) {
	select {
	case out := <-r.result:
		return out.result, out.err
	case <-ctx.Done():
		return ProcessResult{}, ctx.Err()
	}
}

func (s *Session) StartBash(ctx context.Context, prefix, command string) (*RunningCapture, error) {
	if prefix != "!" && prefix != "!!" {
		return nil, fmt.Errorf("prefix must be explicit ! or !!")
	}
	n, err := nonce()
	if err != nil {
		return nil, err
	}
	shell := captureShell(command, n)
	truncations := count(s.Screen(), truncationNotice)
	if _, err := s.write([]byte("\x1b[200~" + prefix + " " + shell + "\x1b[201~\r")); err != nil {
		return nil, s.Error("write bash command: " + err.Error())
	}
	out := make(chan captureOutcome, 1)
	completed := func() bool { return completionVisible(s.Screen(), n) }
	go func() {
		result := ProcessResult{Status: -1}
		err := s.waitFor(ctx, func() (bool, error) {
			if count(s.Screen(), truncationNotice) > truncations {
				return false, errors.New("command output exceeded pi's truncation limits and cannot be captured")
			}
			return completed(), nil
		}, "bash output")
		if err == nil {
			var ok bool
			result, ok, err = decodeCapture(s.Screen(), n)
			for toggles := 0; err == nil && !ok && toggles < 2; toggles++ {
				before := toolOutputToggles(s.Screen())
				_, _ = s.write([]byte{"\x0f"[0]})
				err = s.waitFor(ctx, func() (bool, error) {
					result, ok, err = decodeCapture(s.Screen(), n)
					return ok || toolOutputToggles(s.Screen()) != before, err
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
		}
		result.Mode = s.mode
		result.Command = prefix + " " + command
		result.Elapsed = s.Elapsed()
		result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded) || (err != nil && strings.Contains(err.Error(), "deadline exceeded"))
		if err != nil {
			var sessionErr *SessionError
			if !errors.As(err, &sessionErr) {
				err = s.Error(err.Error())
			}
		}
		out <- captureOutcome{result, err}
	}()
	return &RunningCapture{out, completed}, nil
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
	return r.Wait(ctx)
}

func (s *Session) Quit(ctx context.Context) error {
	if _, err := s.write([]byte("/quit\r")); err != nil {
		return s.Error("write /quit: " + err.Error())
	}
	if err := s.waitFor(ctx, func() (bool, error) {
		exited, code := s.isExited()
		if exited && code != 0 {
			return false, fmt.Errorf("pi exited with status %d", code)
		}
		return exited, nil
	}, "clean exit"); err != nil {
		return err
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
		if s.pty != nil {
			_ = s.pty.Close()
		}
		_ = s.term.Close()
		select {
		case <-s.readDone:
		case <-time.After(time.Second):
		}
		if s.cleanup != nil {
			s.cleanup()
		}
	})
	return nil
}
