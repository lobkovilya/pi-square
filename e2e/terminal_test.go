//go:build linux && e2e

package e2e_test

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/lobkovilya/pi-square/e2e/internal/harness"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("interactive routing and capture", func() {
	DescribeTable("routes explicit ! and !! through the production sandbox",
		func(ctx SpecContext, mode, prefix, writability string) {
			// given
			opts, err := fixture.SessionOptions(mode)
			Expect(err).NotTo(HaveOccurred())
			command := exec.Command(fixture.Binary, "--gh-mode="+mode, "--")
			session, err := harness.OpenPi(ctx, command, opts)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(session.Close)

			// when
			result, err := session.Bash(ctx, prefix, "printf 'hello\\n'; printf 'stderr\\n' >&2; if touch protected 2>/dev/null; then echo writable; else echo readonly; fi; exit 7")

			// then
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status).To(Equal(7))
			Expect(result.Output).To(Equal([]byte("hello\nstderr\n" + writability + "\n")))
			Expect(session.Quit(ctx)).To(Succeed())
		},
		Entry("in browse mode", "browse", "!", "readonly"),
		Entry("in local mode", "local", "!!", "writable"),
		Entry("in publish mode", "publish", "!", "writable"),
	)

	It("captures wrapped ANSI, binary, empty, and signal-terminated output losslessly", func(ctx SpecContext) {
		// given
		mode := "browse"
		opts, err := fixture.SessionOptions(mode)
		Expect(err).NotTo(HaveOccurred())
		session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode=browse", "--"), opts)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(session.Close)

		// when
		wrapped, err := session.Bash(ctx, "!", "printf '%04000d\\n' 0; printf '\\033[31mred\\033[0m\\n'")
		Expect(err).NotTo(HaveOccurred())
		binary, err := session.Bash(ctx, "!", "head -c 300 /dev/urandom | base64 -w0; printf '\\0\\1\\2\\n'")
		Expect(err).NotTo(HaveOccurred())
		empty, err := session.Bash(ctx, "!", "true")
		Expect(err).NotTo(HaveOccurred())
		killed, err := session.Bash(ctx, "!", "kill -9 $$")
		Expect(err).NotTo(HaveOccurred())

		// then
		Expect(wrapped.Status).To(Equal(0))
		Expect(wrapped.Output).To(Equal([]byte(strings.Repeat("0", 4000) + "\n\x1b[31mred\x1b[0m\n")))
		Expect(binary.Status).To(Equal(0))
		Expect(binary.Output).To(HaveLen(404))
		Expect(binary.Output[:400]).To(MatchRegexp(`^[A-Za-z0-9+/=]{400}$`))
		Expect(binary.Output[400:]).To(Equal([]byte{0, 1, 2, '\n'}))
		Expect(empty.Status).To(Equal(0))
		Expect(empty.Output).To(BeEmpty())
		Expect(killed.Status).To(Equal(137))
		Expect(session.Quit(ctx)).To(Succeed())
	})

	It("times out a hung shell after readiness without fabricating a result", func(ctx SpecContext) {
		// given
		mode := "browse"
		opts, err := fixture.SessionOptions(mode)
		Expect(err).NotTo(HaveOccurred())
		opts.Timeout = 90 * time.Second
		session, err := harness.OpenPi(ctx, exec.Command(fixture.Binary, "--gh-mode=browse", "--"), opts)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(session.Close)
		op, cancel := bounded(ctx, 2*time.Second)
		defer cancel()

		// when
		result, err := session.Bash(op, "!", "sleep 30")

		// then
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(err.Error()).To(ContainSubstring("deadline exceeded during bash output"))
		Expect(result.TimedOut).To(BeTrue())
		Expect(result.Completed).To(BeFalse())
		Expect(result.Status).To(Equal(-1))
		Expect(result.Output).To(BeEmpty())
		Expect(session.Screen()).To(ContainSubstring("PSQ_")) // command was accepted, but no completion frame exists
	})
})
