//go:build linux

package main

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

//go:embed gh-mode.ts
var ghModeExtension []byte

type slot struct {
	dir  string
	lock *os.File
}

func main() {
	switch {
	case len(os.Args) >= 3 && os.Args[1] == stageMarker && os.Getenv("PI_SQUARE_STAGE_TOKEN") == os.Args[2]:
		exitOnError(stage(os.Args[3:]))
	case len(os.Args) == 3 && os.Args[1] == gatewayMarker:
		exitOnError(runGatewayDaemon(os.Args[2]))
	case len(os.Args) >= 2 && os.Args[1] == piStageMarker:
		exitOnError(runPiStage(os.Args[2:]))
	case len(os.Args) >= 3 && os.Args[1] == netnsWorkerMarker:
		exitOnError(runFrontend(os.Args[2]))
	case len(os.Args) >= 2 && os.Args[1] == commandMarker:
		exitOnError(commandStage(os.Args[2:]))
	case len(os.Args) >= 2 && os.Args[1] == stubMarker:
		exitOnError(runStub(os.Args[2:]))
	default:
		exitOnError(runCLI(os.Args[1:], os.Stdout, run))
	}
}

func exitOnError(err error) {
	if err == nil {
		return
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	fatal(err)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "pi-square: %v\n", err)
	os.Exit(1)
}

func run(args []string, devMode bool, initialGHMode, gatewayName string, explicitGateway bool) error {
	workdir, err := filepath.EvalSymlinks(mustAbs("."))
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	home, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	if err != nil {
		return fmt.Errorf("resolve HOME: %w", err)
	}
	if workdir == "/" || workdir == home {
		return fmt.Errorf("refusing to sandbox %s; cd into a project first", workdir)
	}
	pi, err := exec.LookPath("pi")
	if err != nil {
		return errors.New("pi not found in PATH")
	}
	pi, err = filepath.Abs(pi)
	if err != nil {
		return err
	}

	attachment, caPEM, paths, err := attachInstance(gatewayName, !explicitGateway)
	if err != nil {
		return err
	}
	defer attachment.Close()
	if caPEM == "" {
		return errors.New("gateway returned no CA certificate")
	}
	attachmentFile, err := attachment.(*net.UnixConn).File()
	if err != nil {
		return err
	}
	defer attachmentFile.Close()

	s, err := claimFFF(workdir)
	if err != nil {
		return err
	}
	defer s.lock.Close()

	root, err := os.MkdirTemp("", ".pi-square-root-")
	if err != nil {
		return fmt.Errorf("create sandbox root: %w", err)
	}
	defer os.RemoveAll(root)

	stageToken, err := randomToken()
	if err != nil {
		return err
	}
	wToken, err := randomToken()
	if err != nil {
		return err
	}

	env := sanitizeParentEnv(os.Environ())
	devModeValue := "0"
	if devMode {
		devModeValue = "1"
	}
	for key, value := range map[string]string{
		"PI_SQUARE_STAGE_TOKEN":     stageToken,
		"PI_SQUARE_ROOT":            root,
		"PI_SQUARE_WORKDIR":         workdir,
		"PI_SQUARE_HOME":            home,
		"PI_SQUARE_PI":              pi,
		"PI_SQUARE_FFF":             s.dir,
		"PI_SQUARE_HOST_UID":        strconv.Itoa(os.Getuid()),
		"PI_SQUARE_DEV_MODE":        devModeValue,
		"PI_SQUARE_INITIAL_GH_MODE": initialGHMode,
		"PI_SQUARE_GATEWAY_DIR":     paths.dir,
	} {
		env = setEnv(env, key, value)
	}

	secretsReader, secretsWriter, err := os.Pipe()
	if err != nil {
		return err
	}

	cmd := exec.Command("/proc/self/exe", append([]string{stageMarker, stageToken}, args...)...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{secretsReader, attachmentFile}
	attr := namespaceAttr(unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP)
	attr.Pdeathsig = syscall.SIGKILL
	cmd.SysProcAttr = attr

	if err := cmd.Start(); err != nil {
		secretsReader.Close()
		secretsWriter.Close()
		return fmt.Errorf("start sandbox (are unprivileged user namespaces enabled?): %w", err)
	}
	secretsReader.Close()

	payload, err := json.Marshal(secrets{
		GatewayCA:   caPEM,
		WToken:      wToken,
		GitIdentity: hostGitIdentity(),
	})
	if err != nil {
		secretsWriter.Close()
		cmd.Process.Kill()
		return err
	}
	secretsWriter.Write(payload)
	secretsWriter.Close()

	if err := cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit
		}
		return fmt.Errorf("sandbox failed: %w", err)
	}
	return nil
}

// namespaceAttr keeps the two management capabilities the supervisor needs
// (creating namespaces, bringing up loopback, dropping capabilities) across the
// re-exec into the stage. The child keeps its real uid, so without ambient caps
// the exec would strip everything.
func namespaceAttr(cloneFlags uintptr) *syscall.SysProcAttr {
	uid, gid := os.Getuid(), os.Getgid()
	return &syscall.SysProcAttr{
		Cloneflags:                 cloneFlags,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SETPCAP, unix.CAP_NET_ADMIN},
	}
}

func claimFFF(workdir string) (*slot, error) {
	sum := sha256.Sum256([]byte(workdir))
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(cache, "pi-square", "fff", hex.EncodeToString(sum[:8]))
	for i := 0; i < 8; i++ {
		dir := filepath.Join(base, strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return &slot{dir: dir, lock: lock}, nil
		}
		lock.Close()
	}
	return nil, errors.New("all 8 per-project fff database slots are busy")
}

func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}
