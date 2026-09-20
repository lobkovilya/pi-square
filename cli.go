package main

import (
	"flag"
	"fmt"
	"io"
	"runtime/debug"
)

// Set by release builds with -ldflags "-X main.version=...".
var version string

func init() {
	if version == "" {
		version = buildVersion()
	}
}

func buildVersion() string {
	const prefix = "0.0.0-preview.v"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				revision := setting.Value
				if len(revision) > 7 {
					revision = revision[:7]
				}
				return prefix + revision
			}
		}
	}
	return prefix + "unknown"
}

func runCLI(args []string, out io.Writer, launch func([]string, bool, string) error) error {
	var piArgs []string
	for i, arg := range args {
		if arg == "--" {
			piArgs = args[i+1:]
			args = args[:i]
			break
		}
	}

	flags := flag.NewFlagSet("pi-square", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	showVersion := flags.Bool("version", false, "Show pi-square version")
	showHelp := flags.Bool("help", false, "Show help")
	flags.BoolVar(showHelp, "h", false, "Show help")
	mode := flags.String("mode", "", "Wrapper mode (dev disables GitHub mode prompt guidance)")
	ghMode := flags.String("gh-mode", "", "Initial GitHub mode (browse, local, or publish)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%v; pass pi arguments after --", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q; pass pi arguments after --", flags.Arg(0))
	}
	if *mode != "" && *mode != "dev" {
		return fmt.Errorf("unsupported mode %q; expected dev", *mode)
	}
	if *ghMode != "" && *ghMode != "browse" && *ghMode != "local" && *ghMode != "publish" {
		return fmt.Errorf("unsupported GitHub mode %q; expected browse, local, or publish", *ghMode)
	}
	if *showHelp {
		_, err := fmt.Fprint(out, "Usage: pi-square [options] [-- pi arguments...]\n\nOptions:\n  --mode=dev            Disable GitHub mode prompt guidance (sandbox remains active)\n  --gh-mode=MODE        Initial GitHub mode: browse, local, or publish\n  --version             Show pi-square version\n  --help, -h            Show help\n\nPass arguments to pi after --, e.g. pi-square -- --version.\n")
		return err
	}
	if *showVersion {
		_, err := fmt.Fprintln(out, "pi-square", version)
		return err
	}
	return launch(piArgs, *mode == "dev", *ghMode)
}
