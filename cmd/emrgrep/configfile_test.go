package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Values from the config file reach validation exactly as if typed:
// an invalid file-provided value fails with the flag's own error.
func TestConfigFileValuesApplied(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\nmax-line-size: 999\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-max-line-size") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// A CLI flag beats the same flag in the config file: the file's bad
// workers value is ignored because the CLI provided one; the CLI's
// bad value is what validation sees.
func TestConfigFileCLIPriority(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\nworkers: 999\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-workers", "0"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "got 0") {
		t.Fatalf("CLI value must win over the file: %q", stderr.String())
	}
}

func TestConfigFileUnknownKeyFails(t *testing.T) {
	p := writeTempConfig(t, "no-such: 1\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-config", p}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown option") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestConfigFileExplicitMissingFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", filepath.Join(t.TempDir(), "nope")}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "reading config file") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// group=false in the config file counts as an explicit choice for the
// group auto-default (it must not be overridden by TTY detection; in
// tests stdout is not a TTY anyway, so here we just prove the value
// parses and the run proceeds to ordinary validation).
func TestConfigFileGroupParsed(t *testing.T) {
	p := writeTempConfig(t, "group: true\ngrep: ERROR\nbucket: b\nprefix: logs/\n"+lateSentinel)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), lateSentinelErr) {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// "md = true" as a standing default must not break runs that are not
// app-scoped: without -app-id it is silently ignored (validation moves
// on to the workers error) instead of demanding -app-id.
func TestConfigFileMDSoftDefault(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\ngrep: ERROR\nmd: true\nworkers: 0\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("file-provided md must be ignored without -app-id: exit %d stderr %q", code, stderr.String())
	}
}

// An explicit -md on the command line keeps strict validation even
// when the config file also sets it.
func TestConfigFileMDExplicitCLIStaysStrict(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\ngrep: ERROR\nmd: true\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-md"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-md requires -app-id") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// -category resolves a pattern.<name> entry into the grep pattern; a
// bad regex would fail at -grep, so reaching the workers error proves
// resolution succeeded.
func TestCategoryResolvesFromConfig(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\npatterns:\n  spark: ERROR|Exception\nworkers: 0\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-category", "spark"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// Push-back 1: an unknown category is a fail-fast error naming what
// the file defines — never a silent fallback to an unfiltered scan.
func TestCategoryUnknownFailsFast(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\npatterns:\n  spark: ERROR\n  oom: OOM\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-category", "sprk"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "available: oom, spark") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestCategoryGrepMutuallyExclusive(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\npatterns:\n  spark: ERROR\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-category", "spark", "-grep", "x"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// A CLI -grep beats a file-provided category standing default; the
// file category names a bad regex, so reaching the workers error
// proves the category was dropped, not resolved.
func TestCategoryFileDefaultLosesToCLIGrep(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\ncategory: spark\npatterns:\n  spark: ERROR\nworkers: 0\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-grep", "FATAL"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// Two standing defaults naming both grep and category is ambiguous.
func TestCategoryAndGrepBothFromFileFails(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\ngrep: ERROR\ncategory: spark\npatterns:\n  spark: X\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "both grep and category") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestCategoryRejectsFixedString(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\npatterns:\n  spark: ERROR\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-category", "spark", "-F"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-F cannot be combined with -category") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// "cat = true" as a standing default applies only to patternless
// runs: with a category in play it is dropped instead of conflicting.
func TestConfigFileCatSoftDefault(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\ncat: true\npatterns:\n  spark: ERROR\nworkers: 0\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p, "-category", "spark"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("file-provided cat must yield to a pattern: exit %d stderr %q", code, stderr.String())
	}
}

// "profile" is an ordinary flag key, so it also works as a standing
// default in the config file — and a padded value there fails with
// -profile's own validation error rather than silently resolving to no
// profile at all.
func TestConfigFileProfile(t *testing.T) {
	isolateHome(t)
	p := writeTempConfig(t, "profile: prod-emr\nbucket: b\nprefix: logs/\nworkers: 0\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-workers") {
		t.Fatalf("a valid file profile must parse and reach ordinary validation: exit %d stderr %q", code, stderr.String())
	}

	p = writeTempConfig(t, "profile: ' prod-emr'\nbucket: b\nprefix: logs/\n")
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "-profile must not have leading or trailing whitespace") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

// lateSentinel is a config-file value whose validation runs after
// every applicability check in Build (-after is parsed last). A test
// that wants to prove an option was DROPPED must fail on something
// later than the option's own check, or an earlier unrelated error
// would make the test pass whether the drop happened or not.
const lateSentinel = "after: nonsense\n"

const lateSentinelErr = "neither RFC3339"

// Standing defaults that do not apply to the run at hand are dropped
// rather than failing it. Each of these keys is legal in the config
// file and each would, unguarded, make an ordinary listing run exit 2
// — so a config file tuned for one workflow would break every other
// one. Reaching the -after error proves the option was dropped and
// validation ran to the end.
func TestConfigFileInapplicableDefaultsAreDropped(t *testing.T) {
	cases := []struct {
		name, keys string
		args       []string
	}{
		{"l on a list-only run", "l: true\n", nil},
		{"discover-apps on a list-only run", "l: true\ndiscover-apps: true\n", nil},
		{"max-total-matches on a list-only run", "max-total-matches: 20\n", nil},
		{"max-matches on a list-only run", "max-matches: 5\n", nil},
		{"max-matches on a download run", "max-matches: 5\n", []string{"-app-id", "application_1_2", "-download"}},
		{"group on a list-only run", "group: true\n", nil},
		{"group on a names-only run", "group: true\n", []string{"-grep", "ERROR", "-l"}},
		{"l on a download run", "l: true\n", []string{"-app-id", "application_1_2", "-download"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeTempConfig(t, "bucket: b\nprefix: logs/\n"+lateSentinel+tc.keys)
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"-config", p}, tc.args...), &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), lateSentinelErr) {
				t.Fatalf("inapplicable standing default must be dropped: exit %d stderr %q", code, stderr.String())
			}
		})
	}
}

