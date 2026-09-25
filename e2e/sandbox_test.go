//go:build linux && e2e

package e2e_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/lobkovilya/pi-square/e2e/internal/harness"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func lines(output []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if i := strings.IndexByte(line, '='); i >= 0 {
			out[line[:i]] = line[i+1:]
		}
	}
	return out
}

var _ = Describe("sandbox permissions and isolation", func() {
	It("defaults to browse when --gh-mode is absent", func(ctx SpecContext) {
		// given
		opts, err := fixture.SessionOptions("browse")
		Expect(err).NotTo(HaveOccurred())
		session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--"), opts)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(session.Close)

		// when
		result, err := session.Bash(ctx, "!", "if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi")

		// then
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Output).To(Equal([]byte("readonly\n")))
		Expect(session.Quit(ctx)).To(Succeed())
	})

	It("applies browse, local, publish, and toggle to newly launched ! and !! commands", func(ctx SpecContext) {
		// given
		opts, err := fixture.SessionOptions("browse")
		Expect(err).NotTo(HaveOccurred())
		session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode=browse", "--"), opts)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(session.Close)

		// when
		browse, err := session.Bash(ctx, "!", "if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi")
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Slash(ctx, "/gh-mode local", "GitHub mode: local")).To(Succeed())
		local, err := session.Bash(ctx, "!!", "if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi")
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Slash(ctx, "/gh-mode browse", "GitHub mode: browse")).To(Succeed())
		browseAgain, err := session.Bash(ctx, "!", "if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi")
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Slash(ctx, "/gh-mode publish", "GitHub mode: publish")).To(Succeed())
		publish, err := session.Bash(ctx, "!!", "if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi")
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Slash(ctx, "/gh-mode toggle", "GitHub mode: browse")).To(Succeed())
		toggled, err := session.Bash(ctx, "!", "if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi")
		Expect(err).NotTo(HaveOccurred())

		// then
		Expect(browse.Output).To(Equal([]byte("readonly\n")))
		Expect(local.Output).To(Equal([]byte("writable\n")))
		Expect(browseAgain.Output).To(Equal([]byte("readonly\n")))
		Expect(publish.Output).To(Equal([]byte("writable\n")))
		Expect(toggled.Output).To(Equal([]byte("readonly\n")))
		Expect(session.Quit(ctx)).To(Succeed())
	})

	It("keeps launch-time permissions for a running command", func(ctx SpecContext) {
		// given
		release := "e2e-release"
		_ = os.Remove(fixture.Workdir + "/" + release)
		opts, err := fixture.SessionOptions("browse")
		Expect(err).NotTo(HaveOccurred())
		session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode=browse", "--"), opts)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(session.Close)

		// when
		running, err := session.StartBash(ctx, "!", "while [ ! -e "+harness.ShellJoin(release)+" ]; do sleep .05; done; if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi")
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Slash(ctx, "/gh-mode local", "GitHub mode: local")).To(Succeed())
		completedBeforeRelease := running.Completed()
		// Host-side release is mechanics, not an action under test; pi accepts only
		// one interactive shell command at a time. The read-only mount observes it.
		Expect(os.WriteFile(fixture.Workdir+"/"+release, []byte("release\n"), 0600)).To(Succeed())
		DeferCleanup(os.Remove, fixture.Workdir+"/"+release)
		old, err := running.Wait()
		Expect(err).NotTo(HaveOccurred())
		newResult, err := session.Bash(ctx, "!", "if touch probe-file 2>/dev/null; then rm -f probe-file; echo writable; else echo readonly; fi")
		Expect(err).NotTo(HaveOccurred())

		// then
		Expect(completedBeforeRelease).To(BeFalse(), "the mode switch must land while the browse command is running")
		Expect(old.Output).To(Equal([]byte("readonly\n")))
		Expect(newResult.Output).To(Equal([]byte("writable\n")))
		Expect(session.Quit(ctx)).To(Succeed())
	})

	DescribeTable("exposes only the dummy credential and restricted root",
		func(ctx SpecContext, mode string) {
			// given
			opts, err := fixture.SessionOptions(mode)
			Expect(err).NotTo(HaveOccurred())
			session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode="+mode, "--"), opts)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(session.Close)
			probe := strings.Join([]string{
				"echo GH_TOKEN=$GH_TOKEN", "echo HTTPS_PROXY=$HTTPS_PROXY", "echo HOME=$HOME", "echo PI_SQUARE_VARS=$(env | grep -c '^PI_SQUARE_')",
				"echo LEAKED_HOST_TOKEN=$(env | grep -c " + harness.ShellJoin(harness.FakeHostToken) + ")", "echo PID1=$(cat /proc/1/comm)",
				"echo CONTROL_SOCKET=$(test -S /run/pi-square/control.sock && echo reachable || echo hidden)", "echo GATEWAY_DIR=$(ls /run/pi-square/gateway >/dev/null 2>&1 && echo readable || echo masked)",
				// Sockets other than the standard descriptors of the command's root shell (PID 1)
				// and of this shell, which bash keeps as saved copies across redirections.
				"std=$(readlink /proc/1/fd/0 /proc/1/fd/1 /proc/1/fd/2 /proc/$$/fd/0 /proc/$$/fd/1 /proc/$$/fd/2)", "n=0", "for f in /proc/$$/fd/*; do t=$(readlink $f); case $t in socket:*) case \"$std\" in *\"$t\"*) ;; *) n=$((n+1));; esac;; esac; done", "echo INHERITED_SOCKETS=$n",
				"echo HOST_TMP=$(test -e " + harness.ShellJoin(fixture.HostSecret) + " && echo visible || echo hidden)", "echo WORKDIR=$(test -d " + harness.ShellJoin(fixture.Workdir) + " && echo visible || echo hidden)"}, "; ")

			// when
			r, err := session.Bash(ctx, "!", probe)

			// then
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Status).To(Equal(0))
			v := lines(r.Output)
			Expect(v["GH_TOKEN"]).To(Equal(harness.DummyCommandToken))
			Expect(v["HTTPS_PROXY"]).To(Equal("http://127.0.0.1:8877"))
			Expect(v["PI_SQUARE_VARS"]).To(Equal("0"))
			Expect(v["LEAKED_HOST_TOKEN"]).To(Equal("0"))
			Expect(v["PID1"]).To(Equal("bash"))
			Expect(v["CONTROL_SOCKET"]).To(Equal("hidden"))
			Expect(v["GATEWAY_DIR"]).To(Equal("masked"))
			Expect(v["INHERITED_SOCKETS"]).To(Equal("0"))
			Expect(v["HOST_TMP"]).To(Equal("hidden"))
			Expect(v["WORKDIR"]).To(Equal("visible"))
			if mode == "publish" {
				Expect(v["HOME"]).To(Equal(fixture.HostHome))
			} else {
				Expect(v["HOME"]).To(MatchRegexp(`^/tmp/pi-command-home-`))
			}
			Expect(session.Quit(ctx)).To(Succeed())
		},
		Entry("in browse mode", "browse"),
		Entry("in local mode", "local"),
		Entry("in publish mode", "publish"),
	)

	DescribeTable("denies REST writes and GraphQL mutations without escalation",
		func(ctx SpecContext, mode string) {
			// given
			opts, err := fixture.SessionOptions(mode)
			Expect(err).NotTo(HaveOccurred())
			session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode="+mode, "--"), opts)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(session.Close)
			var body struct {
				Message string                         `json:"message"`
				Error   struct{ Code, Message string } `json:"error"`
			}

			// when
			rest, restErr := session.Bash(ctx, "!", "curl -sS -D - -X POST -H 'Content-Type: application/json' -d '{}' https://api.github.com/user/repos -w '\\nHTTP_CODE=%{http_code}\\n'")
			text := string(rest.Output)
			var decodeErr error
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "{") {
					decodeErr = json.Unmarshal([]byte(line), &body)
				}
			}
			mutation, mutationErr := session.Bash(ctx, "!", "GH_NO_UPDATE_NOTIFIER=1 gh api graphql -f query='mutation{__typename}'")
			post, postErr := session.Bash(ctx, "!", "GH_NO_UPDATE_NOTIFIER=1 gh api -X POST /user/repos -f name=x")
			screen := session.Screen()

			// then
			Expect(restErr).NotTo(HaveOccurred())
			Expect(decodeErr).NotTo(HaveOccurred())
			Expect(mutationErr).NotTo(HaveOccurred())
			Expect(postErr).NotTo(HaveOccurred())
			Expect(rest.Status).To(Equal(0))
			Expect(text).To(MatchRegexp(`(?m)^HTTP/1\.1 403 `))
			Expect(text).To(MatchRegexp(`(?m)^X-Pi-Square-Denial: write_requires_publish\r?$`))
			Expect(text).To(MatchRegexp(`(?m)^HTTP_CODE=403$`))
			Expect(body.Error.Code).To(Equal("write_requires_publish"))
			Expect(body.Message).To(Equal("write_requires_publish: " + body.Error.Message))
			Expect(mutation.Status).To(Equal(1))
			Expect(mutation.Output).To(MatchRegexp(`gh: write_requires_publish: GraphQL mutations require publish mode \(HTTP 403\)`))
			Expect(post.Status).To(Equal(1))
			Expect(post.Output).To(MatchRegexp(`gh: write_requires_publish: POST is a write and requires publish mode \(HTTP 403\)`))
			Expect(screen).NotTo(ContainSubstring("Switch mode to publish"))
			Expect(session.Slash(ctx, "/gh-mode status", "GitHub mode: "+mode)).To(Succeed())
			Expect(session.Quit(ctx)).To(Succeed())
		},
		Entry("in browse mode", "browse"),
		Entry("in local mode", "local"),
	)

	It("blocks proxy bypass, plain HTTP, private destinations, and non-443 ports", func(ctx SpecContext) {
		// given
		opts, err := fixture.SessionOptions("browse")
		Expect(err).NotTo(HaveOccurred())
		session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode=browse", "--"), opts)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(session.Close)
		probe := strings.Join([]string{`probe() { local label=$1; shift; local out; out=$("$@" 2>&1); printf '%s=exit %s: %s\n' "$label" "$?" "$out"; }`, `probe DIRECT curl -sS -m 5 --noproxy '*' https://api.github.com/`, `probe PLAIN_HTTP curl -sS -m 5 http://example.com/`, `probe PRIVATE curl -sS -m 5 https://10.0.0.1/`, `probe LOOPBACK curl -sS -m 5 https://127.0.0.1/`, `probe OTHER_PORT curl -sS -m 5 https://example.com:8443/`, `probe API_PORT curl -sS -m 5 https://api.github.com:8443/`}, "; ")

		// when
		r, err := session.Bash(ctx, "!", probe)

		// then
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Status).To(Equal(0))
		v := lines(r.Output)
		Expect(v["DIRECT"]).To(MatchRegexp(`^exit 6: curl: \(6\) Could not resolve host`))
		Expect(v["PLAIN_HTTP"]).To(Equal("exit 0: only CONNECT is supported"))
		for _, label := range []string{"PRIVATE", "LOOPBACK", "OTHER_PORT", "API_PORT"} {
			Expect(v[label]).To(MatchRegexp(`^exit (7|56): curl: \((7|56)\) CONNECT tunnel failed, response 403$`), label)
		}
		Expect(session.Quit(ctx)).To(Succeed())
	})
})
