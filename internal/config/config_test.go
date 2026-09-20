package config

import (
	"io"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, args ...string) (*Options, error) {
	t.Helper()
	fs, o := NewFlagSet("test", io.Discard)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	return o, nil
}

func build(t *testing.T, args ...string) error {
	t.Helper()
	o, _ := parse(t, args...)
	_, err := o.Build()
	return err
}

func TestValidationFailFast(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // substring of the error
	}{
		{"missing bucket", []string{}, "-bucket is required"},
		{"empty prefix", []string{"-bucket", "b"}, "allow-whole-bucket-scan"},
		{"workers low", []string{"-bucket", "b", "-prefix", "p", "-workers", "0"}, "-workers"},
		{"workers high", []string{"-bucket", "b", "-prefix", "p", "-workers", "257"}, "-workers"},
		{"zip workers above workers", []string{"-bucket", "b", "-prefix", "p", "-workers", "2", "-zip-workers", "3"}, "-zip-workers"},
		{"window zero", []string{"-bucket", "b", "-prefix", "p", "-smallest-first-window", "0"}, "-smallest-first-window"},
		{"discover without l", []string{"-bucket", "b", "-prefix", "p", "-grep", "x", "-discover-apps"}, "-discover-apps requires -l"},
		{"l without grep", []string{"-bucket", "b", "-prefix", "p", "-l"}, "-l requires -grep"},
		{"bad regex", []string{"-bucket", "b", "-prefix", "p", "-grep", "("}, "-grep"},
		{"bad key regex", []string{"-bucket", "b", "-prefix", "p", "-key", "["}, "-key"},
		{"bad ext no dot", []string{"-bucket", "b", "-prefix", "p", "-ext", "gz"}, "must start with a dot"},
		{"bad ext empty entry", []string{"-bucket", "b", "-prefix", "p", "-ext", ".gz,,.log"}, "empty entry"},
		{"bad after", []string{"-bucket", "b", "-prefix", "p", "-after", "yesterday"}, "-after"},
		{"after not before before", []string{"-bucket", "b", "-prefix", "p", "-after", "2026-07-02", "-before", "2026-07-01"}, "must be earlier"},
		{"bad request payer", []string{"-bucket", "b", "-prefix", "p", "-request-payer", "owner"}, "-request-payer"},
		{"line size zero", []string{"-bucket", "b", "-prefix", "p", "-max-line-size", "0"}, "-max-line-size"},
		// M-03: absurd values are rejected before MiB conversion can
		// overflow and silently disable a budget.
		{"max-size overflow", []string{"-bucket", "b", "-prefix", "p", "-max-size", "9223372036854775807"}, "-max-size"},
		{"max-size above cap", []string{"-bucket", "b", "-prefix", "p", "-max-size", "1048577"}, "-max-size"},
		{"uncompressed above cap", []string{"-bucket", "b", "-prefix", "p", "-max-uncompressed-object-size", "4194305"}, "-max-uncompressed-object-size"},
		{"line size above cap", []string{"-bucket", "b", "-prefix", "p", "-max-line-size", "257"}, "-max-line-size"},
		{"group without grep", []string{"-bucket", "b", "-prefix", "p", "-group"}, "-group requires -grep"},
		// A flag that cannot affect the run at hand is a usage error,
		// not something to accept and ignore: -download reads nothing,
		// so a content-match selector and a per-object match cap have
		// nothing to act on.
		{"download with names-only", []string{"-bucket", "b", "-prefix", "p", "-app-id", "application_1_2", "-download", "-l"}, "cannot be combined with -l"},
		{"max-matches without a pattern", []string{"-bucket", "b", "-prefix", "p", "-max-matches", "5"}, "-max-matches requires -grep"},
		{"max-matches with download", []string{"-bucket", "b", "-prefix", "p", "-app-id", "application_1_2", "-download", "-max-matches", "5"}, "-max-matches requires -grep"},
		{"group with names-only", []string{"-bucket", "b", "-prefix", "p", "-grep", "x", "-l", "-group"}, "-group cannot be combined with -l"},
		{"both cluster flags", []string{"-cluster-name", "hbase", "-cluster-id", "j-1A"}, "mutually exclusive"},
		{"bad cluster id", []string{"-cluster-id", "1ABC"}, "-cluster-id must look like"},
		{"bad app id", []string{"-cluster-name", "hbase", "-app-id", "app_123"}, "-app-id must look like"},
		{"app id with trailing junk", []string{"-cluster-name", "hbase", "-app-id", "application_1_2.log"}, "-app-id must look like"},
		{"app id without cluster or prefix", []string{"-bucket", "b", "-allow-whole-bucket-scan", "-app-id", "application_1700000000000_0042"}, "-app-id needs the cluster's log directory"},
		{"md without app id", []string{"-bucket", "b", "-prefix", "p", "-grep", "x", "-md"}, "-md requires -app-id"},
		{"md without grep", []string{"-cluster-name", "hbase", "-app-id", "application_1_2", "-md"}, "-md requires -grep"},
		// A profile name is matched literally against the shared
		// config's section names, so padded values never resolve.
		{"padded profile", []string{"-bucket", "b", "-prefix", "p", "-profile", " prod-emr"}, "-profile must not have leading or trailing whitespace"},
		{"trailing-space profile", []string{"-bucket", "b", "-prefix", "p", "-profile", "prod-emr "}, "-profile must not have leading or trailing whitespace"},
		{"whitespace-only profile", []string{"-bucket", "b", "-prefix", "p", "-profile", "   "}, "-profile must not have leading or trailing whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := build(t, tc.args...)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidConfigs(t *testing.T) {
	if err := build(t, "-bucket", "b", "-prefix", "logs/", "-grep", "ERROR"); err != nil {
		t.Fatalf("minimal grep config: %v", err)
	}
	if err := build(t, "-bucket", "b", "-prefix", "logs/"); err != nil {
		t.Fatalf("list-only config: %v", err)
	}
	if err := build(t, "-bucket", "b", "-allow-whole-bucket-scan", "-grep", "x"); err != nil {
		t.Fatalf("whole-bucket opt-in: %v", err)
	}
	if err := build(t, "-bucket", "b", "-prefix", "p", "-grep", "x", "-l", "-discover-apps"); err != nil {
		t.Fatalf("-l -discover-apps: %v", err)
	}
	// Cluster flags waive -bucket and the whole-bucket prefix guard:
	// the scan is scoped to the cluster's log destination.
	if err := build(t, "-cluster-name", "hbase-prod", "-grep", "x"); err != nil {
		t.Fatalf("cluster-name without bucket/prefix: %v", err)
	}
	if err := build(t, "-cluster-id", "j-1ABC2DEF", "-grep", "x"); err != nil {
		t.Fatalf("cluster-id without bucket/prefix: %v", err)
	}
	if err := build(t, "-bucket", "b", "-prefix", "logs/", "-cluster-name", "hbase-prod", "-grep", "x"); err != nil {
		t.Fatalf("cluster-name with explicit bucket/prefix: %v", err)
	}
	// -app-id rides on either a cluster flag or an explicit prefix
	// pointing at the cluster's log directory.
	if err := build(t, "-cluster-name", "hbase-prod", "-app-id", "application_1700000000000_0042", "-grep", "x"); err != nil {
		t.Fatalf("app-id with cluster-name: %v", err)
	}
	if err := build(t, "-bucket", "b", "-prefix", "logs/j-1ABC/", "-app-id", "application_1700000000000_0042"); err != nil {
		t.Fatalf("app-id with explicit prefix, list-only: %v", err)
	}
	// -md rides on -app-id (file name) and -grep (the search), and
	// flips on matched-key collection in the engine config.
	o, _ := parse(t, "-cluster-name", "hbase-prod", "-app-id", "application_1_2", "-grep", "x", "-md")
	cfg, err := o.Build()
	if err != nil {
		t.Fatalf("-md with app-id and grep: %v", err)
	}
	if !cfg.CollectMatchedKeys {
		t.Fatal("-md must set CollectMatchedKeys")
	}
}

// -profile is independent of every other flag: it selects credentials
// and places no constraint on the scan, and an omitted -profile leaves
// the standard credential chain in play.
func TestProfileFlag(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "logs/", "-grep", "x", "-profile", "prod-emr")
	if _, err := o.Build(); err != nil {
		t.Fatalf("-profile with a valid scan: %v", err)
	}
	if o.Profile != "prod-emr" {
		t.Errorf("Profile = %q, want prod-emr", o.Profile)
	}
	// Profile names in the wild carry dots, dashes and underscores.
	for _, name := range []string{"prod-emr", "my.profile", "acct_123", "default"} {
		if err := build(t, "-bucket", "b", "-prefix", "p", "-profile", name); err != nil {
			t.Errorf("-profile %q rejected: %v", name, err)
		}
	}
	// -profile and -region compose: both reach the SDK loader.
	o, _ = parse(t, "-bucket", "b", "-prefix", "p", "-profile", "prod-emr", "-region", "ap-southeast-2")
	if err := func() error { _, err := o.Build(); return err }(); err != nil {
		t.Fatalf("-profile with -region: %v", err)
	}
	if o.Profile != "prod-emr" || o.Region != "ap-southeast-2" {
		t.Errorf("got profile %q region %q", o.Profile, o.Region)
	}
	// Default: no profile, so the credential chain is untouched.
	o, _ = parse(t, "-bucket", "b", "-prefix", "p")
	if o.Profile != "" {
		t.Errorf("default Profile = %q, want empty", o.Profile)
	}
}

