//go:build linux && e2e

package e2e_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lobkovilya/pi-square/e2e/internal/harness"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var fixture *harness.Fixture
var liveFixture *harness.LiveFixture
var runLive bool

func TestE2E(t *testing.T) {
	suiteConfig, reporterConfig := GinkgoConfiguration()
	if suiteConfig.LabelFilter == "" {
		suiteConfig.LabelFilter = "!live"
	}
	runLive = strings.Contains(suiteConfig.LabelFilter, "live") && !strings.Contains(suiteConfig.LabelFilter, "!live")
	RegisterFailHandler(Fail)
	RunSpecs(t, "pi-square E2E Suite", suiteConfig, reporterConfig)
}

var _ = BeforeSuite(func(ctx SpecContext) {
	var err error
	setup, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	fixture, err = harness.NewFixture(setup)
	Expect(err).NotTo(HaveOccurred())
	if runLive {
		liveFixture, err = harness.NewLiveFixture(setup, fixture)
		Expect(err).NotTo(HaveOccurred())
	}
})

var _ = AfterSuite(func() {
	if liveFixture != nil {
		Expect(liveFixture.Close()).To(Succeed())
	}
	if fixture != nil {
		Expect(fixture.Close()).To(Succeed())
	}
	harness.SetSecrets()
})

func sessionOptions(mode string) (harness.SessionOptions, error) {
	if liveFixture != nil {
		return fixture.SessionOptionsAt(mode, liveFixture.Token, liveFixture.Checkout)
	}
	return fixture.SessionOptions(mode, "")
}

func bounded(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

var _ = os.Getenv // retain an obvious reminder that live opt-in is process configuration
