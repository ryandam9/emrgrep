// Package config parses and validates the emrgrep command line.
// Validation is fail-fast (M-03): every violation is reported with
// exit status 2 before any AWS call is made.
package config

import (
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/ryandam9/emrgrep/internal/scan"
)

// appIDFormat validates -app-id: a complete YARN application ID and
// nothing else, because the value becomes a literal S3 key prefix
// segment — a partial or decorated ID would silently list nothing.
var appIDFormat = regexp.MustCompile(`^application_\d+_\d+$`)

const mib = 1 << 20

// Upper bounds on size flags (in MiB), chosen far above any realistic
// need but low enough that MiB→byte conversion can never overflow
// int64 (or int on 64-bit platforms) and silently disable a budget
// (M-03).
const (
	maxSizeCapMiB         = 1 << 20 // 1 TiB
	maxUncompressedCapMiB = 4 << 20 // 4 TiB
	maxLineSizeCapMiB     = 256
)

// Options is the parsed, not-yet-validated command line.
type Options struct {
	Bucket               string
	Prefix               string
	AllowWholeBucketScan bool
	ClusterName          string
	ClusterID            string
	AppID                string

	KeyPattern  string
	GrepPattern string
	Category    string
	Cat         bool
	Download    bool
	FixedString bool
	IgnoreCase  bool

	ExtList   string
	AfterStr  string
	BeforeStr string

	MaxSizeMiB          int64
	MaxZipEntries       int
	MaxUncompressedMiB  int64
	MaxLineSizeMiB      int64
	MaxMatches          int64
	MaxTotalMatches     int64
	NamesOnly           bool
	DiscoverApps        bool
	SmallestFirst       bool
	SmallestFirstWindow int
	Workers             int
	ZipWorkers          int
	ObjectTimeout       time.Duration
	OverallTimeout      time.Duration
	RequestPayer        string
	ExpectedBucketOwner string
	SanitizeOutput      bool
	MDReport            bool
	MaxWarnings         int
	Region              string
	Profile             string
	Progress            time.Duration
	Verbose             bool
	Color               string
	Group               bool
	ConfigFile          string
}

// usageExamples is appended to the -h/-help output so the common
// workflows are discoverable without opening the README.
const usageExamples = `
Examples:
  Grep a prefix for a pattern:
    emrgrep -bucket my-emr-logs -prefix logs/j-1ABC/ -grep 'ERROR|Exception'

  Scan an EMR cluster by name (bucket and prefix come from the
  cluster's S3 log destination; no -bucket/-prefix needed):
    emrgrep -cluster-name hbase-prod -grep ERROR

  One cluster, one application (only .../containers/<app-id>/ is
  listed and downloaded — no key search across the cluster's logs):
    emrgrep -cluster-name hbase-prod -app-id application_1700000000000_0042 -grep ERROR

  Same scan, also saved as a Markdown report (matched file names, then
  matches grouped per file) under ~/logscan/<yyyy-mm-dd>/:
    emrgrep -cluster-name hbase-prod -app-id application_1700000000000_0042 -grep ERROR -md

  Patterns you use often can be named in the config file's patterns
  mapping and picked by name — no regex typing:
    emrgrep -app-id application_1700000000000_0042 -category spark

  No pattern at all: list the application's files (the default), print
  the entire logs with -cat, or store them locally with -download
  (files land under ~/logscan/<yyyy-mm-dd>/<app-id>/, as-is):
    emrgrep -app-id application_1700000000000_0042
    emrgrep -app-id application_1700000000000_0042 -cat
    emrgrep -app-id application_1700000000000_0042 -download

  Discover which application logged an error (step logs first):
    emrgrep -bucket my-emr-logs -prefix logs/j-1ABC/ \
        -grep 'Table or view not found' -F -l -discover-apps -smallest-first

  List matching keys only, downloading nothing (omit -grep):
    emrgrep -bucket my-emr-logs -prefix logs/j-1ABC/steps/

  Readable output for deep hierarchies, with live progress:
    emrgrep -bucket b -allow-whole-bucket-scan -grep kyneton -i -group -progress 2s

  Show 20 example matches, then stop downloading:
    emrgrep -bucket b -prefix logs/ -grep ERROR -max-total-matches 20

  Only objects modified on one UTC day (-after inclusive, -before exclusive):
    emrgrep -bucket b -prefix logs/ -after 2026-07-20 -before 2026-07-21 -grep ERROR

  Pick the AWS credentials with a named profile (beats $AWS_PROFILE;
  for an SSO profile, "aws sso login --profile prod-emr" first):
    emrgrep -profile prod-emr -cluster-name hbase-prod -grep ERROR

Config file:
  Standing defaults are read from ~/.config/emrgrep/config.yaml (or
  .yml; override the path with -config FILE). YAML: keys are the flag
  names; any flag given on the command line takes priority. The
  "patterns" mapping defines the named categories -category picks
  from — each name a regex or a list of regexes that OR-combine
  (quote a regex if it starts with a YAML-special character). Example:
      profile: prod-emr
      cluster-name: hbase-prod
      i: true
      progress: 2s
      patterns:
        spark:
          - ERROR|Exception
          - Caused by
        oom: OutOfMemoryError|exit code 137
  With that in place, a scan is just:
      emrgrep -app-id application_..._0042 -category spark

Exit codes:
  0 matched; 1 no matches; 2 usage/credential/listing failure;
  3 object errors, partial scans, or -overall-timeout; 130 interrupted.

Full documentation: https://github.com/ryandam9/emrgrep
`

