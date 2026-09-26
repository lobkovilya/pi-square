//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegration drives real commands through the supervisor, network
// namespaces, and gateway to GitHub.com. It is opt-in because it needs
// unprivileged user namespaces, an authenticated host gh, and network access:
//
//	PI_SQUARE_INTEGRATION=1 go test -run TestIntegration -v
//
// It performs no writes; every scenario is a read or an expected denial.
func TestIntegration(t *testing.T) {
	if os.Getenv("PI_SQUARE_INTEGRATION") != "1" {
		t.Skip("set PI_SQUARE_INTEGRATION=1 to run the live isolation and gateway tests")
	}

	bin := filepath.Join(t.TempDir(), "pi-square")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}
	// Socket paths are length-limited, so the instance directory needs a short
	// parent; the test must not touch or leave behind the user's real instance.
	runtimeDir, err := os.MkdirTemp("/tmp", "pi-square-itest-")
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "XDG_RUNTIME_DIR="+runtimeDir)
	t.Cleanup(func() {
		stop := exec.Command(bin, "gateway", "stop", "default", "--force")
		stop.Env = env
		if out, err := stop.CombinedOutput(); err != nil {
			t.Logf("stop gateway: %v: %s", err, out)
		}
		os.RemoveAll(runtimeDir)
	})

	selftest := func(t *testing.T, profile, command string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin)
		cmd.Env = append(env,
			"PI_SQUARE_SELFTEST_PROFILE="+profile,
			"PI_SQUARE_SELFTEST_CMD="+command,
		)
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
		return string(out), code
	}

	cases := []struct {
		name       string
		profile    string
		command    string
		wantSubstr string
		wantAbsent string
	}{
		{"browse rest read", "browse", "gh api rate_limit -q .rate.limit && echo RATE_OK", "RATE_OK", ""},
		{"browse rest write denied", "browse", "gh api -X POST /user/repos -f name=x 2>&1; true", "write_requires_publish", ""},
		{"browse graphql query", "browse", `gh api graphql -f query='query{viewer{login}}' -q .data.viewer.login && echo QUERY_OK`, "QUERY_OK", ""},
		{"browse graphql mutation denied", "browse", `gh api graphql -f query='mutation{__typename}' 2>&1; true`, "write_requires_publish", ""},
		{"publish graphql mutation allowed", "publish", `gh api graphql -f query='mutation{__typename}' -q .data.__typename`, "Mutation", "write_requires_publish"},
		{"browse non-github passthrough", "browse", `curl -s -m 10 https://example.com >/dev/null && echo REACHED || echo BLOCKED`, "REACHED", "BLOCKED"},
		{"browse proxy bypass blocked", "browse", `curl -s -m 10 --noproxy '*' https://140.82.112.3 >/dev/null && echo REACHED || echo BLOCKED`, "BLOCKED", "REACHED"},
		{"browse workdir readonly", "browse", `touch ./__pi_square_itest 2>&1 && echo WROTE || echo READONLY`, "READONLY", "WROTE"},
		{"local workdir writable", "local", `touch ./__pi_square_itest && echo WROTE && rm -f ./__pi_square_itest`, "WROTE", ""},
		{"local rest write denied", "local", "gh api -X POST /user/repos -f name=x 2>&1; true", "write_requires_publish", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := selftest(t, tc.profile, tc.command)
			if tc.wantSubstr != "" && !strings.Contains(out, tc.wantSubstr) {
				t.Fatalf("output missing %q:\n%s", tc.wantSubstr, out)
			}
			if tc.wantAbsent != "" && strings.Contains(out, tc.wantAbsent) {
				t.Fatalf("output unexpectedly contains %q:\n%s", tc.wantAbsent, out)
			}
		})
	}
}