// M-02: date-only means UTC midnight; RFC3339 offsets are honored.
func TestTimeParsing(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-after", "2026-07-01", "-before", "2026-07-22T10:30:00+02:00")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	wantAfter := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !cfg.Filters.After.Equal(wantAfter) {
		t.Fatalf("after: %v want %v", cfg.Filters.After, wantAfter)
	}
	wantBefore := time.Date(2026, 7, 22, 8, 30, 0, 0, time.UTC)
	if !cfg.Filters.Before.Equal(wantBefore) {
		t.Fatalf("before: %v want %v", cfg.Filters.Before, wantBefore)
	}
}

func TestUnitConversionAndDefaults(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-grep", "x")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Filters.MaxSize != 128<<20 {
		t.Fatalf("default max-size: %d", cfg.Filters.MaxSize)
	}
	if cfg.Scan.MaxLineSize != 4<<20 {
		t.Fatalf("default max-line-size: %d", cfg.Scan.MaxLineSize)
	}
	if cfg.Scan.MaxZipExpandedBytes != 512<<20 {
		t.Fatalf("default expansion budget: %d", cfg.Scan.MaxZipExpandedBytes)
	}
	if cfg.Workers != 16 || cfg.ZipWorkers != 2 || cfg.SmallestFirstWindow != 5000 {
		t.Fatalf("defaults: workers=%d zip=%d window=%d", cfg.Workers, cfg.ZipWorkers, cfg.SmallestFirstWindow)
	}
	if !cfg.SanitizeOutput {
		t.Fatal("sanitize must default on")
	}
}

