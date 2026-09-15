//go:build linux

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const stageMarker = "--pi-square-internal-stage"

type slot struct {
	dir  string
	lock *os.File
}

func main() {
	if len(os.Args) >= 3 && os.Args[1] == stageMarker && os.Getenv("PI_SQUARE_STAGE_TOKEN") == os.Args[2] {
		if err := sandbox(os.Args[3:]); err != nil {
			fatal(err)
		}
		return
	}
	if err := run(os.Args[1:]); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "pi-square: %v\n", err)
	os.Exit(1)
}

func run(args []string) error {
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

	s, err := claimFFF(workdir, home)
	if err != nil {
		return err
	}
	defer s.lock.Close()

	root, err := os.MkdirTemp("", ".pi-square-root-")
	if err != nil {
		return fmt.Errorf("create sandbox root: %w", err)
	}
	defer os.RemoveAll(root)

	tokenBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	uid, gid := os.Getuid(), os.Getgid()
	env := append(os.Environ(),
		"PI_SQUARE_STAGE_TOKEN="+token,
		"PI_SQUARE_ROOT="+root,
		"PI_SQUARE_WORKDIR="+workdir,
		"PI_SQUARE_HOME="+home,
		"PI_SQUARE_PI="+pi,
		"PI_SQUARE_FFF="+s.dir,
		"PI_SQUARE_HOST_UID="+strconv.Itoa(uid),
	)

	cmd := exec.Command("/proc/self/exe", append([]string{stageMarker, token}, args...)...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP,
		Pdeathsig:                  syscall.SIGKILL,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit
		}
		return fmt.Errorf("start sandbox (are unprivileged user namespaces enabled?): %w", err)
	}
	return nil
}

func sandbox(args []string) error {
	root := os.Getenv("PI_SQUARE_ROOT")
	workdir := os.Getenv("PI_SQUARE_WORKDIR")
	home := os.Getenv("PI_SQUARE_HOME")
	pi := os.Getenv("PI_SQUARE_PI")
	fff := os.Getenv("PI_SQUARE_FFF")
	if root == "" || workdir == "" || home == "" || pi == "" || fff == "" {
		return errors.New("invalid internal stage")
	}

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	if err := unix.Mount("tmpfs", root, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount sandbox root: %w", err)
	}

	for _, p := range []string{"/nix/store", "/etc", "/run/current-system", "/bin"} {
		if err := bind(root, p, p, true, false); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := bind(root, "/run/wrappers", "/run/wrappers", true, false); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := mountProc(root); err != nil {
		return err
	}
	if err := mountDev(root); err != nil {
		return err
	}
	if err := mountTmpfs(target(root, "/tmp"), "mode=1777"); err != nil {
		return err
	}

	// Pi rewrites auth/session state and gh rewrites its authentication files.
	if err := bind(root, filepath.Join(home, ".pi"), filepath.Join(home, ".pi"), false, true); err != nil {
		return fmt.Errorf("bind ~/.pi: %w", err)
	}
	for _, p := range []string{filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config/git"), filepath.Join(home, ".ssh")} {
		if err := bind(root, p, p, true, false); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	gh := filepath.Join(home, ".config/gh")
	if err := bind(root, gh, gh, false, true); err != nil && !os.IsNotExist(err) {
		return err
	}

	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = "/run/user/" + os.Getenv("PI_SQUARE_HOST_UID")
	}
	if err := mountTmpfs(target(root, runtimeDir), "mode=0700"); err != nil {
		return err
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if st, err := os.Stat(sock); err == nil && st.Mode()&os.ModeSocket != 0 {
			if err := bind(root, sock, sock, true, false); err != nil {
				return err
			}
		}
	}

	// Keep this order: a project may be an ancestor of ~/.pi/agent/fff.
	if err := bind(root, workdir, workdir, false, true); err != nil {
		return fmt.Errorf("bind workdir: %w", err)
	}
	if err := bind(root, fff, filepath.Join(home, ".pi/agent/fff"), false, true); err != nil {
		return fmt.Errorf("bind fff database: %w", err)
	}

	oldroot := filepath.Join(root, ".oldroot")
	if err := os.Mkdir(oldroot, 0700); err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", ".oldroot"); err != nil {
		return fmt.Errorf("pivot root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Unmount("/.oldroot", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach host root: %w", err)
	}
	_ = os.Remove("/.oldroot")
	if err := os.Chdir(workdir); err != nil {
		return fmt.Errorf("chdir workdir: %w", err)
	}

	if err := dropCapabilities(); err != nil {
		return fmt.Errorf("drop capabilities: %w", err)
	}
	argv := append([]string{"pi"}, args...)
	return unix.Exec(pi, argv, cleanEnv(os.Environ()))
}

