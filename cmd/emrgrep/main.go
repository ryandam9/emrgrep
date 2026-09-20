// Command emrgrep is a resource-budgeted concurrent scanner for
// EMR/YARN logs stored in S3.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/emr"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ryandam9/emrgrep/internal/config"
	"github.com/ryandam9/emrgrep/internal/scan"
)

// Injected at build time via -ldflags (see Makefile).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without process-global state, so exit codes and the
// -h/-version paths are testable (M-04).
func run(args []string, stdout, stderr io.Writer) int {
	fs, opts := config.NewFlagSet("emrgrep", stderr)
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // help is an informational success, not a usage error
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "emrgrep %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}
	// Config file: standing defaults (cluster name, patterns, -i, ...)
	// read as YAML from -config or ~/.config/emrgrep/config.yaml,
	// applied only to flags NOT given on the command line — CLI always
	// takes priority.
	cliSet := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { cliSet[f.Name] = true })
	fileSet := map[string]bool{}
	var filePatterns map[string][]string
	cfgFile := opts.ConfigFile
	if cfgFile == "" {
		cfgFile = config.DefaultConfigPath() // "" when absent: optional
	}
	if cfgFile != "" {
		var err error
		fileSet, filePatterns, err = config.ApplyFile(fs, cfgFile, func(k string) bool { return cliSet[k] })
		if err != nil {
			fmt.Fprintf(stderr, "emrgrep: %v\n", err)
			return 2
		}
	}

	// -category names a config-file pattern (pattern.<name> = <regex>)
	// and resolves it into -grep. Priority mirrors the flag rules: both
	// given on the command line is a contradiction; between sources the
	// command line wins; a config file supplying both grep and category
	// as standing defaults is ambiguous and fails fast.
	if opts.Category != "" {
		switch {
		case cliSet["category"] && cliSet["grep"]:
			fmt.Fprintf(stderr, "emrgrep: -category and -grep are mutually exclusive (the category resolves to a pattern)\n")
			return 2
		case !cliSet["category"] && cliSet["grep"]:
			opts.Category = "" // CLI -grep beats a file-provided category default
		case !cliSet["category"] && fileSet["grep"]:
			fmt.Fprintf(stderr, "emrgrep: the config file sets both grep and category; keep one standing default\n")
			return 2
		}
	}
	if opts.Category != "" {
		if opts.FixedString {
			fmt.Fprintf(stderr, "emrgrep: -F cannot be combined with -category (config-file patterns are always regular expressions)\n")
			return 2
		}
		pattern, err := config.ResolveCategory(opts.Category, filePatterns)
		if err != nil {
			fmt.Fprintf(stderr, "emrgrep: %v\n", err)
			return 2
		}
		opts.GrepPattern = pattern
	}

	// Standing defaults are preferences, not demands. A value that
	// reached an option from the config file is dropped whenever the
	// run it landed on cannot use it, so one config file serves
	// list-only, app-scoped, and cluster-wide runs alike. The same
	// option given explicitly on the command line keeps strict
	// validation and still fails fast — asking for something
	// impossible in the command you just typed is a usage error.
	fromFile := func(name string) bool { return !cliSet[name] && fileSet[name] }

	// "cat = true" applies when no pattern narrows the content and is
	// ignored when one is in play.
	if opts.Cat && fromFile("cat") && opts.GrepPattern != "" {
		opts.Cat = false
	}
	// "download: true" applies only to patternless app-scoped runs.
	if opts.Download && fromFile("download") && (opts.AppID == "" || opts.GrepPattern != "" || opts.Cat) {
		opts.Download = false
	}
	// "md = true" applies when the run is app-scoped with a pattern
	// (-app-id and -grep present).
	if opts.MDReport && fromFile("md") && (opts.AppID == "" || opts.GrepPattern == "") {
		opts.MDReport = false
	}

	// -l, -discover-apps, and -max-total-matches all need content to
	// scan, and -discover-apps additionally needs -l. As standing
	// defaults they drop out of runs that cannot use them; without
	// this a config file holding "l: true" would make every plain
	// listing run fail with "-l requires -grep".
	contentRun := opts.GrepPattern != "" || opts.Cat
	if opts.NamesOnly && fromFile("l") && (!contentRun || opts.Download) {
		opts.NamesOnly = false
	}
	if opts.DiscoverApps && fromFile("discover-apps") && !opts.NamesOnly {
		opts.DiscoverApps = false
	}
	if opts.MaxTotalMatches > 0 && fromFile("max-total-matches") && !contentRun {
		opts.MaxTotalMatches = 0
	}
	if opts.MaxMatches > 0 && fromFile("max-matches") && !contentRun {
		opts.MaxMatches = 0
	}

	// -group defaults on for humans: when the command line did not
	// choose, group whenever grouping applies — because the config
	// file asked for it, or because stdout is a terminal.
	// Piped/redirected output keeps the stable single-line format.
	opts.Group = resolveGroup(cliSet["group"], fileSet["group"], opts.Group,
		contentRun, opts.NamesOnly, isTerminal(stdout))

	cfg, err := opts.Build()
	if err != nil {
		fmt.Fprintf(stderr, "emrgrep: %v\n", err)
		return 2
	}
	cfg.Scan.TempDir = os.TempDir()
	cfg.ColorOutput = resolveColor(opts.Color, stdout)

	// Prompt reaction to SIGINT/SIGTERM: cancel everything, still
	// print the summary, exit 130 (§10). Only signal cancellation
	// lives on this context; -overall-timeout is layered inside the
	// engine so the two remain distinguishable (H-02).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	awsCfg, err := loadAWSConfig(ctx, awsLoadOptions(opts)...)
	if err != nil {
		fmt.Fprintf(stderr, "emrgrep: loading AWS configuration: %v\n", err)
		return 2
	}
	// A region has to come from somewhere before any client is built.
	// The SDK takes it from -region, the environment, or the profile's
	// own "region =" line — which it reads from ~/.aws/credentials as
	// readily as from ~/.aws/config. With none of those, the EMR client
	// would fail with a bare SDK error and the S3 path would quietly
	// probe us-east-1, so say plainly what is missing and where it goes.
	if awsCfg.Region == "" {
		fmt.Fprintf(stderr, "emrgrep: profile %q has no region; add a \"region = <aws-region>\" line to its "+
			"section in ~/.aws/credentials or ~/.aws/config, or pass -region <aws-region>\n", opts.Profile)
		return 2
	}
	// Cluster scoping: -cluster-name resolves EVERY running/waiting
	// cluster with that name — same-name clusters are siblings, and
	// the application under investigation may live on any of them —
	// and each cluster contributes one scan scope with its own log
	// destination. -cluster-id contributes exactly one.
	scopes := []scanScope{{Bucket: cfg.Bucket, Prefix: cfg.Prefix}}
	scopeFailures := false
	if opts.ClusterName != "" || opts.ClusterID != "" {
		emrClient := emr.NewFromConfig(awsCfg)
		clusters := []clusterMatch{{ID: opts.ClusterID}}
		if opts.ClusterName != "" {
			var err error
			clusters, err = resolveClusters(ctx, emrClient, opts.ClusterName)
			if err != nil {
				fmt.Fprintf(stderr, "emrgrep: %v\n", err)
				return 2
			}
			if len(clusters) > 1 {
				fmt.Fprintf(stderr, "emrgrep: %d running/waiting clusters named %q; scanning all of them:\n",
					len(clusters), opts.ClusterName)
				for _, m := range clusters {
					fmt.Fprintf(stderr, "emrgrep:   %s\n", m)
				}
			}
		}
		scopes, scopeFailures = clusterScopes(ctx, emrClient, clusters, opts.Bucket, opts.Prefix, stderr)
		if len(scopes) == 0 {
			return 2
		}
	}
	// -app-id narrows every scope to the application's container-log
	// directory. A scope belonging to a different same-name cluster
	// simply lists zero keys — one cheap LIST, no downloads.
	if opts.AppID != "" {
		for i := range scopes {
			scopes[i].Prefix = joinAppPrefix(scopes[i].Prefix, opts.AppID)
		}
	}

	// -md: record every match structurally (RecordMatch below) plus the
	// scope echoes and per-scope summaries, then render the report when
	// the run ends (interrupted runs included — found matches are never
	// lost). Matches are grouped per file in the report, independent of
	// whether the screen used grouped or flat format.
	var mdBuf bytes.Buffer // scope echoes + summaries
	var mdScopes, mdMatched []string
	var mdMatches []mdMatch
	var mdDropped int64 // matches past mdMaxMatches: counted, not kept
	writeReport := func() (failed bool) {
		if !opts.MDReport {
			return false
		}
		patternDisplay := opts.GrepPattern
		if opts.Category != "" {
			patternDisplay = opts.Category + " → " + opts.GrepPattern
		}
		// One clock reading names the date directory and stamps the
		// Generated header, so the two can never straddle midnight.
		now := time.Now()
		path, err := reportPath(opts.AppID, now)
		if err == nil {
			err = writeMDReport(path, opts.AppID, patternDisplay, mdScopes, mdMatched, mdMatches, mdDropped, mdBuf.String(), now)
		}
		if err != nil {
			fmt.Fprintf(stderr, "emrgrep: %v\n", err)
			return true
		}
		fmt.Fprintf(stderr, "emrgrep: report written to %s\n", path)
		if mdDropped > 0 {
			fmt.Fprintf(stderr, "emrgrep: the report lists the first %d matches; %d more matched and are not in it "+
				"(narrow the pattern, or bound the scan with -max-total-matches)\n", mdMaxMatches, mdDropped)
		}
		return false
	}

	// -download: one destination for the whole run; every scope stores
	// into it (an application lives on one cluster, so same-name
	// sibling scopes contribute nothing but an empty listing).
	downloadDest := ""
	if opts.Download {
		var err error
		downloadDest, err = downloadDir(opts.AppID, time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "emrgrep: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "emrgrep: downloading to %s\n", downloadDest)
	}

	// One engine run per scope, sequentially. -max-total-matches is a
	// budget across ALL scopes: each run gets what the previous runs
	// left over. Exit codes combine by severity (2 > 3 > 0 > 1);
	// interruption stops everything immediately.
	worst := -1
	if scopeFailures {
		worst = 2
	}
	remaining := cfg.MaxTotalMatches // 0 = unlimited
	// Same-name sibling clusters usually share one log bucket, so the
	// region probe is answered once per bucket rather than per scope.
	bucketRegions := map[string]string{}
	for _, sc := range scopes {
		if cfg.MaxTotalMatches > 0 && remaining <= 0 {
			break // global match budget exhausted
		}
		runCfg := *cfg
		runCfg.Bucket = sc.Bucket
		runCfg.Prefix = sc.Prefix
		runCfg.MaxTotalMatches = remaining
		runCfg.DownloadDir = downloadDest

		// Unless -region was given explicitly, resolve each bucket's
		// actual region (log destinations can differ per cluster) so
		// cross-region buckets work without configuration. Detection
		// failures fall back to the configured region.
		scopeAWS := awsCfg.Copy()
		if opts.Region == "" {
			region, cached := bucketRegions[runCfg.Bucket]
			if !cached {
				// "" is cached too: a bucket whose region cannot be
				// discovered must not be probed again per scope.
				region, _ = resolveBucketRegion(ctx, awsCfg, runCfg.Bucket, opts.ExpectedBucketOwner)
				bucketRegions[runCfg.Bucket] = region
			}
			if region != "" {
				scopeAWS.Region = region
			}
		}
		// Echo the exact scope every LIST and GET will be confined to,
		// so a run always shows where it is looking — including the
		// containers/<app-id>/ narrowing when -app-id is in play.
		fmt.Fprintf(stderr, "emrgrep: scanning s3://%s/%s\n", runCfg.Bucket, runCfg.Prefix)
		if opts.MDReport {
			scope := fmt.Sprintf("s3://%s/%s", runCfg.Bucket, runCfg.Prefix)
			mdScopes = append(mdScopes, scope)
			fmt.Fprintf(&mdBuf, "emrgrep: scanning %s\n", scope)
			// Called from the writer goroutine; engine.Run returns only
			// after the writer is closed, so reading mdMatches after
			// each run is safe. Sanitization matches what was printed.
			sanitize, bucket := runCfg.SanitizeOutput, runCfg.Bucket
			runCfg.RecordMatch = func(r scan.Result) {
				// Every recorded match keeps its whole line until the
				// report is written at the end of the run, so the
				// slice is capped: a wide pattern over a busy
				// application would otherwise grow without bound.
				// Matches past the cap are counted and reported in
				// the report and on stderr, never silently dropped.
				if len(mdMatches) >= mdMaxMatches {
					mdDropped++
					return
				}
				key, entry, text := r.Key, r.ZipEntry, string(r.Line)
				if sanitize {
					key, entry, text = scan.SanitizeString(key), scan.SanitizeString(entry), scan.SanitizeString(text)
				}
				mdMatches = append(mdMatches, mdMatch{Key: "s3://" + bucket + "/" + key, Entry: entry, LineNo: r.LineNo, Text: text})
			}
		}

		engine := scan.NewEngine(&runCfg, newS3Client(scopeAWS), stderr)
		result := engine.Run(ctx, stdout)

		engine.Warner().Flush()
		if result.ListingErr != nil {
			fmt.Fprintf(stderr, "emrgrep: %v\n", result.ListingErr)
		}
		scan.PrintSummary(stderr, result, runCfg.ListOnly, resolveColor(opts.Color, stderr))
		if opts.MDReport {
			mdMatched = append(mdMatched, result.MatchedKeys...)
			scan.PrintSummary(&mdBuf, result, runCfg.ListOnly, false)
		}

		code := scan.ExitCode(result)
		if code == 130 {
			writeReport()
			return 130
		}
		worst = combineExit(worst, code)
		if cfg.MaxTotalMatches > 0 {
			remaining -= result.Counters.MatchedLines.Load()
		}
	}
	// The report was asked for; failing to write it is a run failure
	// even when the scan itself succeeded.
	if writeReport() {
		worst = combineExit(worst, 2)
	}
	return worst
}

