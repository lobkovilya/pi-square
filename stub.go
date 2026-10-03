//go:build linux

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// runStub is what every routed shell command actually execs. It runs inside
// pi's namespace, forwards the request and its standard descriptors to the
// supervisor over the private control socket, and exits with the command's
// status. The real command never runs here, so nothing it does can reach the
// control socket or the write capability.
func runStub(args []string) error {
	if len(args) != 2 {
		return errors.New("invalid stub invocation")
	}
	profile := args[0]
	command, err := decode(args[1])
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "/"
	}

	req := launchRequest{
		Profile: profile,
		Cwd:     cwd,
		Command: command,
		WToken:  os.Getenv("PI_SQUARE_WTOKEN"),
	}

	conn, err := net.Dial("unix", controlSocket)
	if err != nil {
		return fmt.Errorf("pi-square launcher is unavailable: %w", err)
	}
	defer conn.Close()
	unixConn := conn.(*net.UnixConn)

	if err := sendRequest(unixConn, req); err != nil {
		return fmt.Errorf("submit command to launcher: %w", err)
	}

	var resp launchResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("launcher did not respond: %w", err)
	}
	if !resp.Started {
		fmt.Fprintf(os.Stderr, "pi-square: %s: %s\n", resp.Code, resp.Message)
		if resp.Code == denyWriteRequiresPublish {
			os.Exit(2)
		}
		os.Exit(1)
	}
	os.Exit(resp.Exit)
	return nil
}

func sendRequest(conn *net.UnixConn, req launchRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)

	rights := unix.UnixRights(int(os.Stdin.Fd()), int(os.Stdout.Fd()), int(os.Stderr.Fd()))
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := raw.Write(func(fd uintptr) bool {
		opErr = unix.Sendmsg(int(fd), frame, rights, nil, 0)
		return opErr != unix.EAGAIN
	}); err != nil {
		return err
	}
	return opErr
}
