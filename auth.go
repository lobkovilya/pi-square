//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// dummyCommandToken is the placeholder credential handed to isolated clients so
// gh and curl construct authenticated calls. The gateway strips it and inserts
// the real credential, so its value is never sensitive.
const dummyCommandToken = "pi-square-gateway-dummy"

// githubCredentialVars are inherited environment variables that could carry a
// real GitHub credential. They are removed before pi and every command run so
// the gateway is the only source of authentication.
var githubCredentialVars = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"GH_ENTERPRISE_TOKEN",
	"GITHUB_ENTERPRISE_TOKEN",
	"GH_HOST",
}

// proxyVars are inherited proxy settings that would otherwise conflict with the
// mandatory gateway proxy.
var proxyVars = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

// resolveGitHubToken reads the host gh credential for github.com. It runs on the
// host before any sandbox so gh can see its real configuration.
func resolveGitHubToken() (string, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", fmt.Errorf("gh is required for GitHub authentication but was not found in PATH")
	}
	cmd := exec.Command(gh, "auth", "token", "--hostname", "github.com")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		hint := strings.TrimSpace(stderr.String())
		if hint == "" {
			hint = err.Error()
		}
		return "", fmt.Errorf("could not read GitHub credentials; run `gh auth login`: %s", hint)
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" {
		return "", fmt.Errorf("gh returned an empty GitHub token; run `gh auth login`")
	}
	return token, nil
}

// sanitizeParentEnv strips inherited GitHub credentials from the environment pi
// itself runs with. Pi keeps host networking, so its proxy settings are left
// alone.
func sanitizeParentEnv(env []string) []string {
	return unsetEnv(env, githubCredentialVars...)
}

// hostGitIdentity resolves the user's git identity on the host so commands run
// with a throwaway home still commit as the right author.
func hostGitIdentity() []string {
	name := firstNonEmptyEnv("GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME")
	if name == "" {
		name = gitConfigLookup("user.name")
	}
	email := firstNonEmptyEnv("GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL")
	if email == "" {
		email = gitConfigLookup("user.email")
	}
	var env []string
	if name != "" {
		env = append(env, "GIT_AUTHOR_NAME="+name, "GIT_COMMITTER_NAME="+name)
	}
	if email != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
	}
	return env
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func gitConfigLookup(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
