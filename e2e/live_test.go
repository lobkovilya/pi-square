//go:build linux && e2e

package e2e_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/lobkovilya/pi-square/e2e/internal/harness"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var liveFixture *harness.LiveFixture

func unique(prefix string) string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	Expect(err).NotTo(HaveOccurred())
	return prefix + hex.EncodeToString(b)
}

var _ = Describe("live gateway smoke tests", Label("live"), Ordered, ContinueOnFailure, func() {
	BeforeAll(func(ctx SpecContext) {
		setup, cancel := bounded(ctx, 3*time.Minute)
		defer cancel()
		var err error
		liveFixture, err = harness.NewLiveFixture(setup, fixture)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(liveFixture.Close)
	})

	DescribeTable("runs gh issue create",
		func(ctx SpecContext, mode string, allowed bool) {
			// given
			started := time.Now()
			id := unique("pi-square-e2e-issue-" + mode + "-")
			title := "[" + id + "] live gateway smoke test"
			body := "Append-only live test artifact. Identifier: " + id + ". Mode: " + mode + "."
			host, cancel := bounded(ctx, 60*time.Second)
			defer cancel()
			before, err := liveFixture.FindIssues(host, id, started)
			Expect(err).NotTo(HaveOccurred())
			Expect(before).To(BeEmpty())
			opts, err := liveFixture.SessionOptions(mode)
			Expect(err).NotTo(HaveOccurred())
			session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode="+mode, "--"), opts)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(session.Close)
			command := harness.ShellJoin("gh", "issue", "create", "--repo", liveFixture.Repository, "--title", title, "--body", body)

			// when
			result, err := session.Bash(ctx, "!", command)

			// then
			Expect(err).NotTo(HaveOccurred())
			Expect(session.Quit(ctx)).To(Succeed())
			diagnostic := harness.FormatResult(result)
			if !allowed {
				Expect(result.Status).NotTo(Equal(0), "%s", diagnostic)
				Expect(string(result.Output)).To(ContainSubstring("write_requires_publish"), "%s", diagnostic)
				check, cancel := bounded(ctx, 60*time.Second)
				defer cancel()
				issues, err := liveFixture.FindIssues(check, id, started)
				Expect(err).NotTo(HaveOccurred())
				Expect(issues).To(BeEmpty(), "unexpected append-only artifacts: %v\n%s", issues, diagnostic)
			} else {
				Expect(result.Status).To(Equal(0), "%s", diagnostic)
				urls := regexp.MustCompile(`https://github\.com/[^\s]+/issues/\d+`).FindAllString(string(result.Output), -1)
				Expect(urls).To(HaveLen(1), "%s", diagnostic)
				pattern := `^https://github\.com/` + regexp.QuoteMeta(liveFixture.Repository) + `/issues/(\d+)$`
				match := regexp.MustCompile(pattern).FindStringSubmatch(urls[0])
				Expect(match).To(HaveLen(2), "wrong repository URL %s", urls[0])
				api, cancel := bounded(ctx, 30*time.Second)
				defer cancel()
				issue, err := liveFixture.API(api, "repos/"+liveFixture.Repository+"/issues/"+match[1])
				Expect(err).NotTo(HaveOccurred())
				Expect(issue["title"]).To(Equal(title))
				Expect(issue["body"]).To(Equal(body))
				Expect(issue["state"]).To(Equal("open"))
				Expect(fmt.Sprint(issue["repository_url"])).To(HaveSuffix("/repos/" + liveFixture.Repository))
				GinkgoWriter.Printf("Retained append-only issue: %s\n", urls[0])
			}
		},
		Entry("in browse mode", "browse", false),
		Entry("in local mode", "local", false),
		Entry("in publish mode", "publish", true),
	)

	It("pushes a throwaway branch in publish mode", func(ctx SpecContext) {
		// given
		branch := unique("pi-square-e2e-push-")
		opts, err := liveFixture.SessionOptions("publish")
		Expect(err).NotTo(HaveOccurred())
		session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode=publish", "--"), opts)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(session.Close)
		remote := "https://github.com/" + liveFixture.Repository + ".git"
		command := harness.ShellJoin("env", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "git", "-c", "credential.helper=", "push", remote, "HEAD:refs/heads/"+branch)

		// when
		result, err := session.Bash(ctx, "!", command)

		// then
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Quit(ctx)).To(Succeed())
		diagnostic := harness.FormatResult(result)
		Expect(result.Status).To(Equal(0), "%s", diagnostic)
		api, cancel := bounded(ctx, 30*time.Second)
		defer cancel()
		ref, err := liveFixture.API(api, "repos/"+liveFixture.Repository+"/git/ref/heads/"+branch)
		Expect(err).NotTo(HaveOccurred())
		Expect(ref["ref"]).To(Equal("refs/heads/" + branch))
		Expect(ref["object"]).To(HaveKeyWithValue("type", "commit"))
		GinkgoWriter.Printf("Retained append-only branch: %s#%s\n", liveFixture.Repository, branch)
	})

	DescribeTable("runs curl to public HTTPS",
		func(ctx SpecContext, mode string) {
			// given
			id := unique("pi-square-e2e-http-" + mode + "-")
			target := "https://httpbin.org/get?pi_square_id=" + url.QueryEscape(id)
			opts, err := liveFixture.SessionOptions(mode)
			Expect(err).NotTo(HaveOccurred())
			session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode="+mode, "--"), opts)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(session.Close)
			command := harness.ShellJoin("curl", "--connect-timeout", "10", "--max-time", "30", "--fail-with-body", "--silent", "--show-error", target, "--write-out", "\n__PI_SQUARE_HTTP_STATUS__:%{http_code}\n")

			// when
			result, err := session.Bash(ctx, "!", command)

			// then
			Expect(err).NotTo(HaveOccurred())
			Expect(session.Quit(ctx)).To(Succeed())
			diagnostic := harness.FormatResult(result)
			Expect(result.Status).To(Equal(0), "%s", diagnostic)
			output := string(result.Output)
			marker := strings.LastIndex(output, "__PI_SQUARE_HTTP_STATUS__:200")
			Expect(marker).To(BeNumerically(">=", 0), "%s", diagnostic)
			start := strings.Index(output[:marker], "{")
			Expect(start).To(BeNumerically(">=", 0), "%s", diagnostic)
			var response struct {
				Args struct {
					ID string `json:"pi_square_id"`
				} `json:"args"`
			}
			Expect(json.Unmarshal([]byte(strings.TrimSpace(output[start:marker])), &response)).To(Succeed(), "%s", diagnostic)
			Expect(response.Args.ID).To(Equal(id))
		},
		Entry("in browse mode", "browse"),
		Entry("in local mode", "local"),
		Entry("in publish mode", "publish"),
	)
})
