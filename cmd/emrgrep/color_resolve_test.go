package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestResolveColor(t *testing.T) {
	var buf bytes.Buffer // not a terminal

	if resolveColor("always", &buf) != true {
		t.Fatal("always must force color even without a TTY")
	}
	if resolveColor("never", &buf) != false {
		t.Fatal("never must disable color")
	}
	// auto on a non-TTY writer: off.
	if resolveColor("auto", &buf) != false {
		t.Fatal("auto must disable color when stdout is not a terminal")
	}
}

func TestResolveColorHonorsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	if resolveColor("auto", &buf) {
		t.Fatal("NO_COLOR must disable auto color")
	}
	if !resolveColor("always", &buf) {
		t.Fatal("explicit always overrides NO_COLOR")
	}
}

func TestBadColorValueExitsTwo(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	if code := run(runArgs("-bucket", "b", "-prefix", "p", "-color", "sometimes"), &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
}

// -group defaults on only where grouping applies: a content scan that
// is not names-only. Within that, an explicit command-line flag wins,
// then the config file's choice, then terminal detection. Where
// grouping does not apply the answer is always false, so a config
// file carrying "group: true" can never fail an unrelated run.
func TestResolveGroup(t *testing.T) {
	cases := []struct {
		name                                           string
		cliSet, fileSet, flagValue, content, namesOnly bool
		tty                                            bool
		want                                           bool
	}{
		{"auto: TTY + content", false, false, false, true, false, true, true},
		{"auto: piped", false, false, false, true, false, false, false},
		{"auto: list-only", false, false, false, false, false, true, false},
		{"auto: -l", false, false, false, true, true, true, false},
		{"explicit -group=false beats TTY", true, false, false, true, false, true, false},
		{"explicit -group beats pipe", true, false, true, true, false, false, true},
		{"file group:true beats pipe", false, true, true, true, false, false, true},
		{"file group:false beats TTY", false, true, false, true, false, true, false},
		{"file group:true on list-only is dropped", false, true, true, false, false, true, false},
		{"file group:true with -l is dropped", false, true, true, true, true, true, false},
		{"explicit -group with -l is kept for Build to reject", true, false, true, true, true, true, true},
	}
	for _, tc := range cases {
		got := resolveGroup(tc.cliSet, tc.fileSet, tc.flagValue, tc.content, tc.namesOnly, tc.tty)
		if got != tc.want {
			t.Errorf("%s: resolveGroup(%v,%v,%v,%v,%v,%v) = %v, want %v", tc.name,
				tc.cliSet, tc.fileSet, tc.flagValue, tc.content, tc.namesOnly, tc.tty, got, tc.want)
		}
	}
}

// In a test environment stdout is never a TTY, so the auto default
// must leave piped output in the classic single-line format (the
// -group=false path through run's flag detection).
func TestGroupExplicitFalseAccepted(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	// Validation error on workers proves parsing got past the group
	// resolution without a -group-related complaint.
	code := run(runArgs("-bucket", "b", "-prefix", "p", "-grep", "x", "-group=false", "-workers", "0"), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}
