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

func unique(prefix string) string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	Expect(err).NotTo(HaveOccurred())
	return prefix + hex.EncodeToString(b)
}

var _ = Describe("live gateway smoke tests", Label("live"), Ordered, func() {
	DescribeTable("runs gh issue create",
		func(ctx SpecContext, mode string, allowed bool) {
			// given
			id := unique("pi-square-e2e-issue-" + mode + "-")
			title := "[" + id + "] live gateway smoke test"
			body := "Append-only live test artifact. Identifier: " + id + ". Mode: " + mode + "."
			host, cancel := bounded(ctx, 60*time.Second)
			defer cancel()
			before, err := liveFixture.FindIssues(host, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(before).To(BeEmpty())
			opts, err := sessionOptions(mode)
			Expect(err).NotTo(HaveOccurred())
			session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode="+mode, "--"), opts)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(session.Close)
			command := harness.ShellJoin("gh", "issue", "create", "--repo", liveFixture.Repository, "--title", title, "--body", body)

			// when
			result, err := session.Bash(ctx, "!", command)

			// then
			Expect(err).NotTo(HaveOccurred())
			diagnostic := harness.FormatResult(result)
			if !allowed {
				Expect(result.Status).NotTo(Equal(0), diagnostic)
				Expect(string(result.Output)).To(ContainSubstring("write_requires_publish"), diagnostic)
				check, cancel := bounded(ctx, 60*time.Second)
				defer cancel()
				issues, err := liveFixture.FindIssues(check, id)
				Expect(err).NotTo(HaveOccurred())
				Expect(issues).To(BeEmpty(), "unexpected append-only artifacts: %v\n%s", issues, diagnostic)
			} else {
				Expect(result.Status).To(Equal(0), diagnostic)
				urls := regexp.MustCompile(`https://github\.com/[^\s]+/issues/\d+`).FindAllString(string(result.Output), -1)
				Expect(urls).To(HaveLen(1), diagnostic)
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
			Expect(session.Quit(ctx)).To(Succeed())
		},
		Entry("in browse mode", "browse", false),
		Entry("in local mode", "local", false),
		Entry("in publish mode", "publish", true),
	)

	DescribeTable("runs curl to public HTTPS",
		func(ctx SpecContext, mode string) {
			// given
			id := unique("pi-square-e2e-http-" + mode + "-")
			target := "https://httpbin.org/get?pi_square_id=" + url.QueryEscape(id)
			opts, err := sessionOptions(mode)
			Expect(err).NotTo(HaveOccurred())
			session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode="+mode, "--"), opts)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(session.Close)
			command := harness.ShellJoin("curl", "--connect-timeout", "10", "--max-time", "30", "--fail-with-body", "--silent", "--show-error", target, "--write-out", "\n__PI_SQUARE_HTTP_STATUS__:%{http_code}\n")

			// when
			result, err := session.Bash(ctx, "!", command)

			// then
			Expect(err).NotTo(HaveOccurred())
			diagnostic := harness.FormatResult(result)
			Expect(result.Status).To(Equal(0), diagnostic)
			output := string(result.Output)
			marker := strings.LastIndex(output, "__PI_SQUARE_HTTP_STATUS__:200")
			Expect(marker).To(BeNumerically(">=", 0), diagnostic)
			start := strings.Index(output[:marker], "{")
			Expect(start).To(BeNumerically(">=", 0), diagnostic)
			var response struct {
				Args struct {
					ID string `json:"pi_square_id"`
				} `json:"args"`
			}
			Expect(json.Unmarshal([]byte(strings.TrimSpace(output[start:marker])), &response)).To(Succeed(), diagnostic)
			Expect(response.Args.ID).To(Equal(id))
			Expect(session.Quit(ctx)).To(Succeed())
		},
		Entry("in browse mode", "browse"),
		Entry("in local mode", "local"),
		Entry("in publish mode", "publish"),
	)
})
