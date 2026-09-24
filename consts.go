//go:build linux

package main

import "strings"

// Internal re-exec markers. Each identifies a stage the binary runs as when it
// re-executes itself; none of them is an authorization on its own.
const (
	stageMarker       = "--pi-square-stage"
	gatewayMarker     = "--pi-square-gateway-daemon"
	netnsWorkerMarker = "--pi-square-netns-worker"
	commandMarker     = "--pi-square-command"
	stubMarker        = "--pi-square-stub"
	piStageMarker     = "--pi-square-pi"
)

// Inherited file descriptors. The stage receives its secrets on fd 3; the netns
// worker and command stages receive purpose-specific fds there instead.
const (
	secretsFD = 3
	readyFD   = 3
	netnsFD   = 3
)

// Paths materialized inside the restricted root.
const (
	runtimeDir    = "/run/pi-square"
	helperPath    = "/run/pi-square/pi-square"
	extensionPath = "/run/pi-square/gh-mode.ts"
	gatewayDir    = "/run/pi-square/gateway"
	roSocket      = "/run/pi-square/gateway/ro.sock"
	wSocket       = "/run/pi-square/gateway/w.sock"
	controlSocket = "/run/pi-square/control.sock"
	caBundlePath  = "/run/pi-square/ca-bundle.crt"
)

func gatewaySocketFor(class policyClass) string {
	if class == classW {
		return wSocket
	}
	return roSocket
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0]
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}

func unsetEnv(env []string, keys ...string) []string {
	out := env[:0]
	for _, item := range env {
		keep := true
		for _, key := range keys {
			if strings.HasPrefix(item, key+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, item)
		}
	}
	return out
}
