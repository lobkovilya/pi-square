//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type profile struct {
	Workdir string `json:"workdir"`
	GitHub  string `json:"github"`
	Net     string `json:"net"`
	Bash    string `json:"bash"`
	Prompt  string `json:"prompt"`
}

type configuration struct {
	DefaultProfile string             `json:"defaultProfile"`
	Profiles       map[string]profile `json:"profiles"`
}

func builtinConfiguration() configuration {
	return configuration{"browse", map[string]profile{
		"browse": {
			Workdir: "ro", GitHub: "ro", Net: "on", Bash: "off",
			Prompt: "The user intends analytical work only: investigation, research, planning, or review. Do not begin implementation or publish results. A review request means report findings in this conversation, not post comments or submit a review.",
		},
		"local": {
			Workdir: "rw", GitHub: "ro", Net: "on", Bash: "on",
			Prompt: "The user intends local implementation. You may edit, test, and commit locally, but keep your work on this host. Do not push, publish, or upload code, patches, findings, or other work products, even through otherwise available network access or credentials.",
		},
		"publish": {
			Workdir: "rw", GitHub: "rw", Net: "on", Bash: "on",
			Prompt: "The user permits publishing work as part of the requested task, including pushes, pull requests, issues, comments, and submitted reviews. Publication is allowed, not required.",
		},
	}}
}

var profileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func parseConfiguration(data []byte) (configuration, error) {
	var c configuration
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("configuration: expected a single JSON object")
	}
	if len(c.Profiles) == 0 {
		return c, fmt.Errorf("configuration: profiles must not be empty")
	}
	for name, p := range c.Profiles {
		if !profileName.MatchString(name) || name == "toggle" || name == "status" || name == "t" {
			return c, fmt.Errorf("configuration: invalid profile name %q", name)
		}
		for _, field := range []struct{ name, value, a, b string }{{"workdir", p.Workdir, "ro", "rw"}, {"github", p.GitHub, "ro", "rw"}, {"net", p.Net, "on", "off"}, {"bash", p.Bash, "on", "off"}} {
			if field.value != field.a && field.value != field.b {
				return c, fmt.Errorf("profile %q: %s is missing or invalid (%q); expected %s or %s", name, field.name, field.value, field.a, field.b)
			}
		}

		if strings.TrimSpace(p.Prompt) == "" {
			return c, fmt.Errorf("profile %q: prompt must not be empty", name)
		}
	}
	if _, ok := c.Profiles[c.DefaultProfile]; !ok {
		return c, fmt.Errorf("configuration: unknown defaultProfile %q", c.DefaultProfile)
	}
	return c, nil
}
func loadConfiguration(path string) (configuration, error) {
	explicit := path != ""
	if !explicit {
		dir, err := os.UserConfigDir()
		if err != nil {
			return configuration{}, err
		}
		path = filepath.Join(dir, "pi-square", "config.json")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) && !explicit {
		return builtinConfiguration(), nil
	}
	if err != nil {
		return configuration{}, fmt.Errorf("load configuration %s: %w", path, err)
	}
	c, err := parseConfiguration(data)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
func (p profile) commandPolicy() (policyClass, bool, bool) {
	class := classRO
	if p.GitHub == "rw" {
		class = classW
	}
	if p.Net == "off" {
		class = classOffline
	}
	return class, p.Workdir == "ro", p.GitHub != "rw"
}

const classOffline policyClass = "offline"
