//go:build linux

package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRunCLI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tt := range []struct {
		name         string
		args         []string
		wantArgs     []string
		wantOutput   string
		wantError    bool
		wantLaunch   bool
		wantDev      bool
		wantProfile  string
		wantGateway  string
		wantExplicit bool
	}{
		{name: "default", wantLaunch: true},
		{name: "empty separator", args: []string{"--"}, wantArgs: []string{}, wantLaunch: true},
		{name: "wrapper version", args: []string{"--version"}, wantOutput: "pi-square " + version + "\n"},
		{name: "help", args: []string{"--help"}, wantOutput: "Usage: pi-square"},
		{name: "short help", args: []string{"-h"}, wantOutput: "Usage: pi-square"},
		{name: "pi version", args: []string{"--", "--version"}, wantArgs: []string{"--version"}, wantLaunch: true},
		{name: "pi help", args: []string{"--", "--help"}, wantArgs: []string{"--help"}, wantLaunch: true},
		{name: "preserve arguments", args: []string{"--", "--model", "model", "a prompt", "--", ""}, wantArgs: []string{"--model", "model", "a prompt", "--", ""}, wantLaunch: true},
		{name: "dev mode", args: []string{"--mode=dev", "--", "prompt"}, wantArgs: []string{"prompt"}, wantLaunch: true, wantDev: true},
		{name: "publish profile", args: []string{"--profile=publish", "--", "prompt"}, wantArgs: []string{"prompt"}, wantLaunch: true, wantProfile: "publish"},
		{name: "named gateway", args: []string{"--gateway=team", "--", "prompt"}, wantArgs: []string{"prompt"}, wantLaunch: true, wantGateway: "team", wantExplicit: true},
		{name: "explicit default", args: []string{"--gateway=default"}, wantLaunch: true, wantGateway: "default", wantExplicit: true},
		{name: "invalid gateway", args: []string{"--gateway=../bad"}, wantError: true},
		{name: "invalid mode", args: []string{"--mode=publish"}, wantError: true},
		{name: "invalid profile", args: []string{"--profile=dev"}, wantError: true},
		{name: "unknown wrapper flag", args: []string{"--model", "model"}, wantError: true},
		{name: "prompt without separator", args: []string{"a prompt"}, wantError: true},
		{name: "positional before separator", args: []string{"prompt", "--", "--version"}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			launched := false
			err := runCLI(tt.args, &out, func(args []string, devMode bool, profile, gateway string, explicit bool, _ configuration) error {
				launched = true
				if !reflect.DeepEqual(args, tt.wantArgs) {
					t.Errorf("args = %#v, want %#v", args, tt.wantArgs)
				}
				if devMode != tt.wantDev {
					t.Errorf("devMode = %v, want %v", devMode, tt.wantDev)
				}
				if profile != tt.wantProfile {
					t.Errorf("profile = %q, want %q", profile, tt.wantProfile)
				}
				wantGateway := tt.wantGateway
				if wantGateway == "" {
					wantGateway = "default"
				}
				if gateway != wantGateway || explicit != tt.wantExplicit {
					t.Errorf("gateway = %q explicit=%v", gateway, explicit)
				}
				return nil
			})
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError = %v", err, tt.wantError)
			}
			if tt.wantError && tt.name != "invalid mode" && tt.name != "invalid profile" && tt.name != "invalid gateway" && !strings.Contains(err.Error(), "after --") {
				t.Errorf("error lacks separator guidance: %v", err)
			}
			if launched != tt.wantLaunch {
				t.Errorf("launched = %v, want %v", launched, tt.wantLaunch)
			}
			if tt.wantOutput == "" && out.Len() != 0 || !strings.HasPrefix(out.String(), tt.wantOutput) {
				t.Errorf("output = %q, want prefix %q", out.String(), tt.wantOutput)
			}
		})
	}
}

func TestRunCLILaunchError(t *testing.T) {
	want := errors.New("launch failed")
	err := runCLI(nil, &bytes.Buffer{}, func([]string, bool, string, string, bool, configuration) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
