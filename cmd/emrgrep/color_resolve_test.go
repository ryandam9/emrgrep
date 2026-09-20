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
	if code := run([]string{"-bucket", "b", "-prefix", "p", "-color", "sometimes"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d want 2", code)
	}
}

// -group defaults on only for interactive terminals with a content
// grep; explicit flags always win.
func TestResolveGroup(t *testing.T) {
	cases := []struct {
		explicit, flagValue, grepSet, namesOnly, tty bool
		want                                         bool
	}{
		{false, false, true, false, true, true},   // auto: TTY + grep → grouped
		{false, false, true, false, false, false}, // auto: piped → flat
		{false, false, false, false, true, false}, // auto: list-only → flat
		{false, false, true, true, true, false},   // auto: -l → flat
		{true, false, true, false, true, false},   // explicit -group=false wins on TTY
		{true, true, true, false, false, true},    // explicit -group wins when piped
	}
	for i, tc := range cases {
		if got := resolveGroup(tc.explicit, tc.flagValue, tc.grepSet, tc.namesOnly, tc.tty); got != tc.want {
			t.Errorf("case %d: resolveGroup(%v,%v,%v,%v,%v) = %v, want %v",
				i, tc.explicit, tc.flagValue, tc.grepSet, tc.namesOnly, tc.tty, got, tc.want)
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
	code := run([]string{"-bucket", "b", "-prefix", "p", "-grep", "x", "-group=false", "-workers", "0"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}
