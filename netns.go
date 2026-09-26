//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// frontendPort is fixed. Each network namespace has its own loopback, so both
// class frontends can listen on the same address without colliding, and the
// command environment never reveals which class it belongs to.
const frontendListen = "127.0.0.1:8877"

// loopbackUp enables the loopback interface in the current network namespace.
// A fresh namespace ships lo administratively down, so 127.0.0.1 is unreachable
// until this runs.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open control socket: %w", err)
	}
	defer unix.Close(fd)

	var ifr struct {
		Name  [unix.IFNAMSIZ]byte
		Flags uint16
		_     [22]byte
	}
	copy(ifr.Name[:], "lo")
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return fmt.Errorf("get lo flags: %w", e)
	}
	ifr.Flags |= unix.IFF_UP | unix.IFF_RUNNING
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return fmt.Errorf("set lo up: %w", e)
	}
	return nil
}

// runFrontend is the netns worker entrypoint. It is cloned into a fresh network
// namespace by the supervisor, brings up loopback, and relays every accepted
// loopback connection to its fixed gateway ingress socket. It holds no
// credentials and cannot choose a policy or a different upstream.
func runFrontend(gatewaySocket string) error {
	if err := loopbackUp(); err != nil {
		return fmt.Errorf("bring up loopback: %w", err)
	}
	if gatewaySocket == "offline" {
		ready := os.NewFile(readyFD, "ready")
		ready.Write([]byte{'1'})
		ready.Close()
		for {
			time.Sleep(time.Hour)
		}
	}
	listener, err := net.Listen("tcp", frontendListen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", frontendListen, err)
	}

	// Signal readiness to the supervisor on the inherited pipe, then close it.
	if ready := os.NewFile(readyFD, "ready"); ready != nil {
		ready.Write([]byte{'1'})
		ready.Close()
	}

	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}
		go relay(client, gatewaySocket)
	}
}

func relay(client net.Conn, gatewaySocket string) {
	defer client.Close()
	upstream, err := net.Dial("unix", gatewaySocket)
	if err != nil {
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); closeWrite(upstream); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); closeWrite(client); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
