package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyFileSetsDefaults(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	p := writeConfig(t, `
# standing defaults
cluster-name: hbase-prod
grep: ERROR|WARN
i: true
progress: 2s
max-total-matches: 20
`)
	provided, _, err := ApplyFile(fs, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.ClusterName != "hbase-prod" || o.GrepPattern != "ERROR|WARN" || !o.IgnoreCase {
		t.Fatalf("values not applied: %+v", o)
	}
	if o.Progress.String() != "2s" || o.MaxTotalMatches != 20 {
		t.Fatalf("typed values not applied: progress=%s cap=%d", o.Progress, o.MaxTotalMatches)
	}
	for _, k := range []string{"cluster-name", "grep", "i", "progress", "max-total-matches"} {
		if !provided[k] {
			t.Fatalf("%q missing from provided set", k)
		}
	}
}

// CLI-set flags are skipped: the file must not overwrite them, but
// they still count as provided.
func TestApplyFileCLIPriority(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	if err := fs.Parse([]string{"-grep", "FROM_CLI"}); err != nil {
		t.Fatal(err)
	}
	p := writeConfig(t, "grep: FROM_FILE\ncluster-name: hbase-prod\n")
	provided, _, err := ApplyFile(fs, p, func(k string) bool { return k == "grep" })
	if err != nil {
		t.Fatal(err)
	}
	if o.GrepPattern != "FROM_CLI" {
		t.Fatalf("CLI value overwritten: %q", o.GrepPattern)
	}
	if o.ClusterName != "hbase-prod" {
		t.Fatalf("non-CLI value not applied: %q", o.ClusterName)
	}
	if !provided["grep"] {
		t.Fatal("skipped keys must still be reported as provided")
	}
}

// YAML quoting preserves leading/trailing spaces and shields regexes
// that start with YAML-special characters.
func TestApplyFileQuotedValues(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	fs.Parse(nil)
	p := writeConfig(t, `grep: " ERROR with spaces "`+"\n")
	if _, _, err := ApplyFile(fs, p, nil); err != nil {
		t.Fatal(err)
	}
	if o.GrepPattern != " ERROR with spaces " {
		t.Fatalf("quoted value: %q", o.GrepPattern)
	}
}

func TestApplyFileErrors(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"unknown key", "no-such: 1\n", "unknown option"},
		{"reserved config", "config: /elsewhere\n", "cannot be set"},
		{"reserved version", "version: true\n", "cannot be set"},
		{"non-scalar flag value", "grep:\n  - a\n  - b\n", "expected a single scalar value"},
		{"bad flag value", "workers: heaps\n", "workers"},
		{"legacy format", "grep = ERROR\n", "cannot unmarshal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, _ := NewFlagSet("test", io.Discard)
			fs.Parse(nil)
			p := writeConfig(t, tc.content)
			_, _, err := ApplyFile(fs, p, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestApplyFileMissing(t *testing.T) {
	fs, _ := NewFlagSet("test", io.Discard)
	fs.Parse(nil)
	if _, _, err := ApplyFile(fs, filepath.Join(t.TempDir(), "absent.yaml"), nil); err == nil {
		t.Fatal("missing explicit config file must error")
	}
}

// The patterns mapping defines named categories: a single regex or a
// list that OR-combines, alongside ordinary flag defaults.
func TestApplyFilePatterns(t *testing.T) {
	fs, o := NewFlagSet("test", io.Discard)
	fs.Parse(nil)
	p := writeConfig(t, `
cluster-name: hbase-prod
patterns:
  spark:
    - ERROR|Exception
    - Caused by
  oom: OutOfMemoryError
`)
	_, patterns, err := ApplyFile(fs, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.ClusterName != "hbase-prod" {
		t.Fatalf("flag alongside patterns not applied: %+v", o)
	}
	if len(patterns) != 2 || len(patterns["spark"]) != 2 || patterns["spark"][1] != "Caused by" || patterns["oom"][0] != "OutOfMemoryError" {
		t.Fatalf("patterns = %v", patterns)
	}
}

func TestApplyFilePatternErrors(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"empty value", "patterns:\n  spark: \"\"\n", "empty pattern"},
		{"bad regex", "patterns:\n  spark: (\n", "patterns.spark"},
		{"bad regex in list", "patterns:\n  spark:\n    - ok\n    - (\n", "patterns.spark"},
		{"patterns not a mapping", "patterns: just-a-string\n", "expected a mapping"},
		{"nested mapping", "patterns:\n  spark:\n    deep: x\n", "expected a regex or a list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, _ := NewFlagSet("test", io.Discard)
			fs.Parse(nil)
			p := writeConfig(t, tc.content)
			_, _, err := ApplyFile(fs, p, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestResolveCategory(t *testing.T) {
	patterns := map[string][]string{
		"spark": {"ERROR|Exception", "Caused by"},
		"oom":   {"OutOfMemoryError"},
	}
	if got, err := ResolveCategory("oom", patterns); err != nil || got != "OutOfMemoryError" {
		t.Fatalf("single: %q %v", got, err)
	}
	if got, err := ResolveCategory("spark", patterns); err != nil || got != "(?:ERROR|Exception)|(?:Caused by)" {
		t.Fatalf("multi: %q %v", got, err)
	}
	_, err := ResolveCategory("sprk", patterns)
	if err == nil || !strings.Contains(err.Error(), "available: oom, spark") {
		t.Fatalf("unknown category must list what exists: %v", err)
	}
	_, err = ResolveCategory("spark", nil)
	if err == nil || !strings.Contains(err.Error(), "no patterns") {
		t.Fatalf("no patterns at all: %v", err)
	}
}

// DefaultConfigPath finds config.yaml first, then config.yml.
func TestDefaultConfigPathYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := DefaultConfigPath(); got != "" {
		t.Fatalf("empty home: got %q", got)
	}
	dir := filepath.Join(home, ".config", "emrgrep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yml := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(yml, []byte("i: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultConfigPath(); got != yml {
		t.Fatalf("got %q want %q", got, yml)
	}
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("i: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DefaultConfigPath(); got != yamlPath {
		t.Fatalf("got %q want %q (yaml preferred over yml)", got, yamlPath)
	}
}
