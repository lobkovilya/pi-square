//go:build linux

package main

import (
	"encoding/json"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGatewayInstances(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "Gateway instances") }

var _ = Describe("gateway instances", func() {
	var bin, runtimeDir, ghDir, tokenFile, callsFile string
	var env []string
	command := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	BeforeEach(func() {
		// given
		tmp := GinkgoT().TempDir()
		bin = filepath.Join(tmp, "pi-square")
		build := exec.Command("go", "build", "-o", bin, ".")
		output, err := build.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(output))
		runtimeDir = filepath.Join(tmp, "runtime")
		Expect(os.Mkdir(runtimeDir, 0700)).To(Succeed())
		ghDir = filepath.Join(tmp, "bin")
		Expect(os.Mkdir(ghDir, 0700)).To(Succeed())
		tokenFile = filepath.Join(tmp, "token")
		callsFile = filepath.Join(tmp, "calls")
		Expect(os.WriteFile(tokenFile, []byte("first"), 0600)).To(Succeed())
		script := "#!/bin/sh\necho call >> '" + callsFile + "'\ncat '" + tokenFile + "'\n"
		Expect(os.WriteFile(filepath.Join(ghDir, "gh"), []byte(script), 0700)).To(Succeed())
		// Named-instance validation runs before pi, but the launcher first
		// resolves HOME and locates pi. Nix builders have /homeless-shelter
		// as HOME, which does not exist in the sandbox.
		Expect(os.WriteFile(filepath.Join(ghDir, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0700)).To(Succeed())
		env = append(os.Environ(), "HOME="+tmp, "XDG_RUNTIME_DIR="+runtimeDir, "PATH="+ghDir+":"+os.Getenv("PATH"))
		DeferCleanup(func() {
			command("gateway", "stop", "default", "--force")
			command("gateway", "stop", "named", "--force")
		})
	})
	It("uses the supervisor ingress socket names", func() {
		// given
		dir := GinkgoT().TempDir()

		// when
		p := instancePathsAt(dir)

		// then
		Expect(p.ro).To(Equal(filepath.Join(dir, filepath.Base(roSocket))))
		Expect(p.rw).To(Equal(filepath.Join(dir, filepath.Base(wSocket))))
	})
	DescribeTable("classifies probe failures",
		func(err error, expectedState instanceState, expectedReason string) {
			// given
			probeErr := err

			// when
			state, reason := classifyProbe(probeErr)

			// then
			Expect(state).To(Equal(expectedState))
			Expect(reason).To(Equal(expectedReason))
		},
		Entry("missing control socket", &net.OpError{Op: "dial", Net: "unix", Err: fs.ErrNotExist}, stateStopped, ""),
		Entry("refused control socket", &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}, stateUnhealthy, "stale control socket, daemon not listening"),
		Entry("timed out health check", &net.OpError{Op: "read", Net: "unix", Err: os.ErrDeadlineExceeded}, stateUnhealthy, "health check timed out"),
		Entry("protocol mismatch", errProtocolMismatch, stateUnhealthy, "protocol mismatch"),
	)
	It("serializes simultaneous default starts and retains its credential and CA until restart", func() {
		// given
		var wg sync.WaitGroup
		results := make(chan error, 8)

		// when
		for range 8 {
			wg.Add(1)
			go func() { defer wg.Done(); _, err := command("gateway", "start", "default"); results <- err }()
		}
		wg.Wait()
		close(results)

		// then
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			}
		}
		Expect(successes).To(Equal(1))
		Expect(os.ReadFile(callsFile)).To(HaveLen(len("call\n")))
		p, err := pathsForWithRuntime("default", runtimeDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(p.ro).To(BeAnExistingFile())
		Expect(p.rw).To(BeAnExistingFile())
		Expect(p.log).To(BeAnExistingFile())
		c, first, err := probeInstance(p, "attach", false)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.CA).To(ContainSubstring("BEGIN CERTIFICATE"))
		Expect(first.Active).To(Equal(1))
		_, err = command("gateway", "stop", "default")
		Expect(err).To(HaveOccurred())
		c.Close()
		Expect(os.WriteFile(tokenFile, []byte("second"), 0600)).To(Succeed())
		other, again, err := probeInstance(p, "attach", false)
		Expect(err).NotTo(HaveOccurred())
		other.Close()
		Expect(again.CA).To(Equal(first.CA))
		Expect(os.ReadFile(callsFile)).To(HaveLen(len("call\n")))

		// and then
		_, err = command("gateway", "stop", "default")
		Expect(err).NotTo(HaveOccurred())
		_, err = command("gateway", "start", "default")
		Expect(err).NotTo(HaveOccurred())
		fresh, rotated, err := probeInstance(p, "health", false)
		Expect(err).NotTo(HaveOccurred())
		fresh.Close()
		Expect(rotated.CA).To(BeEmpty())
		fresh, rotated, err = probeInstance(p, "attach", false)
		Expect(err).NotTo(HaveOccurred())
		fresh.Close()
		Expect(rotated.CA).NotTo(Equal(first.CA))
		Expect(os.ReadFile(callsFile)).To(HaveLen(2 * len("call\n")))
	})
	It("reports stopped gateways without a socket error", func() {
		// given
		_, err := command("gateway", "start", "default")
		Expect(err).NotTo(HaveOccurred())
		_, err = command("gateway", "stop", "default")
		Expect(err).NotTo(HaveOccurred())

		// when
		output, err := command("gateway", "list")

		// then
		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(Equal("default\tstopped\n"))
		Expect(output).NotTo(ContainSubstring("unhealthy"))
		Expect(output).NotTo(ContainSubstring("dial unix"))

		// and then
		p, err := pathsForWithRuntime("other", runtimeDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.MkdirAll(p.dir, 0700)).To(Succeed())
		output, err = command("gateway", "list")
		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(Equal("default\tstopped\nother\tstopped\n"))
	})
	It("rejects missing named gateways, checks actual health, and clears stale sockets", func() {
		// given
		missing, missingErr := command("--gateway=named", "--", "--version")
		Expect(missingErr).To(HaveOccurred())
		Expect(missing).To(ContainSubstring("does not exist"))
		p, err := pathsForWithRuntime("named", runtimeDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.MkdirAll(p.dir, 0700)).To(Succeed())
		Expect(os.WriteFile(p.management, []byte("stale"), 0600)).To(Succeed())

		// when
		output, err := command("gateway", "list")

		// then
		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(ContainSubstring("named\tunhealthy"))
		Expect(output).NotTo(ContainSubstring("dial unix"))
		output, err = command("--gateway=named", "--", "--version")
		Expect(err).To(HaveOccurred())
		Expect(output).To(ContainSubstring("not healthy"))
		_, err = command("gateway", "start", "named")
		Expect(err).NotTo(HaveOccurred())
		output, err = command("gateway", "list")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(output)).To(ContainSubstring("named\thealthy"))
	})
	It("recovers from a crashed daemon without trusting stale sockets", func() {
		// given
		_, err := command("gateway", "start", "default")
		Expect(err).NotTo(HaveOccurred())
		p, err := pathsForWithRuntime("default", runtimeDir)
		Expect(err).NotTo(HaveOccurred())
		conn, first, err := probeInstance(p, "attach", false)
		Expect(err).NotTo(HaveOccurred())
		raw, err := conn.(*net.UnixConn).SyscallConn()
		Expect(err).NotTo(HaveOccurred())
		var pid int
		Expect(raw.Control(func(fd uintptr) {
			peer, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
			Expect(e).NotTo(HaveOccurred())
			pid = int(peer.Pid)
		})).To(Succeed())

		// when
		Expect(syscall.Kill(pid, syscall.SIGKILL)).To(Succeed())
		Eventually(func() error {
			c, _, e := probeInstance(p, "health", false)
			if c != nil {
				c.Close()
			}
			return e
		}).Should(HaveOccurred())
		conn.Close()
		output, err := command("gateway", "list")
		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(ContainSubstring("default\tunhealthy"))
		_, err = command("gateway", "start", "default")

		// then
		Expect(err).NotTo(HaveOccurred())
		fresh, next, err := probeInstance(p, "attach", false)
		Expect(err).NotTo(HaveOccurred())
		fresh.Close()
		Expect(next.CA).NotTo(Equal(first.CA))
	})
	It("rejects incompatible readiness handshakes", func() {
		// given
		p, err := pathsForWithRuntime("named", runtimeDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.MkdirAll(p.dir, 0700)).To(Succeed())
		listener, err := net.Listen("unix", p.management)
		Expect(err).NotTo(HaveOccurred())
		defer listener.Close()
		go func() {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			defer conn.Close()
			var request gatewayMessage
			json.NewDecoder(conn).Decode(&request)
			json.NewEncoder(conn).Encode(gatewayMessage{Version: gatewayProtocol + 1})
		}()

		// when
		conn, _, err := probeInstance(p, "health", false)

		// then
		Expect(conn).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("protocol mismatch")))
	})
	It("refuses stop with live attachments unless forced", func() {
		// given
		_, err := command("gateway", "start", "default")
		Expect(err).NotTo(HaveOccurred())
		p, err := pathsForWithRuntime("default", runtimeDir)
		Expect(err).NotTo(HaveOccurred())
		conn, _, err := probeInstance(p, "attach", false)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()

		// when
		output, err := command("gateway", "stop", "default")

		// then
		Expect(err).To(HaveOccurred())
		Expect(output).To(ContainSubstring("1 active session"))
		Expect(output).NotTo(ContainSubstring("not healthy"))
		_, err = command("gateway", "stop", "default", "--force")
		Expect(err).NotTo(HaveOccurred())
		_, _, err = probeInstance(p, "health", false)
		Expect(err).To(HaveOccurred())
	})
})

// Resolve paths without changing global environment during the suite.
func pathsForWithRuntime(name, runtimeDir string) (instancePaths, error) {
	p, err := pathsFor(name)
	if err != nil {
		return p, err
	}
	return instancePathsAt(filepath.Join(runtimeDir, "pi-square", "instances", name)), nil
}
