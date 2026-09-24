//go:build linux && e2e

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lobkovilya/pi-square/e2e/internal/harness"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var fixture *harness.Fixture

func TestE2E(t *testing.T) {
	suiteConfig, reporterConfig := GinkgoConfiguration()
	if suiteConfig.LabelFilter == "" {
		suiteConfig.LabelFilter = "!live"
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "pi-square E2E Suite", suiteConfig, reporterConfig)
}

var _ = BeforeSuite(func(ctx SpecContext) {
	var err error
	setup, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	fixture, err = harness.NewFixture(setup)
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if fixture != nil {
		Expect(fixture.Close()).To(Succeed())
	}
	harness.SetSecrets()
})

func bounded(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
