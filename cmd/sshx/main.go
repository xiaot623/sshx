package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/xiaot623/sshx/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := cli.NewRunner(os.Stdin, os.Stdout, os.Stderr)
	os.Exit(r.Run(ctx, os.Args[1:]))
}
