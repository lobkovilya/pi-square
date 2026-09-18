//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// setupRestrictedRoot builds the pivoted filesystem the supervisor, pi, and all
// commands share: a tmpfs root with a small set of read-only system mounts, a
// writable workdir, and pi's state. It stops after pivot_root; it does not drop
// capabilities, because the supervisor keeps running inside this root.
func setupRestrictedRoot(root, workdir, home, fff, runtimeXDG string) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	if err := unix.Mount("tmpfs", root, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount sandbox root: %w", err)
	}

	for _, p := range []string{"/nix/store", "/etc", "/run/current-system", "/run/wrappers", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/usr"} {
		if err := bind(root, p, p, true, false); err != nil && !os.IsNotExist(err) {
			return err
		}
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
	if err := writeExtension(root); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find pi-square executable: %w", err)
	}
	if err := bind(root, self, helperPath, true, false); err != nil {
		return fmt.Errorf("bind pi-square helper: %w", err)
	}

	// Pi rewrites auth/session state.
	if err := bind(root, filepath.Join(home, ".pi"), filepath.Join(home, ".pi"), false, true); err != nil {
		return fmt.Errorf("bind ~/.pi: %w", err)
	}
	for _, p := range []string{".gitconfig", ".config/git", ".ssh/config", ".ssh/known_hosts", ".config/gh/config.yml"} {
		p = filepath.Join(home, p)
		if err := bind(root, p, p, true, false); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	if runtimeXDG == "" {
		runtimeXDG = "/run/user/" + fmt.Sprint(os.Getuid())
	}
	if err := mountTmpfs(target(root, runtimeXDG), "mode=0700"); err != nil {
		return err
	}

	// Keep this order: a project may be an ancestor of ~/.pi/agent/fff.
	if err := bind(root, workdir, workdir, false, true); err != nil {
		return fmt.Errorf("bind workdir: %w", err)
	}
	if err := bind(root, fff, filepath.Join(home, ".pi/agent/fff"), false, true); err != nil {
		return fmt.Errorf("bind fff database: %w", err)
	}

	if err := pivotInto(root, workdir); err != nil {
		return err
	}
	return nil
}

func pivotInto(root, workdir string) error {
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
	return nil
}

// setupCommandSandbox is run by the command stage after it has joined the class
// network namespace and created fresh mount and PID namespaces. It applies the
// per-command filesystem policy and hides the trusted runtime sockets, then the
// caller drops capabilities and execs the command.
func setupCommandSandbox(workdir, cwd string, readonlyWorkdir bool) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make command mounts private: %w", err)
	}
	// Fresh proc from this PID namespace so the command cannot inspect the
	// supervisor, the netns workers, or their namespace handles.
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount private proc: %w", err)
	}
	if readonlyWorkdir {
		if err := protectWorkdir(workdir); err != nil {
			return err
		}
	}
	// Keep the control channel and gateway ingress sockets out of reach. A
	// command that reached them could try to launch further work or a write
	// tunnel; only the trusted stub in pi's namespace may talk to them.
	if err := mountTmpfs(gatewayDir, "mode=0000"); err != nil {
		return fmt.Errorf("mask gateway sockets: %w", err)
	}
	if err := hidePath(controlSocket); err != nil {
		return fmt.Errorf("mask control socket: %w", err)
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("enter command cwd: %w", err)
	}
	return nil
}

func protectWorkdir(workdir string) error {
	if err := unix.Mount(workdir, workdir, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("protect workdir %s: %w", workdir, err)
	}
	attr := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}
	if err := unix.MountSetattr(unix.AT_FDCWD, workdir, unix.AT_RECURSIVE, attr); err != nil {
		return fmt.Errorf("make workdir %s read-only: %w", workdir, err)
	}
	return nil
}

// hidePath masks a single file with /dev/null so its guessable pathname stops
// being usable even though the name is known.
func hidePath(path string) error {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("hide %s: is a directory", path)
	}
	if err := unix.Mount("/dev/null", path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("hide %s: %w", path, err)
	}
	return nil
}

func writeExtension(root string) error {
	path := target(root, extensionPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create extension directory: %w", err)
	}
	if err := os.WriteFile(path, ghModeExtension, 0444); err != nil {
		return fmt.Errorf("write bundled extension: %w", err)
	}
	return nil
}

func dropCapabilities() error {
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