// The same options typed on the command line still fail fast: asking
// for an impossible combination in the command you just typed is a
// usage error, not a preference to be quietly ignored.
func TestExplicitFlagsStayStrict(t *testing.T) {
	isolateHome(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-l"}, "-l requires -grep"},
		{[]string{"-grep", "x", "-discover-apps"}, "-discover-apps requires -l"},
		{[]string{"-max-total-matches", "20"}, "-max-total-matches requires"},
		{[]string{"-group"}, "-group requires"},
		{[]string{"-grep", "x", "-l", "-group"}, "-group cannot be combined with -l"},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"-bucket", "b", "-prefix", "p"}, tc.args...), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v: exit %d stderr %q, want %q", tc.args, code, stderr.String(), tc.want)
		}
	}
}

// A standing default that DOES apply is honored: "l: true" with a
// file-provided pattern is a real names-only run, so -discover-apps
// (which needs -l) survives alongside it.
func TestApplicableDefaultsAreKept(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\ngrep: ERROR\nl: true\ndiscover-apps: true\n"+lateSentinel)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", p}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), lateSentinelErr) {
		t.Fatalf("applicable defaults must survive: exit %d stderr %q", code, stderr.String())
	}
}

// A CLI pattern turns a previously inapplicable "l: true" into an
// applicable one — the drop decision is made after -grep/-category
// have been resolved, not before.
func TestFileNamesOnlyAppliesOnceCLIAddsAPattern(t *testing.T) {
	p := writeTempConfig(t, "bucket: b\nprefix: logs/\nl: true\ngroup: true\n"+lateSentinel)
	var stdout, stderr bytes.Buffer
	// -l is now applicable, which makes the file's group:true
	// inapplicable — and dropped, not turned into an error.
	code := run([]string{"-config", p, "-grep", "ERROR"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), lateSentinelErr) {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}
