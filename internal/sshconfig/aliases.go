package sshconfig

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	aliases, err := Aliases(path)
	if err != nil {
		return false, err
	}
	i := sort.SearchStrings(aliases, target)
	return i < len(aliases) && aliases[i] == target, nil
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

	baseDir := filepath.Dir(clean)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(stripComment(scanner.Text()))
		if len(fields) < 2 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "host":
			for _, field := range fields[1:] {
				if isConcreteHostAlias(field) {
					seenAliases[field] = true
				}
			}
		case "include":
			for _, include := range fields[1:] {
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
	return scanner.Err()
}

func stripComment(line string) string {
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range line {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			b.WriteRune(r)
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			b.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			b.WriteRune(r)
			continue
		}
		if r == '#' {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
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
