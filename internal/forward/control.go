package forward

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/xiaot623/sshx/internal/sshcompat"
)

func ControlOperationArgs(sshArgs []string, controlPath, operation, direction, forwardSpec string) []string {
	parsed := sshcompat.Parse(CleanSSHArgs(sshArgs))
	options := []string{
		"-S", controlPath,
		"-O", operation,
		"-o", "ExitOnForwardFailure=yes",
		"-" + direction, forwardSpec,
	}
	return sshcompat.InsertBeforeTarget(parsed, options)
}

func LocalForwardSpec(listenIP string, port int, remoteHost string) string {
	return net.JoinHostPort(listenIP, strconv.Itoa(port)) + ":" +
		net.JoinHostPort(NormalizeRemoteHost(remoteHost), strconv.Itoa(port))
}

func NormalizeRemoteHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.Trim(host, "[]")
	switch strings.ToLower(host) {
	case "", "localhost":
		return "127.0.0.1"
	default:
		return host
	}
}

func CleanSSHArgs(args []string) []string {
	return stripForwardingConfigOptions(stripAuxiliarySSHOptions(stripControlSSHOptions(args)))
}

func stripControlSSHOptions(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-S" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-S") && len(arg) > 2 {
			continue
		}
		if arg == "-o" && i+1 < len(args) {
			if isControlSSHOption(args[i+1]) {
				i++
				continue
			}
			out = append(out, arg, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, "-o") && len(arg) > 2 && isControlSSHOption(arg[2:]) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func stripAuxiliarySSHOptions(args []string) []string {
	valueActions := map[string]bool{"-D": true, "-L": true, "-R": true, "-W": true}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valueActions[arg] {
			i++
			continue
		}
		if len(arg) > 2 && valueActions[arg[:2]] {
			continue
		}
		if arg == "-N" || arg == "-f" || isTTYRequestFlag(arg) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func stripForwardingConfigOptions(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-o" && i+1 < len(args) {
			key, _, _ := strings.Cut(args[i+1], "=")
			if isForwardingConfigOption(key) {
				i++
				continue
			}
		}
		if strings.HasPrefix(arg, "-o") && len(arg) > 2 {
			key, _, _ := strings.Cut(arg[2:], "=")
			if isForwardingConfigOption(key) {
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

func isControlSSHOption(value string) bool {
	key, _, _ := strings.Cut(value, "=")
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "controlmaster", "controlpath", "controlpersist":
		return true
	default:
		return false
	}
}

func isForwardingConfigOption(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "clearallforwardings", "exitonforwardfailure", "localforward", "remoteforward", "dynamicforward":
		return true
	default:
		return false
	}
}

func isTTYRequestFlag(arg string) bool {
	if len(arg) < 2 || arg[0] != '-' || arg[1] != 't' {
		return false
	}
	for i := 1; i < len(arg); i++ {
		if arg[i] != 't' {
			return false
		}
	}
	return true
}

func controlForwardError(output []byte, err error) error {
	msg := strings.TrimSpace(string(output))
	if msg == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, msg)
}
