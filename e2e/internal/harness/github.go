package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	markerSuffix  = ".pi-square-e2e-owner.json"
	fixtureReadme = "Private fixture repository for pi-square sandbox integration tests."
)

type LiveFixture struct {
	*Fixture
	Repository, Token, Checkout, lockPath string
	lockFile                              *os.File
}

func NewLiveFixture(ctx context.Context, local *Fixture) (*LiveFixture, error) {
	repo := strings.TrimSpace(os.Getenv("PI_SQUARE_E2E_REPOSITORY"))
	if repo == "" {
		return nil, fmt.Errorf("PI_SQUARE_E2E_REPOSITORY is required (OWNER/REPOSITORY)")
	}
	token := os.Getenv("PI_SQUARE_E2E_GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("PI_SQUARE_E2E_GITHUB_TOKEN is required for live tests")
	}
	SetSecrets(token)
	lf := &LiveFixture{Fixture: local, Repository: repo, Token: token}
	if err := lf.validate(ctx); err != nil {
		return nil, err
	}
	if err := lf.prepareCheckout(ctx); err != nil {
		lf.Close()
		return nil, err
	}
	return lf, nil
}

func (f *LiveFixture) hostEnv() []string { return CleanEnvironment(f.Token) }
func (f *LiveFixture) gh(ctx context.Context, args ...string) (ProcessResult, error) {
	r := RunProcess(ctx, "gh", args, "", f.hostEnv())
	return r, RequireSuccess("gh "+strings.Join(args, " "), r)
}
func (f *LiveFixture) validate(ctx context.Context) error {
	r, err := f.gh(ctx, "api", "repos/"+f.Repository)
	if err != nil {
		return fmt.Errorf("fixture repository authentication: %w", err)
	}
	var metadata struct {
		Private       bool   `json:"private"`
		HasIssues     bool   `json:"has_issues"`
		DefaultBranch string `json:"default_branch"`
	}
	if err = json.Unmarshal(r.Output, &metadata); err != nil {
		return err
	}
	if !metadata.Private || !metadata.HasIssues || metadata.DefaultBranch != "main" {
		return fmt.Errorf("fixture %s must be private, have issues enabled, and use main", f.Repository)
	}
	r, err = f.gh(ctx, "api", "repos/"+f.Repository+"/contents/README.md", "-H", "Accept: application/vnd.github.raw+json")
	if err != nil {
		return err
	}
	if string(r.Output) != fixtureReadme+"\n" {
		return fmt.Errorf("fixture %s has unexpected README.md content", f.Repository)
	}
	return nil
}

func normalizedRemote(value string) string {
	value = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(value), "/"), ".git")
	value = strings.Replace(value, "git@github.com:", "https://github.com/", 1)
	return value + ".git"
}
func (f *LiveFixture) acquireLock(target string) error {
	f.lockPath = target + ".lock"
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(f.lockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return fmt.Errorf("checkout lock %s is held; concurrent live runs need separate PI_SQUARE_E2E_CHECKOUT paths", f.lockPath)
	}
	if err = file.Truncate(0); err == nil {
		_, err = fmt.Fprintf(file, "%d\n", os.Getpid())
	}
	if err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return err
	}
	f.lockFile = file
	return nil
}
func (f *LiveFixture) validateCheckout(ctx context.Context, target string) error {
	data, err := os.ReadFile(target + markerSuffix)
	if err != nil {
		return fmt.Errorf("refusing to reset %s: ownership marker: %w", target, err)
	}
	var marker struct {
		Repository string `json:"repository"`
	}
	if json.Unmarshal(data, &marker) != nil || marker.Repository != f.Repository {
		return fmt.Errorf("refusing to reset %s: invalid test-ownership marker", target)
	}
	real, _ := filepath.EvalSymlinks(target)
	r := RunProcess(ctx, "git", []string{"rev-parse", "--show-toplevel"}, target, CleanEnvironment(""))
	if r.Status != 0 {
		return fmt.Errorf("refusing to reset %s: not a repository root", target)
	}
	top, _ := filepath.EvalSymlinks(strings.TrimSpace(string(r.Output)))
	if top != real {
		return fmt.Errorf("refusing to reset %s: it is not the repository root", target)
	}
	r = RunProcess(ctx, "git", []string{"remote", "get-url", "origin"}, target, CleanEnvironment(""))
	expected := "https://github.com/" + f.Repository + ".git"
	if r.Status != 0 || normalizedRemote(string(r.Output)) != expected {
		return fmt.Errorf("refusing to reset %s: unexpected origin", target)
	}
	return nil
}
func (f *LiveFixture) prepareCheckout(ctx context.Context) error {
	checkout := os.Getenv("PI_SQUARE_E2E_CHECKOUT")
	if checkout == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		checkout = filepath.Join(cache, "pi-square", "e2e", "checkout")
	}
	checkout, _ = filepath.Abs(checkout)
	f.Checkout = checkout
	if err := f.acquireLock(checkout); err != nil {
		return err
	}
	if _, err := os.Stat(checkout); err == nil {
		if err = f.validateCheckout(ctx, checkout); err != nil {
			return err
		}
	} else if os.IsNotExist(err) {
		temp := fmt.Sprintf("%s.clone-%d", checkout, os.Getpid())
		_ = os.RemoveAll(temp)
		r := RunProcess(ctx, "gh", []string{"repo", "clone", f.Repository, temp, "--", "--quiet"}, "", f.hostEnv())
		if err = RequireSuccess("fixture clone", r); err != nil {
			return err
		}
		if err = os.Rename(temp, checkout); err != nil {
			return err
		}
		marker, _ := json.Marshal(struct {
			Repository string `json:"repository"`
		}{f.Repository})
		if err = os.WriteFile(checkout+markerSuffix, append(marker, '\n'), 0600); err != nil {
			return err
		}
		if err = f.validateCheckout(ctx, checkout); err != nil {
			return err
		}
	} else {
		return err
	}
	for _, args := range [][]string{{"fetch", "--prune", "origin", "main"}, {"reset", "--hard", "origin/main"}, {"clean", "-ffdx"}} {
		env := CleanEnvironment("")
		if args[0] == "fetch" {
			env = f.hostEnv()
			env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=!f() { echo username=x-access-token; echo password=$GH_TOKEN; }; f")
		}
		r := RunProcess(ctx, "git", args, checkout, env)
		if err := RequireSuccess("git "+args[0], r); err != nil {
			return err
		}
	}
	return nil
}
func (f *LiveFixture) Close() error {
	if f.lockFile != nil {
		_ = syscall.Flock(int(f.lockFile.Fd()), syscall.LOCK_UN)
		err := f.lockFile.Close()
		f.lockFile = nil
		return err
	}
	return nil
}

