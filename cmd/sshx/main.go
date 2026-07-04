package main

import (
	"context"
	"os"

	"github.com/xiaot623/sshx/internal/cli"
)

func main() {
	r := cli.NewRunner(os.Stdin, os.Stdout, os.Stderr)
	os.Exit(r.Run(context.Background(), os.Args[1:]))
}
