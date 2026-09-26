//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("permission configuration", func() {
	DescribeTable("rejects invalid configurations", func(input, message string) {
		// given
		data := []byte(input)

		// when
		_, err := parseConfiguration(data)

		// then
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(message))
	},
		Entry("no profiles", `{}`, "profiles"),
		Entry("version", `{"version":1}`, "unknown field"),
		Entry("missing net", `{"defaultProfile":"x","profiles":{"x":{"workdir":"rw","github":"ro","bash":"on"}}}`, `profile "x": net`),
		Entry("invalid github", `{"defaultProfile":"x","profiles":{"x":{"workdir":"rw","github":"off","net":"on","bash":"on"}}}`, "github"),
		Entry("git resource", `{"profiles":{"x":{"git":"ro"}}}`, "unknown field"),
		Entry("unknown default", `{"defaultProfile":"missing","profiles":{"x":{"workdir":"rw","github":"ro","net":"off","bash":"on"}}}`, "defaultProfile"),
		Entry("trailing object", `{} {}`, "single JSON"),
	)
	It("replaces builtins with complete profiles", func() {
		// given
		input := []byte(`{"defaultProfile":"offline","profiles":{"offline":{"workdir":"ro","github":"rw","net":"off","bash":"on"}}}`)

		// when
		c, err := parseConfiguration(input)
		class, ro, _ := c.Profiles["offline"].commandPolicy()

		// then
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Profiles).To(HaveLen(1))
		Expect(class).To(Equal(classOffline))
		Expect(ro).To(BeTrue())
	})
	It("loads the XDG file once and errors for missing explicit files", func() {
		// given
		dir := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", dir)
		c := builtinConfiguration()
		c.DefaultProfile = "local"
		data, err := json.Marshal(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.MkdirAll(filepath.Join(dir, "pi-square"), 0700)).To(Succeed())
		path := filepath.Join(dir, "pi-square", "config.json")
		Expect(os.WriteFile(path, data, 0600)).To(Succeed())

		// when
		loaded, err := loadConfiguration("")
		Expect(os.Remove(path)).To(Succeed())
		fallback, fallbackErr := loadConfiguration("")
		_, explicitErr := loadConfiguration(path)

		// then
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.DefaultProfile).To(Equal("local"))
		Expect(fallbackErr).NotTo(HaveOccurred())
		Expect(fallback.DefaultProfile).To(Equal("browse"))
		Expect(explicitErr).To(HaveOccurred())
	})
})
