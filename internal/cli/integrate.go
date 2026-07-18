package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xiaot623/sshx/internal/integration"
)

const (
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
)

func (r *Runner) runIntegrate(ctx context.Context, args []string) int {
	profileName, assumeYes, ok := parseIntegrateInstallArgs(args)
	if !ok {
		fmt.Fprintln(r.Stderr, "usage: sshx integrate install [-y] <vscode|cursor>")
		return 2
	}
	profile, err := integration.ParseProfile(profileName)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx integrate: %v\n", err)
		return 2
	}
	printExperimentalIntegrationWarning(r.Stderr)
	if !assumeYes {
		confirmed, err := confirmIntegrationInstall(r.Stdin, r.Stderr, profile)
		if err != nil {
			fmt.Fprintf(r.Stderr, "sshx integrate: confirmation failed: %v\n", err)
			return 1
		}
		if !confirmed {
			fmt.Fprintln(r.Stderr, "sshx: integration installation cancelled")
			return 1
		}
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

func parseIntegrateInstallArgs(args []string) (profile string, assumeYes bool, ok bool) {
	if len(args) < 2 || len(args) > 3 || args[0] != "install" {
		return "", false, false
	}
	for _, arg := range args[1:] {
		if arg == "-y" {
			if assumeYes {
				return "", false, false
			}
			assumeYes = true
			continue
		}
		if profile != "" {
			return "", false, false
		}
		profile = arg
	}
	return profile, assumeYes, profile != ""
}

func printExperimentalIntegrationWarning(w io.Writer) {
	lines := []string{
		"+--------------------------------------------------------------------+",
		"| WARNING: VS Code / Cursor integration is highly experimental.     |",
		"| It changes Remote SSH settings and may affect existing workflows. |",
		"+--------------------------------------------------------------------+",
	}
	fmt.Fprintln(w, ansiRed+strings.Join(lines, "\n")+ansiReset)
}

func confirmIntegrationInstall(in io.Reader, out io.Writer, profile integration.Profile) (bool, error) {
	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprintf(out, "Proceed with %s integration installation? [y/n]: ", profile)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return false, err
			}
			fmt.Fprintln(out)
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(out, "Please enter y or n.")
		}
	}
}