// combineExit merges per-scope exit codes into the run's overall code
// by severity: fatal (2) > partial/object errors (3) > matched (0) >
// no matches (1). -1 means "no code yet".
func combineExit(a, b int) int {
	severity := map[int]int{2: 3, 3: 2, 0: 1, 1: 0}
	if a < 0 {
		return b
	}
	if severity[b] > severity[a] {
		return b
	}
	return a
}

// resolveGroup decides the effective -group value. An explicit
// command-line flag always wins, including when it asks for a
// combination Build will reject — that is a usage error worth
// reporting. Otherwise grouping happens only where it applies (a
// content scan, not names-only), and there the config file's choice
// wins over terminal detection; pipes keep the stable single-line
// format.
func resolveGroup(cliSet, fileSet, flagValue, contentRun, namesOnly, tty bool) bool {
	if cliSet {
		return flagValue
	}
	if !contentRun || namesOnly {
		return false // grouping does not apply; never fail over a default
	}
	if fileSet {
		return flagValue
	}
	return tty
}

// resolveColor decides whether a stream gets ANSI colors. "always"
// and "never" are absolute; "auto" colors only when the stream is a
// terminal, honoring the NO_COLOR convention and TERM=dumb.
func resolveColor(mode string, w io.Writer) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(w)
}

