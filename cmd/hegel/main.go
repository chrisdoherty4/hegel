package main

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"github.com/tinkerbell/hegel/signals"
)

func main() {
	root, err := NewRootCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}

	ctx, _ := signals.ContextCanceller(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
