package sshconfig

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kevinburke/ssh_config"
)

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

func Aliases(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	seenFiles := map[string]bool{}
	seenAliases := map[string]bool{}
	if err := collectAliases(path, seenFiles, seenAliases); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seenAliases))
	for alias := range seenAliases {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out, nil
}

func HasAlias(path, target string) (bool, error) {
	if path == "" {
		return false, nil
	}
	seenFiles := map[string]bool{}
	seenAliases := map[string]bool{}
	if err := collectAliases(path, seenFiles, seenAliases); err != nil {
		return false, err
	}
	return seenAliases[target], nil
}

func collectAliases(path string, seenFiles map[string]bool, seenAliases map[string]bool) error {
	expanded := expandHome(path)
	if !filepath.IsAbs(expanded) {
		abs, err := filepath.Abs(expanded)
		if err == nil {
			expanded = abs
		}
	}
	matches, err := filepath.Glob(expanded)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		matches = []string{expanded}
	}
	for _, match := range matches {
		if err := collectAliasesFile(match, seenFiles, seenAliases); err != nil {
			return err
		}
	}
	return nil
}

func collectAliasesFile(path string, seenFiles map[string]bool, seenAliases map[string]bool) error {
	clean := filepath.Clean(path)
	if seenFiles[clean] {
		return nil
	}
	seenFiles[clean] = true
	f, err := os.Open(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	cfg, err := ssh_config.Decode(f)
	if err != nil {
		return err
	}

	baseDir := filepath.Dir(clean)
	for _, host := range cfg.Hosts {
		if isHostDirective(host) {
			for _, pat := range host.Patterns {
				if pat == nil {
					continue
				}
				alias := pat.String()
				if isConcreteHostAlias(alias) {
					seenAliases[alias] = true
				}
			}
		}
		for _, node := range host.Nodes {
			inc, ok := node.(*ssh_config.Include)
			if !ok {
				continue
			}
			for _, include := range includeDirectives(inc) {
				include = expandHome(include)
				if !filepath.IsAbs(include) {
					include = filepath.Join(baseDir, include)
				}
				if err := collectAliases(include, seenFiles, seenAliases); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isHostDirective(h *ssh_config.Host) bool {
	line, _, _ := strings.Cut(strings.TrimSpace(h.String()), "\n")
	key, _, _ := strings.Cut(strings.ToLower(strings.ReplaceAll(line, "=", " ")), " ")
	return key == "host"
}

func includeDirectives(inc *ssh_config.Include) []string {
	line := inc.String()
	if inc.Comment != "" {
		line, _, _ = strings.Cut(line, "#")
	}
	line = strings.TrimSpace(line)
	lower := strings.ToLower(line)
	switch {
	case strings.HasPrefix(lower, "include="):
		line = line[len("include="):]
	case strings.HasPrefix(lower, "include"):
		line = strings.TrimSpace(line[len("include"):])
		line = strings.TrimPrefix(line, "=")
	default:
		return nil
	}
	return strings.Fields(line)
}

func isConcreteHostAlias(s string) bool {
	return s != "" && !strings.ContainsAny(s, "*?!")
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