func TestExtListNormalization(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-ext", ".GZ, .Log")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Filters.Extensions) != 2 || cfg.Filters.Extensions[0] != ".gz" || cfg.Filters.Extensions[1] != ".log" {
		t.Fatalf("extensions: %v", cfg.Filters.Extensions)
	}
}

func TestZeroMeansUnlimited(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-grep", "x", "-max-size", "0", "-max-matches", "0")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Filters.MaxSize != 0 || cfg.Scan.MaxMatches != 0 {
		t.Fatalf("zero must mean unlimited: %d %d", cfg.Filters.MaxSize, cfg.Scan.MaxMatches)
	}
}

// -cat turns a patternless run into a full-content dump: not
// list-only, a match-everything matcher, highlighting suppressed.
func TestCatMode(t *testing.T) {
	o, _ := parse(t, "-bucket", "b", "-prefix", "p", "-cat")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListOnly || cfg.Scan.Grep == nil || !cfg.Scan.CatMode {
		t.Fatalf("listOnly=%v grep=%v catMode=%v", cfg.ListOnly, cfg.Scan.Grep, cfg.Scan.CatMode)
	}
	if !cfg.Scan.Grep.Match([]byte("anything at all")) || !cfg.Scan.Grep.Match([]byte("")) {
		t.Fatal("-cat matcher must match every line, empty included")
	}
	// Budgets and grouping that require "a content scan" accept -cat.
	if err := build(t, "-bucket", "b", "-prefix", "p", "-cat", "-max-total-matches", "100", "-group"); err != nil {
		t.Fatalf("-cat with caps and grouping: %v", err)
	}
}