// isTerminal reports whether w is an interactive terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// loadAWSConfig and s3ClientOptions are the two seams run() is wired
// through, so the whole orchestration — scope loop, cross-scope match
// budget, report writing, exit-code combination — can be exercised
// against a local stub instead of AWS. Production never replaces them.
var (
	loadAWSConfig   = awsconfig.LoadDefaultConfig
	s3ClientOptions = func(*s3.Options) {}
)

// newS3Client builds the S3 client with response-checksum validation
// set to WhenRequired. The SDK's default (WhenSupported) logs a
// "Response has no supported checksum" WARN line for every GetObject
// of an object uploaded without a modern checksum — one useless stderr
// line per object on older buckets. Integrity is still covered by TLS,
// the gzip/ZIP CRCs of compressed content, and the If-Match ETag
// condition on every GET.
func newS3Client(cfg aws.Config) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		s3ClientOptions(o)
	})
}

// awsLoadOptions turns the credential/region flags into SDK load
// options.
//
// -profile is required (config.Options.Build enforces it) and names a
// section of ~/.aws/credentials and ~/.aws/config. Resolution is the
// SDK's job, which is what makes SSO, credential_process, and role_arn
// profiles work without any code here — and what makes the profile's
// region arrive with it: LoadSharedConfigProfile merges both files, so
// a "region =" line in ~/.aws/credentials is picked up exactly like
// one in ~/.aws/config (and wins when both define it).
//
// -region is an explicit override that outranks the profile's own
// region, so the two flags compose.
func awsLoadOptions(opts *config.Options) []func(*awsconfig.LoadOptions) error {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	if opts.Profile != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(opts.Profile))
	}
	return loadOpts
}

