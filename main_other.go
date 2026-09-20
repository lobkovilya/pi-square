//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	if err := runCLI(os.Args[1:], os.Stdout, func([]string, bool, string) error {
		return fmt.Errorf("Linux is required")
	}); err != nil {
		fmt.Fprintf(os.Stderr, "pi-square: %v\n", err)
		os.Exit(1)
	}
}
