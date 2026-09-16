package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRunCLI(t *testing.T) {
	for _, tt := range []struct {
		name       string
		args       []string
		wantArgs   []string
		wantOutput string
		wantError  bool
		wantLaunch bool
	}{
		{name: "default", wantLaunch: true},
		{name: "empty separator", args: []string{"--"}, wantArgs: []string{}, wantLaunch: true},
		{name: "wrapper version", args: []string{"--version"}, wantOutput: "pi-square " + version + "\n"},
		{name: "help", args: []string{"--help"}, wantOutput: "Usage: pi-square"},
		{name: "short help", args: []string{"-h"}, wantOutput: "Usage: pi-square"},
		{name: "pi version", args: []string{"--", "--version"}, wantArgs: []string{"--version"}, wantLaunch: true},
		{name: "pi help", args: []string{"--", "--help"}, wantArgs: []string{"--help"}, wantLaunch: true},
		{name: "preserve arguments", args: []string{"--", "--model", "model", "a prompt", "--", ""}, wantArgs: []string{"--model", "model", "a prompt", "--", ""}, wantLaunch: true},
		{name: "unknown wrapper flag", args: []string{"--model", "model"}, wantError: true},
		{name: "prompt without separator", args: []string{"a prompt"}, wantError: true},
		{name: "positional before separator", args: []string{"prompt", "--", "--version"}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			launched := false
			err := runCLI(tt.args, &out, func(args []string) error {
				launched = true
				if !reflect.DeepEqual(args, tt.wantArgs) {
					t.Errorf("args = %#v, want %#v", args, tt.wantArgs)
				}
				return nil
			})
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError = %v", err, tt.wantError)
			}
			if tt.wantError && !strings.Contains(err.Error(), "after --") {
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
	err := runCLI(nil, &bytes.Buffer{}, func([]string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
