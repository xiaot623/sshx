package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestParseIntegrateInstallArgsAcceptsYesFlagBeforeOrAfterProfile(t *testing.T) {
	for _, args := range [][]string{
		{"install", "-y", "vscode"},
		{"install", "cursor", "-y"},
	} {
		profile, assumeYes, ok := parseIntegrateInstallArgs(args)
		if !ok || !assumeYes || profile != argsProfile(args) {
			t.Fatalf("parseIntegrateInstallArgs(%q) = %q, %t, %t", args, profile, assumeYes, ok)
		}
	}
}

func TestParseIntegrateInstallArgsRejectsInvalidForms(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"install"},
		{"install", "-y"},
		{"install", "-y", "-y"},
		{"install", "vscode", "cursor"},
	} {
		if _, _, ok := parseIntegrateInstallArgs(args); ok {
			t.Fatalf("parseIntegrateInstallArgs(%q) accepted invalid arguments", args)
		}
	}
}

func TestConfirmIntegrationInstallRetriesUntilYOrN(t *testing.T) {
	var output bytes.Buffer
	confirmed, err := confirmIntegrationInstall(strings.NewReader("maybe\ny\n"), &output, "vscode")
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("confirmation = false")
	}
	if !strings.Contains(output.String(), "Please enter y or n.") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestIntegrateInstallDeclinePrintsRedWarningAndDoesNotInstall(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runner := NewRunner(strings.NewReader("n\n"), &stdout, &stderr)
	code := runner.Run(context.Background(), []string{"integrate", "install", "vscode"})
	if code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	for _, want := range []string{ansiRed, "WARNING:", "+----------------", ansiReset, "[y/n]", "cancelled"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func argsProfile(args []string) string {
	for _, arg := range args {
		if arg == "vscode" || arg == "cursor" {
			return arg
		}
	}
	return ""
}