// NewFlagSet binds all flags (§11) onto a fresh FlagSet.
func NewFlagSet(name string, out io.Writer) (*flag.FlagSet, *Options) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintf(out, "emrgrep — a resource-budgeted concurrent scanner for EMR/YARN logs in S3\n\n")
		fmt.Fprintf(out, "Usage: %s [flags]\n\nFlags:\n", name)
		fs.PrintDefaults()
		fmt.Fprint(out, usageExamples)
	}
	o := &Options{}

	fs.StringVar(&o.Bucket, "bucket", "", "S3 bucket name (required unless -cluster-name/-cluster-id derive it)")
	fs.StringVar(&o.Prefix, "prefix", "", "key prefix; required unless -allow-whole-bucket-scan or a cluster flag")
	fs.StringVar(&o.ClusterName, "cluster-name", "", "EMR cluster name; resolves the RUNNING/WAITING cluster and scopes the scan to its S3 log destination")
	fs.StringVar(&o.ClusterID, "cluster-id", "", "EMR cluster ID (j-...); scopes the scan to its S3 log destination (works for terminated clusters)")
	fs.StringVar(&o.AppID, "app-id", "", "YARN application ID (application_<ts>_<seq>); lists only .../containers/<app-id>/ — EMR's layout makes the path deterministic, no key search needed")
	fs.BoolVar(&o.AllowWholeBucketScan, "allow-whole-bucket-scan", false, "explicit opt-in to empty-prefix enumeration of the whole bucket")
	fs.StringVar(&o.KeyPattern, "key", "", "object-key filter (client-side; cuts GETs, not LIST work)")
	fs.StringVar(&o.GrepPattern, "grep", "", "content filter; omit for list-only mode (no downloads)")
	fs.StringVar(&o.Category, "category", "", "named pattern from the config file's patterns mapping; resolves to -grep so the regex never needs typing")
	fs.BoolVar(&o.Cat, "cat", false, "no pattern: download and print entire logs line by line (default without -grep/-category is listing file names only)")
	fs.BoolVar(&o.Download, "download", false, "store the application's log files locally under ~/logscan/<yyyy-mm-dd>/<app-id>/, as-is (requires -app-id; no pattern)")
	fs.BoolVar(&o.FixedString, "F", false, "-key/-grep are fixed strings, not regex")
	fs.BoolVar(&o.IgnoreCase, "i", false, "case-insensitive matching")
	fs.StringVar(&o.ExtList, "ext", "", "comma-separated extension allow-list, e.g. .gz,.log (case-insensitive)")
	fs.StringVar(&o.AfterStr, "after", "", "RFC3339 or YYYY-MM-DD (UTC midnight); inclusive; vs LastModified")
	fs.StringVar(&o.BeforeStr, "before", "", "RFC3339 or YYYY-MM-DD (UTC midnight); exclusive; vs LastModified")
	fs.Int64Var(&o.MaxSizeMiB, "max-size", 128, "compressed object size cap in MiB (0 = unlimited)")
	fs.IntVar(&o.MaxZipEntries, "max-zip-entries", 10000, "maximum entries per ZIP object")
	fs.Int64Var(&o.MaxUncompressedMiB, "max-uncompressed-object-size", 512, "cumulative ZIP expansion budget in MiB")
	fs.Int64Var(&o.MaxLineSizeMiB, "max-line-size", 4, "oversized-line truncation boundary in MiB")
	fs.Int64Var(&o.MaxMatches, "max-matches", 0, "match cap per object; whole ZIP = one object (0 = unlimited)")
	fs.Int64Var(&o.MaxTotalMatches, "max-total-matches", 0, "stop the whole run after this many matches (0 = unlimited)")
	fs.BoolVar(&o.NamesOnly, "l", false, "names only; first-hit exit; best-effort application IDs")
	fs.BoolVar(&o.DiscoverApps, "discover-apps", false, "with -l: read on until an application ID is found")
	fs.BoolVar(&o.SmallestFirst, "smallest-first", false, "windowed approximate smallest-first ordering")
	fs.IntVar(&o.SmallestFirstWindow, "smallest-first-window", 5000, "smallest-first window size in descriptors")
	fs.IntVar(&o.Workers, "workers", 16, "download/scan workers (1-256)")
	fs.IntVar(&o.ZipWorkers, "zip-workers", 2, "concurrent ZIP objects (1-workers); each holds up to -max-size temp disk")
	fs.DurationVar(&o.ObjectTimeout, "object-timeout", 0, "per-object timeout (0 = none)")
	fs.DurationVar(&o.OverallTimeout, "overall-timeout", 0, "whole-run timeout (0 = none)")
	fs.StringVar(&o.RequestPayer, "request-payer", "", `set to "requester" for requester-pays buckets`)
	fs.StringVar(&o.ExpectedBucketOwner, "expected-bucket-owner", "", "account ID the bucket must belong to (cross-account safety)")
	fs.BoolVar(&o.SanitizeOutput, "sanitize-output", true, "replace control characters in output")
	fs.BoolVar(&o.MDReport, "md", false, "write a Markdown report to ~/logscan/<yyyy-mm-dd>/<app-id>.md: matched file names, matches grouped per file, and the run summary (requires -app-id and -grep)")
	fs.IntVar(&o.MaxWarnings, "max-warnings", 100, "stderr warning cap (0 = unlimited)")
	fs.StringVar(&o.Region, "region", "", "AWS region override")
	fs.StringVar(&o.Profile, "profile", "", "named profile from ~/.aws/config and ~/.aws/credentials; overrides $AWS_PROFILE (default: the standard credential chain)")
	fs.DurationVar(&o.Progress, "progress", 0, "print a status line to stderr every interval, e.g. 2s (0 = off)")
	fs.BoolVar(&o.Verbose, "verbose", false, "log each listing page and each object as scanning starts (stderr)")
	fs.StringVar(&o.Color, "color", "auto", `colorize results: "auto" (only when stdout is a terminal), "always", or "never"`)
	fs.BoolVar(&o.Group, "group", false, "print each object key once as a heading with its matches indented below (default: on when stdout is a terminal; -group=false forces flat lines)")
	fs.StringVar(&o.ConfigFile, "config", "", "YAML config file with standing defaults (default: ~/.config/emrgrep/config.yaml or .yml if present); command-line flags take priority")
	return fs, o
}

