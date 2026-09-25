//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const gatewayProtocol = 1

var instanceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,39}$`)

type instancePaths struct{ dir, lock, log, management, ro, rw string }

func instancePathsAt(dir string) instancePaths {
	return instancePaths{
		dir:        dir,
		lock:       filepath.Join(dir, ".lock"),
		log:        filepath.Join(dir, "gateway.log"),
		management: filepath.Join(dir, "control.sock"),
		ro:         filepath.Join(dir, filepath.Base(roSocket)),
		rw:         filepath.Join(dir, filepath.Base(wSocket)),
	}
}

var errProtocolMismatch = errors.New("gateway protocol mismatch")

// daemonError is a refusal reported by a healthy gateway, as opposed to a
// connection or handshake failure.
type daemonError struct{ message string }

func (e *daemonError) Error() string { return e.message }

func pathsFor(name string) (instancePaths, error) {
	if !instanceName.MatchString(name) {
		return instancePaths{}, fmt.Errorf("invalid gateway name %q (use 1-40 letters, digits, _ or -)", name)
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return instancePaths{}, err
		}
		base = cache
	}
	dir := filepath.Join(base, "pi-square", "instances", name)
	p := instancePathsAt(dir)
	if len(p.management) >= 108 {
		return instancePaths{}, errors.New("gateway socket path too long")
	}
	return p, nil
}

func lockInstance(p instancePaths) (*os.File, error) {
	if err := os.MkdirAll(p.dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(p.dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

type gatewayMessage struct {
	Op      string `json:"op"`
	Version int    `json:"version"`
	Force   bool   `json:"force,omitempty"`
	CA      string `json:"ca,omitempty"`
	Active  int    `json:"active,omitempty"`
	Error   string `json:"error,omitempty"`
}

func probeInstance(p instancePaths, op string, force bool) (net.Conn, gatewayMessage, error) {
	conn, err := net.DialTimeout("unix", p.management, 500*time.Millisecond)
	if err != nil {
		return nil, gatewayMessage{}, err
	}
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(gatewayMessage{Op: op, Version: gatewayProtocol, Force: force}); err != nil {
		conn.Close()
		return nil, gatewayMessage{}, err
	}
	var msg gatewayMessage
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		conn.Close()
		return nil, gatewayMessage{}, err
	}
	if msg.Version != gatewayProtocol {
		conn.Close()
		return nil, msg, fmt.Errorf("%w: gateway=%d wrapper=%d", errProtocolMismatch, msg.Version, gatewayProtocol)
	}
	if msg.Error != "" {
		conn.Close()
		return nil, msg, &daemonError{message: msg.Error}
	}
	conn.SetDeadline(time.Time{})
	return conn, msg, nil
}

// attachInstance holds the startup lock until the live attachment is established.
// The returned connection must remain open for the entire supervisor lifetime.
func attachInstance(name string, startDefault bool) (net.Conn, string, instancePaths, error) {
	p, err := pathsFor(name)
	if err != nil {
		return nil, "", p, err
	}
	if !startDefault {
		if _, err := os.Stat(p.dir); err != nil {
			return nil, "", p, fmt.Errorf("gateway %q does not exist: %w", name, err)
		}
	}
	lock, err := lockInstance(p)
	if err != nil {
		return nil, "", p, err
	}
	defer lock.Close()
	conn, msg, err := probeInstance(p, "attach", false)
	if err == nil {
		return conn, msg.CA, p, nil
	}
	if errors.Is(err, errProtocolMismatch) {
		return nil, "", p, err
	}
	if !startDefault {
		return nil, "", p, fmt.Errorf("gateway %q is not healthy: %w", name, err)
	}
	if err := startInstanceLocked(p); err != nil {
		return nil, "", p, err
	}
	conn, msg, err = probeInstance(p, "attach", false)
	if err != nil {
		return nil, "", p, fmt.Errorf("attach newly started gateway: %w", err)
	}
	return conn, msg.CA, p, nil
}

func startInstance(name string) error {
	p, err := pathsFor(name)
	if err != nil {
		return err
	}
	lock, err := lockInstance(p)
	if err != nil {
		return err
	}
	defer lock.Close()
	conn, _, err := probeInstance(p, "health", false)
	if err == nil {
		conn.Close()
		return fmt.Errorf("gateway %q is already running", name)
	}
	if errors.Is(err, errProtocolMismatch) {
		return err
	}
	return startInstanceLocked(p)
}

func startInstanceLocked(p instancePaths) error {
	token, err := resolveGitHubToken()
	if err != nil {
		return err
	}
	for _, path := range []string{p.management, p.ro, p.rw} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		reader.Close()
		writer.Close()
		return err
	}
	defer devnull.Close()
	logFile, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		reader.Close()
		writer.Close()
		return err
	}
	defer logFile.Close()
	cmd := exec.Command("/proc/self/exe", gatewayMarker, p.dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, logFile
	cmd.ExtraFiles = []*os.File{reader}
	cmd.Env = sanitizeParentEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		reader.Close()
		writer.Close()
		return err
	}
	reader.Close()
	_, writeErr := io.WriteString(writer, token+"\n")
	writer.Close()
	if writeErr != nil {
		cmd.Process.Kill()
		cmd.Process.Release()
		return writeErr
	}
	defer cmd.Process.Release()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err := probeInstance(p, "health", false)
		if err == nil {
			conn.Close()
			return nil
		}
		if errors.Is(err, errProtocolMismatch) {
			return err
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return fmt.Errorf("gateway exited before readiness: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Process.Kill()
	return errors.New("gateway did not become ready")
}

func stopInstance(name string, force bool) error {
	p, err := pathsFor(name)
	if err != nil {
		return err
	}
	lock, err := lockInstance(p)
	if err != nil {
		return err
	}
	defer lock.Close()
	conn, _, err := probeInstance(p, "stop", force)
	var refused *daemonError
	if errors.As(err, &refused) {
		return fmt.Errorf("gateway %q: %w", name, err)
	}
	if err != nil {
		return fmt.Errorf("gateway %q is not healthy: %w", name, err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var b [1]byte
	if _, err := conn.Read(b[:]); err != io.EOF {
		return fmt.Errorf("gateway shutdown incomplete: %v", err)
	}
	return nil
}

func listInstances(out io.Writer) error {
	p, err := pathsFor("default")
	if err != nil {
		return err
	}
	base := filepath.Dir(p.dir)
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !instanceName.MatchString(entry.Name()) {
			continue
		}
		path, _ := pathsFor(entry.Name())
		conn, msg, err := probeInstance(path, "health", false)
		if err != nil {
			fmt.Fprintf(out, "%s\tunhealthy (%v)\n", entry.Name(), err)
		} else {
			conn.Close()
			fmt.Fprintf(out, "%s\thealthy\t%d active\n", entry.Name(), msg.Active)
		}
	}
	return nil
}

// The daemon stays in the host mount namespace. No credential or private CA is
// stored on disk; only its public CA is returned over the management socket.
func runGatewayDaemon(dir string) error {
	p := instancePathsAt(dir)
	secret := os.NewFile(3, "gateway credential")
	if secret == nil {
		return errors.New("missing gateway credential")
	}
	data, err := io.ReadAll(io.LimitReader(secret, 16<<10))
	secret.Close()
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return errors.New("empty gateway credential")
	}
	ca, err := newEphemeralCA()
	if err != nil {
		return err
	}
	gw, err := newGateway(ca, token, log.New(os.Stderr, "", log.LstdFlags))
	if err != nil {
		return err
	}
	listeners := make([]net.Listener, 0, 3)
	for _, spec := range []struct {
		path  string
		class policyClass
	}{{p.ro, classRO}, {p.rw, classW}} {
		l, err := net.Listen("unix", spec.path)
		if err != nil {
			return err
		}
		listeners = append(listeners, l)
		os.Chmod(spec.path, 0600)
		go func(l net.Listener, class policyClass) {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go gw.serveIngress(c, class)
			}
		}(l, spec.class)
	}
	l, err := net.Listen("unix", p.management)
	if err != nil {
		return err
	}
	listeners = append(listeners, l)
	os.Chmod(p.management, 0600)
	var mu sync.Mutex
	active := 0
	stopping := make(chan struct{})
	var once sync.Once
	for {
		conn, err := l.Accept()
		if err != nil {
			break
		}
		go func() {
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var req gatewayMessage
			if json.NewDecoder(conn).Decode(&req) != nil {
				return
			}
			mu.Lock()
			resp := gatewayMessage{Version: gatewayProtocol}
			if req.Version != gatewayProtocol {
				resp.Error = errProtocolMismatch.Error()
			} else {
				switch req.Op {
				case "health":
				case "attach":
					active++
					resp.CA = string(ca.certPEM)
				case "stop":
					if active != 0 && !req.Force {
						resp.Error = fmt.Sprintf("%d active session(s); use --force", active)
					}
				default:
					resp.Error = "unknown gateway operation"
				}
			}
			resp.Active = active
			json.NewEncoder(conn).Encode(resp)
			mu.Unlock()
			if resp.Error != "" {
				return
			}
			if req.Op == "attach" {
				conn.SetReadDeadline(time.Time{})
				io.Copy(io.Discard, conn)
				mu.Lock()
				active--
				mu.Unlock()
			}
			if req.Op == "stop" {
				once.Do(func() { close(stopping); l.Close() })
				<-stopping
			}
		}()
	}
	for _, listener := range listeners {
		listener.Close()
	}
	return nil
}
