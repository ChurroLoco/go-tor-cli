package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ChurroLoco/go-tor-cli/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root, err := cli.NewRoot()
	if err == nil {
		err = root.ExecuteContext(ctx)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "gotorrent:", err)
		os.Exit(1)
	}
}
