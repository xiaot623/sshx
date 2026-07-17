package main

import (
	"os"

	"github.com/xiaot623/sshx/internal/cli"
)

func main() {
	ctx, stop := cli.NotifySignalContext()
	defer stop()
	r := cli.NewRunner(os.Stdin, os.Stdout, os.Stderr)
	r.InvocationPath = os.Args[0]
	os.Exit(r.Run(ctx, os.Args[1:]))
}
