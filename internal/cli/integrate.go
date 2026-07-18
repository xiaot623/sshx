package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/xiaot623/sshx/internal/integration"
)

func (r *Runner) runIntegrate(ctx context.Context, args []string) int {
	if len(args) != 2 || args[0] != "install" {
		fmt.Fprintln(r.Stderr, "usage: sshx integrate install <vscode|cursor>")
		return 2
	}
	profile, err := integration.ParseProfile(args[1])
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx integrate: %v\n", err)
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx integrate: %v\n", err)
		return 1
	}
	result, err := integration.Install(ctx, profile, integration.InstallOptions{
		Executable: executable,
		NPMManaged: os.Getenv("SSHX_NPM_LAUNCHER") == "1",
	})
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx integrate: %v\n", err)
		return 1
	}
	fmt.Fprintf(r.Stdout, "sshx: installed %s integration in %s\n", result.Profile, result.SettingsPath)
	return 0
}
