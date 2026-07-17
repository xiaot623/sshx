package cli

import (
	"strings"

	"github.com/xiaot623/sshx/internal/sshcompat"
)

func baseSSHArgs(parsed sshcompat.Parsed) []string {
	if parsed.TargetIndex < 0 || parsed.TargetIndex >= len(parsed.Args) {
		return nil
	}
	return append([]string(nil), parsed.Args[:parsed.TargetIndex+1]...)
}

func internalSSHArgs(sshArgs []string, remoteCommand string) []string {
	args := make([]string, 0, len(sshArgs)+2)
	args = append(args, "-n")
	args = append(args, sshCommandArgs(sshArgs, remoteCommand)...)
	return args
}

func sshCommandArgs(sshArgs []string, remoteCommand string) []string {
	if len(sshArgs) == 0 {
		return []string{remoteCommand}
	}
	// Internal control/data transports must never inherit a user-requested
	// pseudo-terminal: TTY processing would corrupt framed and binary streams.
	args := make([]string, 0, len(sshArgs)+2)
	args = append(args, sshArgs[:len(sshArgs)-1]...)
	args = append(args, "-T", sshArgs[len(sshArgs)-1])
	args = append(args, remoteCommand)
	return args
}

func sessionSSHArgs(parsed sshcompat.Parsed, remoteHome string) []string {
	return sessionSSHArgsForBridge(parsed, remoteHome, nil)
}

func sessionSSHArgsForBridge(parsed sshcompat.Parsed, remoteHome string, session *BridgeSession) []string {
	args := baseSSHArgs(parsed)
	if len(args) == 0 {
		return append([]string(nil), parsed.Args...)
	}
	envLine := remoteServerEnvScript(remoteHome)
	if session != nil {
		envLine = remoteBridgeEnvScript(remoteHome, session)
	}
	if len(parsed.RemoteCommand) == 0 {
		if hasSSHSessionlessFlag(args) {
			return append([]string(nil), parsed.Args...)
		}
		if !hasSSHDisableTTYFlag(args) && !hasSSHForceTTYFlag(args) {
			args = append([]string{"-t"}, args...)
		}
		return append(args, remoteLoginShellWithEnv(envLine))
	}
	return append(args, remoteExecShellWithEnv(envLine, parsed.RemoteCommand))
}

func remoteLoginShell(remoteHome string) string {
	return remoteLoginShellWithEnv(remoteServerEnvScript(remoteHome))
}

func remoteLoginShellWithEnv(envLine string) string {
	script := strings.Join([]string{
		envLine,
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"shell=${SHELL:-sh}",
		"name=${shell##*/}",
		"case \"$name\" in",
		"  bash) rc=\"$SSHX_SERVER_HOME/bashrc\"; { printf '%s\\n' " + shellQuote("test -f \"$HOME/.bashrc\" && . \"$HOME/.bashrc\"") + "; printf '%s\\n' " + shellQuote(envLine) + "; } > \"$rc\"; exec \"$shell\" --rcfile \"$rc\" -i ;;",
		"  zsh) zdot=\"$SSHX_SERVER_HOME/zdotdir\"; mkdir -p \"$zdot\"; { printf '%s\\n' " + shellQuote("test -f \"$HOME/.zshrc\" && . \"$HOME/.zshrc\"") + "; printf '%s\\n' " + shellQuote(envLine) + "; } > \"$zdot/.zshrc\"; ZDOTDIR=\"$zdot\" exec \"$shell\" -i ;;",
		"  *) exec \"$shell\" -i ;;",
		"esac",
	}, "\n")
	return remoteShell(script)
}

func remoteExecShell(remoteHome string, argv []string) string {
	return remoteExecShellWithEnv(remoteServerEnvScript(remoteHome), argv)
}

func remoteExecShellWithEnv(envLine string, argv []string) string {
	if len(argv) == 1 {
		return remoteExecCommandShellWithEnv(envLine, argv[0])
	}
	parts := []string{
		"sh",
		"-lc",
		strings.Join([]string{
			envLine,
			"mkdir -p \"$SSHX_SERVER_HOME\"",
			"shell=${SHELL:-sh}",
			"name=${shell##*/}",
			"case \"$name\" in",
			"  bash) err=\"$SSHX_SERVER_HOME/bash-stderr.$$\"; \"$shell\" -ic " + shellQuote(envLine+"; \"$@\"") + " sh \"$@\" 2>\"$err\"; status=$?; sed '/^bash: cannot set terminal process group /d; /^bash: no job control in this shell$/d' \"$err\" >&2; rm -f \"$err\"; exit $status ;;",
			"  zsh) exec \"$shell\" -ic " + shellQuote(envLine+"; \"$@\"") + " sh \"$@\" ;;",
			"  *) \"$@\" ;;",
			"esac",
		}, "\n"),
		"sh",
	}
	parts = append(parts, argv...)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, shellQuote(part))
	}
	return strings.Join(quoted, " ")
}

func remoteExecCommandShell(remoteHome string, command string) string {
	return remoteExecCommandShellWithEnv(remoteServerEnvScript(remoteHome), command)
}

func remoteExecCommandShellWithEnv(envLine, command string) string {
	commandLine := envLine + "; " + command
	script := strings.Join([]string{
		envLine,
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"shell=${SHELL:-sh}",
		"name=${shell##*/}",
		"case \"$name\" in",
		"  bash) err=\"$SSHX_SERVER_HOME/bash-stderr.$$\"; \"$shell\" -ic " + shellQuote(commandLine) + " 2>\"$err\"; status=$?; sed '/^bash: cannot set terminal process group /d; /^bash: no job control in this shell$/d' \"$err\" >&2; rm -f \"$err\"; exit $status ;;",
		"  zsh) exec \"$shell\" -ic " + shellQuote(commandLine) + " ;;",
		"  *) exec \"$shell\" -lc " + shellQuote(commandLine) + " ;;",
		"esac",
	}, "\n")
	return remoteShell(script)
}

func remoteShell(script string) string {
	return "sh -lc " + shellQuote(script)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func hasSSHSessionlessFlag(args []string) bool {
	for _, arg := range args {
		if shortOptionClusterContains(arg, 'N') || shortOptionClusterContains(arg, 'W') {
			return true
		}
	}
	return false
}

func hasSSHDisableTTYFlag(args []string) bool {
	for _, arg := range args {
		if shortOptionClusterContains(arg, 'T') {
			return true
		}
	}
	return false
}

func hasSSHForceTTYFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-t" || arg == "-tt" {
			return true
		}
	}
	return false
}

func shortOptionClusterContains(arg string, flag byte) bool {
	if len(arg) < 2 || arg[0] != '-' || arg == "--" {
		return false
	}
	if strings.HasPrefix(arg, "-o") || strings.HasPrefix(arg, "-O") {
		return false
	}
	for i := 1; i < len(arg); i++ {
		if arg[i] == flag {
			return true
		}
	}
	return false
}