// Build validates o and produces the engine configuration. All
// violations are usage errors (exit 2).
func (o *Options) Build() (*scan.Config, error) {
	if o.ClusterName != "" && o.ClusterID != "" {
		return nil, fmt.Errorf("-cluster-name and -cluster-id are mutually exclusive")
	}
	if o.ClusterID != "" && !strings.HasPrefix(o.ClusterID, "j-") {
		return nil, fmt.Errorf("-cluster-id must look like j-XXXXXXXXXXXXX, got %q", o.ClusterID)
	}
	cluster := o.ClusterName != "" || o.ClusterID != ""
	if o.AppID != "" {
		if !appIDFormat.MatchString(o.AppID) {
			return nil, fmt.Errorf("-app-id must look like application_<timestamp>_<sequence> (e.g. application_1700000000000_0042), got %q", o.AppID)
		}
		if !cluster && o.Prefix == "" {
			return nil, fmt.Errorf("-app-id needs the cluster's log directory to build .../containers/%s/; pass -cluster-name/-cluster-id, or a -prefix that points at it", o.AppID)
		}
	}
	// By Build time -category has been resolved into GrepPattern (main
	// owns that, since the named patterns live in the config file), so
	// -cat conflicting with either shows up as a conflict with the
	// resolved pattern.
	if o.Cat && o.GrepPattern != "" {
		return nil, fmt.Errorf("-cat prints entire logs; it cannot be combined with -grep or -category")
	}
	if o.Download {
		if o.GrepPattern != "" || o.Cat {
			return nil, fmt.Errorf("-download stores raw log files; it cannot be combined with -grep, -category, or -cat")
		}
		if o.AppID == "" {
			return nil, fmt.Errorf("-download requires -app-id (files are stored under ~/logscan/<yyyy-mm-dd>/<app-id>/)")
		}
	}
	if o.MDReport {
		if o.AppID == "" {
			return nil, fmt.Errorf("-md requires -app-id (the report file is named ~/logscan/<yyyy-mm-dd>/<app-id>.md)")
		}
		if o.GrepPattern == "" {
			return nil, fmt.Errorf("-md requires -grep or -category (the report records where a search pattern was found)")
		}
	}
	if o.Bucket == "" && !cluster {
		return nil, fmt.Errorf("-bucket is required (or pass -cluster-name/-cluster-id to derive it from the cluster's S3 log destination)")
	}
	// A cluster flag always narrows the prefix to .../<cluster-id>/,
	// so the whole-bucket guard does not apply.
	if o.Prefix == "" && !o.AllowWholeBucketScan && !cluster {
		return nil, fmt.Errorf("-prefix is required; pass -allow-whole-bucket-scan to enumerate the whole bucket (listing time is proportional to key count)")
	}
	if o.Workers < 1 || o.Workers > 256 {
		return nil, fmt.Errorf("-workers must be in 1-256, got %d", o.Workers)
	}
	if o.ZipWorkers < 1 || o.ZipWorkers > o.Workers {
		return nil, fmt.Errorf("-zip-workers must be in 1-%d (workers), got %d", o.Workers, o.ZipWorkers)
	}
	if o.SmallestFirstWindow < 1 {
		return nil, fmt.Errorf("-smallest-first-window must be >= 1, got %d", o.SmallestFirstWindow)
	}
	if o.MaxSizeMiB < 0 || o.MaxUncompressedMiB < 0 || o.MaxMatches < 0 || o.MaxTotalMatches < 0 || o.MaxWarnings < 0 {
		return nil, fmt.Errorf("size, match, and warning limits must be >= 0")
	}
	if o.MaxTotalMatches > 0 && o.GrepPattern == "" && !o.Cat {
		return nil, fmt.Errorf("-max-total-matches requires -grep, -category, or -cat")
	}
	if o.Group && o.GrepPattern == "" && !o.Cat {
		return nil, fmt.Errorf("-group requires -grep, -category, or -cat (list-only output is already one key per line)")
	}
	if o.Group && o.NamesOnly {
		return nil, fmt.Errorf("-group cannot be combined with -l (names-only output is already one key per line)")
	}
	if o.MaxSizeMiB > maxSizeCapMiB {
		return nil, fmt.Errorf("-max-size must be <= %d MiB (1 TiB), got %d", int64(maxSizeCapMiB), o.MaxSizeMiB)
	}
	if o.MaxUncompressedMiB > maxUncompressedCapMiB {
		return nil, fmt.Errorf("-max-uncompressed-object-size must be <= %d MiB (4 TiB), got %d", int64(maxUncompressedCapMiB), o.MaxUncompressedMiB)
	}
	if o.MaxLineSizeMiB < 1 || o.MaxLineSizeMiB > maxLineSizeCapMiB {
		return nil, fmt.Errorf("-max-line-size must be in 1-%d MiB, got %d", maxLineSizeCapMiB, o.MaxLineSizeMiB)
	}
	if o.MaxZipEntries < 1 {
		return nil, fmt.Errorf("-max-zip-entries must be >= 1, got %d", o.MaxZipEntries)
	}
	if o.DiscoverApps && !o.NamesOnly {
		return nil, fmt.Errorf("-discover-apps requires -l")
	}
	if o.RequestPayer != "" && o.RequestPayer != "requester" {
		return nil, fmt.Errorf(`-request-payer accepts only "requester", got %q`, o.RequestPayer)
	}
	if o.ObjectTimeout < 0 || o.OverallTimeout < 0 {
		return nil, fmt.Errorf("timeouts must be >= 0")
	}
	if o.Progress < 0 {
		return nil, fmt.Errorf("-progress must be >= 0")
	}
	if o.Progress > 0 && o.Progress < 100*time.Millisecond {
		return nil, fmt.Errorf("-progress interval must be at least 100ms, got %s", o.Progress)
	}
	if o.Color != "auto" && o.Color != "always" && o.Color != "never" {
		return nil, fmt.Errorf(`-color must be "auto", "always", or "never", got %q`, o.Color)
	}
	// A profile name is matched literally against the shared config
	// file's section names, so surrounding whitespace never resolves.
	// Rejecting it here turns a silent "profile not found" — which on
	// a whitespace-only value would read as if no profile were given
	// at all — into a usage error that names the value (M-03).
	if o.Profile != strings.TrimSpace(o.Profile) {
		return nil, fmt.Errorf("-profile must not have leading or trailing whitespace, got %q", o.Profile)
	}

	cfg := &scan.Config{
		Bucket:              o.Bucket,
		Prefix:              o.Prefix,
		ListOnly:            o.GrepPattern == "" && !o.Cat && !o.Download,
		SmallestFirst:       o.SmallestFirst,
		SmallestFirstWindow: o.SmallestFirstWindow,
		Workers:             o.Workers,
		ZipWorkers:          o.ZipWorkers,
		ObjectTimeout:       o.ObjectTimeout,
		OverallTimeout:      o.OverallTimeout,
		RequestPayer:        o.RequestPayer == "requester",
		ExpectedBucketOwner: o.ExpectedBucketOwner,
		SanitizeOutput:      o.SanitizeOutput,
		MaxWarnings:         o.MaxWarnings,
		MaxTotalMatches:     o.MaxTotalMatches,
		GroupOutput:         o.Group,
		Progress:            o.Progress,
		Verbose:             o.Verbose,
		CollectMatchedKeys:  o.MDReport,
	}

	cfg.Filters.MaxSize = o.MaxSizeMiB * mib
	if o.KeyPattern != "" {
		m, err := scan.NewMatcher(o.KeyPattern, o.FixedString, o.IgnoreCase)
		if err != nil {
			return nil, fmt.Errorf("-key: %w", err)
		}
		cfg.Filters.KeyMatcher = m
	}
	if o.GrepPattern != "" {
		m, err := scan.NewMatcher(o.GrepPattern, o.FixedString, o.IgnoreCase)
		if err != nil {
			return nil, fmt.Errorf("-grep: %w", err)
		}
		cfg.Scan.Grep = m
	}
	if o.Cat {
		// Full-log dump: an empty regex matches every line, so the
		// entire scan pipeline (workers, budgets, grouping, counters)
		// applies unchanged. CatMode suppresses match highlighting —
		// there is no pattern to highlight.
		m, err := scan.NewMatcher("", false, false)
		if err != nil {
			return nil, fmt.Errorf("-cat: %w", err)
		}
		cfg.Scan.Grep = m
		cfg.Scan.CatMode = true
	}
	if o.NamesOnly && cfg.ListOnly {
		return nil, fmt.Errorf("-l requires -grep (names of objects with content matches)")
	}
	if o.DiscoverApps && cfg.ListOnly {
		return nil, fmt.Errorf("-discover-apps requires -grep")
	}

	exts, err := parseExtList(o.ExtList)
	if err != nil {
		return nil, err
	}
	cfg.Filters.Extensions = exts

	if o.AfterStr != "" {
		t, err := parseTimeFlag(o.AfterStr)
		if err != nil {
			return nil, fmt.Errorf("-after: %w", err)
		}
		cfg.Filters.After, cfg.Filters.HasAfter = t, true
	}
	if o.BeforeStr != "" {
		t, err := parseTimeFlag(o.BeforeStr)
		if err != nil {
			return nil, fmt.Errorf("-before: %w", err)
		}
		cfg.Filters.Before, cfg.Filters.HasBefore = t, true
	}
	if cfg.Filters.HasAfter && cfg.Filters.HasBefore && !cfg.Filters.After.Before(cfg.Filters.Before) {
		return nil, fmt.Errorf("-after (%s) must be earlier than -before (%s)", cfg.Filters.After.Format(time.RFC3339), cfg.Filters.Before.Format(time.RFC3339))
	}

	cfg.Scan.MaxMatches = o.MaxMatches
	cfg.Scan.MaxLineSize = int(o.MaxLineSizeMiB * mib)
	cfg.Scan.NamesOnly = o.NamesOnly
	cfg.Scan.DiscoverApps = o.DiscoverApps
	cfg.Scan.MaxZipEntries = o.MaxZipEntries
	cfg.Scan.MaxZipExpandedBytes = o.MaxUncompressedMiB * mib
	return cfg, nil
}

// parseExtList validates a comma-separated extension allow-list;
// entries are lower-cased and must begin with a dot.
func parseExtList(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("-ext: empty entry in %q", s)
		}
		if !strings.HasPrefix(p, ".") {
			return nil, fmt.Errorf("-ext: %q must start with a dot (e.g. .gz)", p)
		}
		out = append(out, strings.ToLower(p))
	}
	return out, nil
}

// parseTimeFlag accepts RFC3339 (with offsets, for precision) or
// YYYY-MM-DD, which means UTC midnight (M-02).
func parseTimeFlag(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is neither RFC3339 nor YYYY-MM-DD", s)
}
