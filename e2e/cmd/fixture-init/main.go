package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lobkovilya/pi-square/e2e/internal/harness"
)

func main() {
	confirm := flag.String("confirm", "", "exact OWNER/REPOSITORY confirmation")
	flag.Parse()
	repo := os.Getenv("PI_SQUARE_E2E_REPOSITORY")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := harness.InitRepository(ctx, repo, *confirm); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
