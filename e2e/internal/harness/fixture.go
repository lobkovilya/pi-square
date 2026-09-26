//go:build unix

package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	FakeHostToken     = "e2e-host-token-must-not-leak"
	DummyCommandToken = "pi-square-gateway-dummy"
)

var Modes = []string{"browse", "local", "publish"}

type Fixture struct {
	Root, Binary, Workdir, HostHome, HostSecret string
	buildDir, secretDir, runtimeDir             string
}

type SessionOptions struct {
	Cwd, Profile, Guard string
	Env                 []string
	Timeout             time.Duration
	Cleanup             func()
}

func ProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("cannot locate project go.mod")
		}
		dir = parent
	}
}

func NewFixture(ctx context.Context) (*Fixture, error) {
	root, err := ProjectRoot()
	if err != nil {
		return nil, err
	}
	buildDir, err := os.MkdirTemp("", "pi-square-build-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(buildDir) }
	binary := os.Getenv("PI_SQUARE_E2E_EXECUTABLE")
	if binary != "" {
		binary, err = filepath.Abs(binary)
		if err == nil {
			var st os.FileInfo
			st, err = os.Stat(binary)
			if err == nil && st.Mode()&0111 == 0 {
				err = fmt.Errorf("%s is not executable", binary)
			}
		}
	} else {
		binary = filepath.Join(buildDir, "pi-square")
		buildCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		r := RunProcess(buildCtx, "go", []string{"build", "-o", binary, "."}, root, CleanEnvironment(""))
		err = RequireSuccess("pi-square build", r)
	}
	if err != nil {
		cleanup()
		return nil, err
	}
	workdir, err := os.MkdirTemp("", "pi-square-pty-")
	if err != nil {
		cleanup()
		return nil, err
	}
	secretDir, err := os.MkdirTemp("", "pi-square-host-only-")
	if err != nil {
		cleanup()
		_ = os.RemoveAll(workdir)
		return nil, err
	}
	secret := filepath.Join(secretDir, "host-only-file")
	if err = os.WriteFile(secret, []byte("must not be visible inside the sandbox\n"), 0600); err != nil {
		cleanup()
		_ = os.RemoveAll(workdir)
		_ = os.RemoveAll(secretDir)
		return nil, err
	}
	runtimeDir, err := os.MkdirTemp("", "pi-square-runtime-")
	if err != nil {
		cleanup()
		_ = os.RemoveAll(workdir)
		_ = os.RemoveAll(secretDir)
		return nil, err
	}
	home, _ := os.UserHomeDir()
	home, _ = filepath.EvalSymlinks(home)
	return &Fixture{Root: root, Binary: binary, Workdir: workdir, HostHome: home, HostSecret: secret, buildDir: buildDir, secretDir: secretDir, runtimeDir: runtimeDir}, nil
}

func (f *Fixture) Close() error {
	// Gateways intentionally outlive sessions, so the fixture must stop its
	// isolated default instance before removing the binary and runtime state.
	cmd := exec.Command(f.Binary, "gateway", "stop", "default", "--force")
	cmd.Env = f.Environment("")
	_ = cmd.Run() // A suite that never launched pi has no gateway to stop.
	for _, dir := range []string{f.Workdir, f.secretDir, f.runtimeDir, f.buildDir} {
		_ = os.RemoveAll(dir)
	}
	return nil
}

func (f *Fixture) Environment(token string) []string {
	env := CleanEnvironment("")
	filtered := env[:0]
	for _, item := range env {
		if !strings.HasPrefix(item, "XDG_RUNTIME_DIR=") {
			filtered = append(filtered, item)
		}
	}
	if token == "" {
		token = FakeHostToken
	}
	return append(filtered, "GH_TOKEN="+token, "XDG_RUNTIME_DIR="+f.runtimeDir)
}

// SessionOptions is the sandbox configuration: fake host token, scratch workdir.
func (f *Fixture) SessionOptions(profile string) (SessionOptions, error) {
	return f.SessionOptionsAt(profile, "", f.Workdir)
}

// SessionOptions is the live configuration: real token, fixture checkout.
func (f *LiveFixture) SessionOptions(profile string) (SessionOptions, error) {
	return f.Fixture.SessionOptionsAt(profile, f.Token, f.Checkout)
}

func (f *Fixture) SessionOptionsAt(profile string, token string, cwd string) (SessionOptions, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return SessionOptions{}, err
	}
	base := filepath.Join(home, ".pi", "e2e-go")
	if err = os.MkdirAll(base, 0700); err != nil {
		return SessionOptions{}, err
	}
	agent, err := os.MkdirTemp(base, "agent-")
	if err != nil {
		return SessionOptions{}, err
	}
	fail := func(e error) (SessionOptions, error) { _ = os.RemoveAll(agent); return SessionOptions{}, e }
	guard := filepath.Join(agent, "no-model.ts")
	data, err := os.ReadFile(filepath.Join(f.Root, "e2e", "testdata", "no-model.ts"))
	if err != nil {
		return fail(err)
	}
	if err = os.WriteFile(guard, data, 0600); err != nil {
		return fail(err)
	}
	if err = os.WriteFile(filepath.Join(agent, "settings.json"), []byte(`{"quietStartup":true}`), 0600); err != nil {
		return fail(err)
	}
	env := append(f.Environment(token), "PI_CODING_AGENT_DIR="+agent)
	return SessionOptions{Cwd: cwd, Profile: profile, Guard: guard, Env: env, Cleanup: func() { _ = os.RemoveAll(agent) }}, nil
}

func (f *Fixture) Command(profile *string) *exec.Cmd {
	args := []string{}
	if profile != nil {
		args = append(args, "--profile="+*profile)
	}
	args = append(args, "--")
	return exec.Command(f.Binary, args...)
}
