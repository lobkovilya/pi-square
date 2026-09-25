//go:build linux

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

func runCLI(args []string, out io.Writer, launch func([]string, bool, string, string, bool) error) error {
	if len(args) > 0 && args[0] == "gateway" {
		if len(args) < 2 {
			return fmt.Errorf("usage: pi-square gateway list | start NAME | stop NAME [--force]")
		}
		switch args[1] {
		case "list":
			if len(args) != 2 {
				return fmt.Errorf("usage: pi-square gateway list")
			}
			return listInstances(out)
		case "start":
			if len(args) != 3 {
				return fmt.Errorf("usage: pi-square gateway start NAME")
			}
			return startInstance(args[2])
		case "stop":
			if len(args) != 3 && len(args) != 4 {
				return fmt.Errorf("usage: pi-square gateway stop NAME [--force]")
			}
			force := len(args) == 4 && args[3] == "--force"
			if len(args) == 4 && !force {
				return fmt.Errorf("usage: pi-square gateway stop NAME [--force]")
			}
			return stopInstance(args[2], force)
		default:
			return fmt.Errorf("unknown gateway command %q", args[1])
		}
	}

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
	gateway := flags.String("gateway", "", "Attach to an existing named gateway")
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
	if *gateway != "" && !instanceName.MatchString(*gateway) {
		return fmt.Errorf("invalid gateway name %q", *gateway)
	}
	if *showHelp {
		_, err := fmt.Fprint(out, "Usage: pi-square [options] [-- pi arguments...]\n       pi-square gateway list | start NAME | stop NAME [--force]\n\nOptions:\n  --mode=dev            Disable GitHub mode prompt guidance (sandbox remains active)\n  --gh-mode=MODE        Initial GitHub mode: browse, local, or publish\n  --gateway=NAME        Attach to an existing gateway\n  --version             Show pi-square version\n  --help, -h            Show help\n\nPass arguments to pi after --, e.g. pi-square -- --version.\n")
		return err
	}
	if *showVersion {
		_, err := fmt.Fprintln(out, "pi-square", version)
		return err
	}
	name := *gateway
	if name == "" {
		name = "default"
	}
	return launch(piArgs, *mode == "dev", *ghMode, name, *gateway != "")
}