// resolveBucketRegion discovers which region a bucket lives in.
// HeadBucket reports it in the response — via the BucketRegion field
// on success, and via the x-amz-bucket-region header even on the
// 301/403 responses a wrong-region or access-restricted probe gets.
// The probe uses us-east-1 when no region is configured at all.
//
// expectedOwner carries -expected-bucket-owner, so the probe is held
// to the same cross-account guard as every LIST and GET: without it
// the one call that runs before the guard takes effect would be the
// one that could reach a squatted bucket name.
func resolveBucketRegion(ctx context.Context, cfg aws.Config, bucket, expectedOwner string) (string, bool) {
	probe := cfg.Copy()
	if probe.Region == "" {
		probe.Region = "us-east-1"
	}
	client := newS3Client(probe)
	input := &s3.HeadBucketInput{Bucket: aws.String(bucket)}
	if expectedOwner != "" {
		input.ExpectedBucketOwner = aws.String(expectedOwner)
	}
	out, err := client.HeadBucket(ctx, input)
	if err == nil {
		if out.BucketRegion != nil && *out.BucketRegion != "" {
			return *out.BucketRegion, true
		}
		return probe.Region, true
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		if region := respErr.Response.Header.Get("x-amz-bucket-region"); region != "" {
			return region, true
		}
	}
	return "", false
}