func dropCapabilities() error {
	// Prevent namespace-root special handling from restoring capabilities on exec.
	if err := unix.Prctl(unix.PR_SET_SECUREBITS, uintptr(1<<0|1<<1), 0, 0, 0); err != nil {
		return err
	}
	for capability := 0; capability <= 63; capability++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0); err != nil && err != unix.EINVAL {
			return err
		}
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return err
	}
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

func bind(root, source, destination string, readonly, recursive bool) error {
	st, err := os.Stat(source)
	if err != nil {
		return err
	}
	dst := target(root, destination)
	if st.IsDir() {
		if err := os.MkdirAll(dst, 0755); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE, 0600)
		if err != nil {
			return err
		}
		f.Close()
	}
	flags := uintptr(unix.MS_BIND)
	if recursive && st.IsDir() {
		flags |= unix.MS_REC
	}
	if err := unix.Mount(source, dst, "", flags, ""); err != nil {
		return fmt.Errorf("bind %s: %w", source, err)
	}
	attr := &unix.MountAttr{}
	if readonly {
		attr.Attr_set = unix.MOUNT_ATTR_RDONLY
	} else {
		attr.Attr_clr = unix.MOUNT_ATTR_RDONLY
	}
	setFlags := uint(0)
	if recursive && st.IsDir() {
		setFlags = unix.AT_RECURSIVE
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, dst, setFlags, attr); err != nil {
		return fmt.Errorf("set mount permissions on %s: %w", source, err)
	}
	return nil
}

func mountProc(root string) error {
	dst := target(root, "/proc")
	if err := os.MkdirAll(dst, 0555); err != nil {
		return err
	}
	if err := unix.Mount("proc", dst, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount proc: %w", err)
	}
	return nil
}

func mountDev(root string) error {
	dst := target(root, "/dev")
	if err := mountTmpfs(dst, "mode=0755"); err != nil {
		return err
	}
	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		src := "/dev/" + name
		if err := bind(root, src, src, false, false); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_ = os.Symlink("/proc/self/fd", filepath.Join(dst, "fd"))
	_ = os.Symlink("/proc/self/fd/0", filepath.Join(dst, "stdin"))
	_ = os.Symlink("/proc/self/fd/1", filepath.Join(dst, "stdout"))
	_ = os.Symlink("/proc/self/fd/2", filepath.Join(dst, "stderr"))
	return nil
}

func mountTmpfs(dst, options string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", dst, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, options); err != nil {
		return fmt.Errorf("mount tmpfs at %s: %w", dst, err)
	}
	return nil
}

func target(root, absolute string) string {
	return filepath.Join(root, strings.TrimPrefix(filepath.Clean(absolute), "/"))
}

func claimFFF(workdir, home string) (*slot, error) {
	sum := sha256.Sum256([]byte(workdir))
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		cache = filepath.Join(home, ".cache")
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

func cleanEnv(env []string) []string {
	out := env[:0]
	for _, item := range env {
		if !strings.HasPrefix(item, "PI_SQUARE_STAGE_TOKEN=") && !strings.HasPrefix(item, "PI_SQUARE_ROOT=") && !strings.HasPrefix(item, "PI_SQUARE_WORKDIR=") && !strings.HasPrefix(item, "PI_SQUARE_HOME=") && !strings.HasPrefix(item, "PI_SQUARE_PI=") && !strings.HasPrefix(item, "PI_SQUARE_FFF=") && !strings.HasPrefix(item, "PI_SQUARE_HOST_UID=") {
			out = append(out, item)
		}
	}
	return out
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}