func TestCatConflicts(t *testing.T) {
	if err := build(t, "-bucket", "b", "-prefix", "p", "-cat", "-grep", "x"); err == nil || !strings.Contains(err.Error(), "-cat prints entire logs") {
		t.Fatalf("-cat with -grep: %v", err)
	}
	if err := build(t, "-cluster-name", "h", "-app-id", "application_1_2", "-cat", "-md"); err == nil || !strings.Contains(err.Error(), "-md requires -grep or -category") {
		t.Fatalf("-md with -cat: %v", err)
	}
}

// -download stores raw files: app-scoped, patternless.
func TestDownloadValidation(t *testing.T) {
	o, _ := parse(t, "-cluster-name", "h", "-app-id", "application_1_2", "-download")
	cfg, err := o.Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListOnly {
		t.Fatal("-download must not be list-only (workers must run)")
	}
	if err := build(t, "-bucket", "b", "-prefix", "p", "-download"); err == nil || !strings.Contains(err.Error(), "-download requires -app-id") {
		t.Fatalf("download without app-id: %v", err)
	}
	if err := build(t, "-cluster-name", "h", "-app-id", "application_1_2", "-download", "-grep", "x"); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("download with grep: %v", err)
	}
	if err := build(t, "-cluster-name", "h", "-app-id", "application_1_2", "-download", "-cat"); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("download with cat: %v", err)
	}
	// -download reads nothing, so a content-match selector has
	// nothing to select on. Accepting it silently would look like a
	// filter that ran and matched everything.
	if err := build(t, "-cluster-name", "h", "-app-id", "application_1_2", "-download", "-l"); err == nil || !strings.Contains(err.Error(), "cannot be combined with -l") {
		t.Fatalf("download with -l: %v", err)
	}
}

// The two match caps behave identically: both count matches, so both
// need something producing matches. Before, only the run-wide one
// said so and -max-matches was accepted and ignored.
func TestMatchCapsRequireAPattern(t *testing.T) {
	for _, flag := range []string{"-max-matches", "-max-total-matches"} {
		if err := build(t, "-bucket", "b", "-prefix", "p", flag, "5"); err == nil ||
			!strings.Contains(err.Error(), flag+" requires -grep") {
			t.Errorf("%s on a list-only run: %v", flag, err)
		}
		if err := build(t, "-bucket", "b", "-prefix", "p", "-grep", "x", flag, "5"); err != nil {
			t.Errorf("%s with -grep must be accepted: %v", flag, err)
		}
		if err := build(t, "-bucket", "b", "-prefix", "p", "-cat", flag, "5"); err != nil {
			t.Errorf("%s with -cat must be accepted: %v", flag, err)
		}
	}
}
