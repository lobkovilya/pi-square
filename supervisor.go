//go:build linux

package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// secrets travel from the host launcher to the supervisor over an inherited
// pipe so they never appear in any environment or argument list.
type secrets struct {
	GitHubToken  string   `json:"github_token"`
	WToken       string   `json:"w_token"`
	GitIdentity  []string `json:"git_identity"`
	SystemCAPath string   `json:"system_ca_path"`
}

// launchRequest is what the stub asks the supervisor to run. The supervisor
// never trusts Mode alone for a write: publish additionally requires WToken.
type launchRequest struct {
	Mode    string `json:"mode"`
	Cwd     string `json:"cwd"`
	Command string `json:"command"`
	WToken  string `json:"wtoken,omitempty"`
}

type launchResponse struct {
	Started bool   `json:"started"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Exit    int    `json:"exit"`
}

type worker struct {
	cmd   *exec.Cmd
	netns *os.File
}

type supervisor struct {
	gw       *gateway
	workers  map[policyClass]*worker
	baseEnv  []string
	workdir  string
	home     string
	devMode  bool
	piPath   string
	secrets  secrets
	logger   *log.Logger
	caBundle []byte

	jobs   *jobTracker
	closed chan struct{}
}

// stage is the supervisor entrypoint. It owns the shared restricted root, the
// gateway, the network namespace workers, the control channel, and pi's
// lifecycle.
func stage(args []string) error {
	root := os.Getenv("PI_SQUARE_ROOT")
	workdir := os.Getenv("PI_SQUARE_WORKDIR")
	home := os.Getenv("PI_SQUARE_HOME")
	pi := os.Getenv("PI_SQUARE_PI")
	fff := os.Getenv("PI_SQUARE_FFF")
	if root == "" || workdir == "" || home == "" || pi == "" || fff == "" {
		return errors.New("invalid internal stage")
	}

	sec, err := readSecrets()
	if err != nil {
		return err
	}

	if err := setupRestrictedRoot(root, workdir, home, fff, os.Getenv("XDG_RUNTIME_DIR")); err != nil {
		return err
	}

	logger := log.New(os.Stderr, "", 0)
	s := &supervisor{
		workers: map[policyClass]*worker{},
		baseEnv: commandBaseEnv(os.Environ()),
		workdir: workdir,
		home:    home,
		devMode: os.Getenv("PI_SQUARE_DEV_MODE") == "1",
		piPath:  pi,
		secrets: sec,
		logger:  logger,
		jobs:    newJobTracker(),
		closed:  make(chan struct{}),
	}

	if err := s.start(); err != nil {
		s.shutdown()
		return err
	}
	defer s.shutdown()

	// Integration hook: drive a single command through the real stub, control
	// channel, network namespace, and gateway instead of launching pi. Used by
	// the isolation and policy tests; never reachable through the normal CLI.
	if selftestMode := os.Getenv("PI_SQUARE_SELFTEST_MODE"); selftestMode != "" {
		return s.runSelftest(selftestMode, os.Getenv("PI_SQUARE_SELFTEST_CMD"))
	}

	return s.runPi(args)
}

func (s *supervisor) runSelftest(mode, command string) error {
	env := stripSupervisorEnv(sanitizeParentEnv(os.Environ()))
	env = setEnv(env, "PI_SQUARE_WTOKEN", s.secrets.WToken)
	env = setEnv(env, "PI_SQUARE_GH_HELPER", helperPath)

	// Emulate exactly what the extension does: rewrite the command to exec the
	// stub, then let bash run it, so the whole real invocation path is exercised.
	bashCommand := fmt.Sprintf("exec '%s' %s %s %s", helperPath, stubMarker, mode, encode(command))
	cmd := exec.Command("bash", "-lc", bashCommand)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit
		}
		return err
	}
	return nil
}

func (s *supervisor) start() error {
	ca, err := newEphemeralCA()
	if err != nil {
		return err
	}
	bundle, err := combinedCABundle(ca.certPEM)
	if err != nil {
		return err
	}
	s.caBundle = bundle
	if err := os.MkdirAll(gatewayDir, 0700); err != nil {
		return fmt.Errorf("create gateway directory: %w", err)
	}
	if err := os.WriteFile(caBundlePath, bundle, 0644); err != nil {
		return fmt.Errorf("write CA bundle: %w", err)
	}

	gw, err := newGateway(ca, s.secrets.GitHubToken, s.logger)
	if err != nil {
		return err
	}
	s.gw = gw
	if err := s.listenIngress(classRO, roSocket); err != nil {
		return err
	}
	if err := s.listenIngress(classW, wSocket); err != nil {
		return err
	}
	if err := s.startWorker(classRO, roSocket); err != nil {
		return err
	}
	if err := s.startWorker(classW, wSocket); err != nil {
		return err
	}
	return s.listenControl()
}

func (s *supervisor) listenIngress(class policyClass, socketPath string) error {
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s ingress: %w", class, err)
	}
	os.Chmod(socketPath, 0600)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.gw.serveIngress(conn, class)
		}
	}()
	return nil
}

func (s *supervisor) startWorker(class policyClass, socketPath string) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()

	cmd := exec.Command(helperPath, netnsWorkerMarker, socketPath)
	cmd.Env = minimalEnv(s.baseEnv)
	cmd.ExtraFiles = []*os.File{pw}
	cmd.SysProcAttr = netnsAttr()
	if err := cmd.Start(); err != nil {
		pw.Close()
		return fmt.Errorf("start %s network worker: %w", class, err)
	}
	pw.Close()

	// Wait for the worker to signal that loopback is up and the frontend is
	// listening, then capture a handle to its network namespace.
	ready := make([]byte, 1)
	pr.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := pr.Read(ready); err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("%s network worker did not become ready: %w", class, err)
	}
	netns, err := os.Open("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/ns/net")
	if err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("open %s network namespace: %w", class, err)
	}
	s.workers[class] = &worker{cmd: cmd, netns: netns}
	return nil
}

func (s *supervisor) listenControl() error {
	os.Remove(controlSocket)
	listener, err := net.Listen("unix", controlSocket)
	if err != nil {
		return fmt.Errorf("listen on control socket: %w", err)
	}
	os.Chmod(controlSocket, 0600)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			unixConn, ok := conn.(*net.UnixConn)
			if !ok {
				conn.Close()
				continue
			}
			go s.handleControl(unixConn)
		}
	}()
	return nil
}

func (s *supervisor) handleControl(conn *net.UnixConn) {
	defer conn.Close()

	req, fds, err := recvRequest(conn)
	if err != nil {
		closeFiles(fds)
		return
	}
	defer closeFiles(fds)
	if len(fds) != 3 {
		s.reply(conn, launchResponse{Code: denyUnsupportedRequest, Message: "expected three standard descriptors"})
		return
	}

	class, readonlyWorkdir, throwawayHome, ok := deriveMode(req.Mode)
	if !ok {
		s.reply(conn, launchResponse{Code: denyUnsupportedRequest, Message: "unknown launch mode"})
		return
	}
	if class == classW && req.WToken != s.secrets.WToken {
		s.reply(conn, launchResponse{Code: denyWriteRequiresPublish, Message: "publish is required to run write-enabled commands"})
		return
	}

	home := s.home
	if throwawayHome {
		if tmp, err := os.MkdirTemp("/tmp", "pi-command-home-"); err == nil {
			home = tmp
			defer os.RemoveAll(tmp)
		}
	}

	cmd, err := s.launchCommand(req, class, readonlyWorkdir, home, fds)
	if err != nil {
		s.reply(conn, launchResponse{Code: "launch_failed", Message: err.Error()})
		return
	}

	id := s.jobs.add(class, cmd)
	defer s.jobs.remove(id)

	waitCh := make(chan int, 1)
	go func() { waitCh <- waitExit(cmd) }()
	cancelCh := watchClose(conn)

	select {
	case code := <-waitCh:
		s.reply(conn, launchResponse{Started: true, Exit: code})
	case <-cancelCh:
		cmd.Process.Kill()
		code := <-waitCh
		s.reply(conn, launchResponse{Started: true, Exit: code})
	case <-s.closed:
		cmd.Process.Kill()
		<-waitCh
	}
}

func (s *supervisor) launchCommand(req launchRequest, class policyClass, readonlyWorkdir bool, home string, stdio []*os.File) (*exec.Cmd, error) {
	w := s.workers[class]
	if w == nil {
		return nil, fmt.Errorf("no network worker for class %s", class)
	}
	cmd := exec.Command(helperPath, commandMarker,
		string(class),
		boolArg(readonlyWorkdir),
		encode(req.Cwd),
		encode(req.Command),
		encode(s.workdir),
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio[0], stdio[1], stdio[2]
	cmd.Env = s.commandEnv(home)
	cmd.ExtraFiles = []*os.File{w.netns}
	cmd.SysProcAttr = commandAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func (s *supervisor) commandEnv(home string) []string {
	env := append([]string(nil), s.baseEnv...)
	env = setEnv(env, "HOME", home)
	env = setEnv(env, "GH_TOKEN", dummyCommandToken)
	proxy := "http://" + frontendListen
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		env = setEnv(env, key, proxy)
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		env = setEnv(env, key, "")
	}
	for _, key := range []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "GIT_SSL_CAINFO", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE"} {
		env = setEnv(env, key, caBundlePath)
	}
	env = unsetEnv(env, "SSL_CERT_DIR")
	env = append(env, s.secrets.GitIdentity...)
	return env
}

func (s *supervisor) runPi(args []string) error {
	// Drop the stage-internal variables first, then install only the ones the
	// extension and stub need. The supervisor keeps its own secrets out of pi.
	env := stripSupervisorEnv(sanitizeParentEnv(os.Environ()))
	env = setEnv(env, "PI_SQUARE_ACTIVE", "1")
	env = setEnv(env, "PI_SQUARE_GH_HELPER", helperPath)
	env = setEnv(env, "PI_SQUARE_WTOKEN", s.secrets.WToken)
	if s.devMode {
		env = setEnv(env, "PI_SQUARE_DEV_MODE", "1")
	}
	if initialMode := os.Getenv("PI_SQUARE_INITIAL_GH_MODE"); initialMode != "" {
		env = setEnv(env, "PI_SQUARE_INITIAL_GH_MODE", initialMode)
	}

	cmd := exec.Command(helperPath, append([]string{piStageMarker, s.piPath}, args...)...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit
		}
		return fmt.Errorf("run pi: %w", err)
	}
	return nil
}

func (s *supervisor) reply(conn *net.UnixConn, resp launchResponse) {
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	json.NewEncoder(conn).Encode(resp)
}

func (s *supervisor) shutdown() {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	s.jobs.killAll()
	for _, w := range s.workers {
		if w.netns != nil {
			w.netns.Close()
		}
		if w.cmd != nil && w.cmd.Process != nil {
			w.cmd.Process.Kill()
		}
	}
	for _, socketPath := range []string{roSocket, wSocket, controlSocket} {
		os.Remove(socketPath)
	}
}

// runPiStage drops every capability inherited from the supervisor and then
// becomes pi. Pi is trusted but must not hold the namespace-management
// capabilities the supervisor keeps.
func runPiStage(args []string) error {
	if len(args) < 1 {
		return errors.New("pi stage requires the pi path")
	}
	piPath := args[0]
	if err := dropCapabilities(); err != nil {
		return fmt.Errorf("drop pi capabilities: %w", err)
	}
	argv := append([]string{"pi", "--extension", extensionPath}, args[1:]...)
	return unix.Exec(piPath, argv, os.Environ())
}

// commandStage joins the class network namespace, applies the per-command
// filesystem policy, drops all capabilities, and becomes the command.
func commandStage(args []string) error {
	if len(args) != 5 {
		return errors.New("invalid command stage")
	}
	readonlyWorkdir := args[1] == "1"
	cwd, err := decode(args[2])
	if err != nil {
		return err
	}
	command, err := decode(args[3])
	if err != nil {
		return err
	}
	workdir, err := decode(args[4])
	if err != nil {
		return err
	}

	if err := unix.Setns(netnsFD, unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("join network namespace: %w", err)
	}
	unix.Close(netnsFD)

	if err := setupCommandSandbox(workdir, cwd, readonlyWorkdir); err != nil {
		return err
	}
	if err := dropCapabilities(); err != nil {
		return fmt.Errorf("drop command capabilities: %w", err)
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		return errors.New("bash not found in PATH")
	}
	return unix.Exec(bash, []string{"bash", "-lc", command}, os.Environ())
}

func readSecrets() (secrets, error) {
	file := os.NewFile(secretsFD, "secrets")
	if file == nil {
		return secrets{}, errors.New("secrets descriptor missing")
	}
	defer file.Close()
	var sec secrets
	if err := json.NewDecoder(file).Decode(&sec); err != nil {
		return secrets{}, fmt.Errorf("read secrets: %w", err)
	}
	if sec.GitHubToken == "" || sec.WToken == "" {
		return secrets{}, errors.New("incomplete secrets")
	}
	return sec, nil
}

func deriveMode(mode string) (class policyClass, readonlyWorkdir, throwawayHome, ok bool) {
	switch mode {
	case "browse":
		return classRO, true, true, true
	case "local":
		return classRO, false, true, true
	case "publish":
		return classW, false, false, true
	}
	return "", false, false, false
}

func recvRequest(conn *net.UnixConn) (launchRequest, []*os.File, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return launchRequest{}, nil, err
	}
	buf := make([]byte, 65536)
	var firstN int
	var rawFDs []int
	var opErr error
	if err := raw.Read(func(fd uintptr) bool {
		firstN, rawFDs, opErr = recvmsgWithFds(int(fd), buf)
		return opErr != unix.EAGAIN
	}); err != nil {
		return launchRequest{}, nil, err
	}
	files := wrapFDs(rawFDs)
	if opErr != nil {
		return launchRequest{}, files, opErr
	}
	if firstN < 4 {
		return launchRequest{}, files, errors.New("short control frame")
	}
	data := append([]byte(nil), buf[:firstN]...)
	total := binary.BigEndian.Uint32(data[:4])
	if total > 256<<10 {
		return launchRequest{}, files, errors.New("control frame too large")
	}
	for uint32(len(data)-4) < total {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		chunk := make([]byte, int(4+total)-len(data))
		n, err := conn.Read(chunk)
		if n > 0 {
			data = append(data, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}
	if uint32(len(data)-4) < total {
		return launchRequest{}, files, errors.New("truncated control frame")
	}
	var req launchRequest
	if err := json.Unmarshal(data[4:4+total], &req); err != nil {
		return launchRequest{}, files, err
	}
	return req, files, nil
}

func recvmsgWithFds(fd int, buf []byte) (int, []int, error) {
	oob := make([]byte, unix.CmsgSpace(3*4))
	n, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, 0)
	if err != nil {
		return 0, nil, err
	}
	var fds []int
	if oobn > 0 {
		if scms, e := unix.ParseSocketControlMessage(oob[:oobn]); e == nil {
			for _, scm := range scms {
				if got, e2 := unix.ParseUnixRights(&scm); e2 == nil {
					fds = append(fds, got...)
				}
			}
		}
	}
	return n, fds, nil
}

func wrapFDs(fds []int) []*os.File {
	files := make([]*os.File, len(fds))
	for i, fd := range fds {
		files[i] = os.NewFile(uintptr(fd), "stdio")
	}
	return files
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		if f != nil {
			f.Close()
		}
	}
}

// watchClose reports when the peer closes the control connection, which is how
// the tool signals cancellation or timeout.
func watchClose(conn *net.UnixConn) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		buf := make([]byte, 1)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
	return ch
}

func waitExit(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 1
}

func netnsAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWNET,
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN, unix.CAP_SYS_ADMIN},
		Pdeathsig:   syscall.SIGKILL,
	}
}

func commandAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWNS | unix.CLONE_NEWPID,
		AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN},
		Pdeathsig:   syscall.SIGKILL,
	}
}

func boolArg(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("decode argument: %w", err)
	}
	return string(b), nil
}

// commandBaseEnv is the environment commands inherit: the user environment with
// pi-square internals, inherited GitHub credentials, and inherited proxy
// settings removed. Networking and credentials are added back per command.
func commandBaseEnv(env []string) []string {
	env = stripSupervisorEnv(env)
	env = unsetEnv(env, githubCredentialVars...)
	env = unsetEnv(env, proxyVars...)
	// SSH-agent forwarding is out of scope for this API-only runner.
	env = unsetEnv(env, "SSH_AUTH_SOCK")
	return env
}

func minimalEnv(env []string) []string {
	return stripSupervisorEnv(env)
}

func stripSupervisorEnv(env []string) []string {
	out := env[:0]
	for _, item := range env {
		if hasPrefix(item, "PI_SQUARE_") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

type jobTracker struct {
	mu   sync.Mutex
	next int
	jobs map[int]*exec.Cmd
}

func newJobTracker() *jobTracker {
	return &jobTracker{jobs: map[int]*exec.Cmd{}}
}

func (t *jobTracker) add(class policyClass, cmd *exec.Cmd) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	id := t.next
	t.jobs[id] = cmd
	return id
}

func (t *jobTracker) remove(id int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.jobs, id)
}

func (t *jobTracker) killAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, cmd := range t.jobs {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}
