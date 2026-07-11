package cli

import (
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/xiaot623/sshx/internal/loopback"
)

var loopbackAliasRE = regexp.MustCompile(`(?m)^\s*inet\s+` + regexp.QuoteMeta(loopback.Prefix) + `(\d+)\s`)

// missingLoopbackAliases returns the provisioned target addresses that are not
// present in ifconfigOutput. Pure function for testability.
func missingLoopbackAliases(ifconfigOutput string) []string {
	present := make(map[string]bool, loopback.Size)
	for _, m := range loopbackAliasRE.FindAllStringSubmatch(ifconfigOutput, -1) {
		present[loopback.Prefix+m[1]] = true
	}
	var missing []string
	for i := 0; i < loopback.Size; i++ {
		ip := loopback.Address(i)
		if !present[ip] {
			missing = append(missing, ip)
		}
	}
	return missing
}

// loopbackAliasCommands returns the "ifconfig lo0 alias <ip> up" shell commands
// needed to provision the darwin loopback pool, or nil if nothing is missing or
// the platform is not darwin.
func loopbackAliasCommands() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("ifconfig", "lo0").Output()
	if err != nil {
		// Best-effort: assume nothing present, provision the whole pool.
		out = nil
	}
	missing := missingLoopbackAliases(string(out))
	if len(missing) == 0 {
		return nil
	}
	cmds := make([]string, 0, len(missing))
	for _, ip := range missing {
		cmds = append(cmds, "ifconfig lo0 alias "+ip+" up")
	}
	return cmds
}

// joinShellCommands joins independent shell commands with " ; " so a single
// sudo invocation runs them all; a failure of one does not abort the others
// (we want best-effort provisioning of the whole pool).
func joinShellCommands(cmds []string) string {
	return strings.Join(cmds, " ; ")
}
