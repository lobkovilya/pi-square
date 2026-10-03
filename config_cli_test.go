//go:build linux

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("config default command", func() {
	It("prints formatted built-ins without reading configuration or launching Pi", func() {
		// given
		dir := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", dir)
		configDir := filepath.Join(dir, "pi-square")
		Expect(os.MkdirAll(configDir, 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(configDir, "config.json"), []byte("invalid JSON"), 0600)).To(Succeed())

		var out bytes.Buffer

		// when
		err := runCLI([]string{"config", "default"}, &out, nil)

		// then
		Expect(err).NotTo(HaveOccurred())
		Expect(out.String()).To(HavePrefix("{\n  \"defaultProfile\": \"browse\","))
		Expect(out.String()).To(HaveSuffix("\n"))

		// and then
		parsed, parseErr := parseConfiguration(out.Bytes())
		Expect(parseErr).NotTo(HaveOccurred())
		Expect(parsed).To(Equal(builtinConfiguration()))
	})

	DescribeTable("rejects unsupported config invocations", func(args []string) {
		// given
		var out bytes.Buffer

		// when
		err := runCLI(args, &out, nil)

		// then
		Expect(err).To(MatchError("usage: pi-square config default"))
		Expect(out.Len()).To(BeZero())
	},
		Entry("missing subcommand", []string{"config"}),
		Entry("unknown subcommand", []string{"config", "init"}),
		Entry("extra argument", []string{"config", "default", "extra"}),
		Entry("unsupported flag", []string{"config", "default", "--config=other.json"}),
	)

	It("propagates output errors", func() {
		// given
		reader, writer := io.Pipe()
		Expect(reader.Close()).To(Succeed())
		DeferCleanup(writer.Close)

		// when
		err := runCLI([]string{"config", "default"}, writer, nil)

		// then
		Expect(err).To(MatchError(io.ErrClosedPipe))
	})
})