func (f *LiveFixture) API(ctx context.Context, path string) (map[string]any, error) {
	r, err := f.gh(ctx, "api", path)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(r.Output, &out)
	return out, err
}
func (f *LiveFixture) FindIssues(ctx context.Context, identifier string) ([]map[string]any, error) {
	var matches []map[string]any
	for page := 1; ; page++ {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("listing all fixture issues: %w", ctx.Err())
		default:
		}
		r, err := f.gh(ctx, "api", fmt.Sprintf("repos/%s/issues?state=all&per_page=100&page=%d", f.Repository, page))
		if err != nil {
			return nil, err
		}
		var items []map[string]any
		if err = json.Unmarshal(r.Output, &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			if item["title"] == identifier || strings.Contains(fmt.Sprint(item["body"]), identifier) {
				matches = append(matches, item)
			}
		}
		if len(items) < 100 {
			return matches, nil
		}
	}
}

func InitRepository(ctx context.Context, repository, confirmation string) error {
	if repository == "" {
		return fmt.Errorf("PI_SQUARE_E2E_REPOSITORY is required")
	}
	if confirmation != repository {
		return fmt.Errorf("repository creation is irreversible; confirm with --confirm=%s", repository)
	}
	token := os.Getenv("PI_SQUARE_E2E_GITHUB_TOKEN")
	SetSecrets(token)
	defer SetSecrets()
	env := CleanEnvironment(token)
	probe := RunProcess(ctx, "gh", []string{"api", "repos/" + repository}, "", env)
	if probe.Status == 0 {
		return fmt.Errorf("%s already exists; refusing to alter it", repository)
	}
	if !strings.Contains(probe.Stderr, "HTTP 404") && !strings.Contains(string(probe.Output), "Not Found") {
		return fmt.Errorf("could not prove repository absence; refusing creation\n%s", FormatResult(probe))
	}
	dir, err := os.MkdirTemp("", "pi-square-e2e-fixture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err = os.WriteFile(filepath.Join(dir, "README.md"), []byte(fixtureReadme+"\n"), 0600); err != nil {
		return err
	}
	commands := [][]string{{"git", "init", "-b", "main"}, {"git", "add", "README.md"}, {"git", "-c", "user.name=pi-square e2e", "-c", "user.email=pi-square-e2e@users.noreply.github.com", "commit", "-m", "Initialize live test fixture"}, {"gh", "repo", "create", repository, "--private", "--source", dir, "--remote", "origin", "--push"}, {"gh", "api", "--method", "PATCH", "repos/" + repository, "-F", "has_issues=true"}}
	for _, c := range commands {
		runDir := dir
		r := RunProcess(ctx, c[0], c[1:], runDir, env)
		if err = RequireSuccess(strings.Join(c, " "), r); err != nil {
			return err
		}
	}
	fmt.Printf("Created private append-only fixture %s\n", repository)
	return nil
}
