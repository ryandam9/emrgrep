package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateHome points $HOME (and the Windows equivalent) at an empty
// temp dir so run() tests never read the developer's real
// ~/.config/emrgrep/config — standing defaults there (cluster-name,
// md = true, ...) would change validation errors and fail assertions
// that pass everywhere else.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// M-04: help is an informational success — and it carries worked
// examples and the exit-code cheat sheet, not just the flag list.
func TestHelpExitsZero(t *testing.T) {
	isolateHome(t)
	for _, flag := range []string{"-h", "-help"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{flag}, &stdout, &stderr); code != 0 {
			t.Errorf("%s: exit %d want 0", flag, code)
		}
		usage := stderr.String()
		for _, want := range []string{
			"-bucket",
			"-version",
			"Examples:",
			"-cluster-name hbase-prod",
			"-discover-apps",
			"-profile",
			"-profile prod-emr",
			"Exit codes:",
			"130 interrupted",
		} {
			if !strings.Contains(usage, want) {
				t.Errorf("%s: usage missing %q", flag, want)
			}
		}
	}
}

func TestInvalidFlagExitsTwo(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-no-such-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
}

func TestValidationErrorExitsTwo(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-bucket", "b", "-prefix", "p", "-workers", "0"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("stderr: %s", stderr.String())
	}
}

// -version needs no AWS configuration and exits 0.
func TestVersionExitsZero(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d want 0", code)
	}
	if !strings.Contains(stdout.String(), "emrgrep") {
		t.Fatalf("stdout: %q", stdout.String())
	}
}

// The default config path (~/.config/emrgrep/config.yaml) is honored,
// proven against an isolated home rather than the developer's real
// one: the file's bad workers value reaches validation.
func TestDefaultConfigReadFromHome(t *testing.T) {
	isolateHome(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".config", "emrgrep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("workers: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-bucket", "b", "-prefix", "p"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}
