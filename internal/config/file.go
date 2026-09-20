package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// reservedKeys cannot be set from a config file: -config would recurse
// and -version is a one-shot informational action, not a default.
var reservedKeys = map[string]bool{
	"config":  true,
	"version": true,
}

// ResolveCategory turns a -category name into the grep pattern it
// names. Multiple patterns for one category OR-combine, each isolated
// in a non-capturing group. Unknown names fail fast — a typo silently
// falling back to "no pattern" would turn a search into a file
// listing — and the error says what the file actually defines.
func ResolveCategory(name string, patterns map[string][]string) (string, error) {
	pats := patterns[name]
	if len(pats) == 0 {
		if len(patterns) == 0 {
			return "", fmt.Errorf("-category %q: the config file defines no patterns", name)
		}
		known := make([]string, 0, len(patterns))
		for k := range patterns {
			known = append(known, k)
		}
		sort.Strings(known)
		return "", fmt.Errorf("-category %q is not defined in the config file (available: %s)", name, strings.Join(known, ", "))
	}
	if len(pats) == 1 {
		return pats[0], nil
	}
	groups := make([]string, len(pats))
	for i, p := range pats {
		groups[i] = "(?:" + p + ")"
	}
	return strings.Join(groups, "|"), nil
}

// DefaultConfigPath returns ~/.config/emrgrep/config.yaml (or .yml)
// if one exists, "" otherwise.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, name := range []string{"config.yaml", "config.yml"} {
		p := filepath.Join(home, ".config", "emrgrep", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
