package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFenceFor(t *testing.T) {
	cases := []struct{ content, want string }{
		{"plain text", "```"},
		{"one `tick`", "```"},
		{"has ``` inside", "````"},
		{"has ````` five", "``````"},
	}
	for _, tc := range cases {
		if got := fenceFor(tc.content); got != tc.want {
			t.Errorf("fenceFor(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}

func TestWriteMDReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logscan", "application_1_2.md")
	key1 := "s3://b/logs/j-1/containers/application_1_2/c_01/stderr.gz"
	key2 := "s3://b/logs/j-1/containers/application_1_2/c_02/stderr.gz"
	matches := []mdMatch{
		{Key: key1, LineNo: 44, Text: "line with ERROR text"},
		{Key: key2, LineNo: 7, Text: "another ERROR"},
		{Key: key1, LineNo: 812, Text: "Caused by: something"},
		{Key: key2, Entry: "inner.log", LineNo: 3, Text: "zip ERROR"},
	}
	runLog := "emrgrep: scanning s3://b/logs/j-1/containers/application_1_2/\n---\nemrgrep: completed in 1.0s\n"
	// A non-UTC zone proves the timestamp renders in the local zone it
	// was produced in (main passes time.Now()), not converted to UTC.
	now := time.Date(2026, 8, 4, 20, 30, 0, 0, time.FixedZone("AEST", 10*3600))
	if err := writeMDReport(path, "application_1_2", "ERROR", []string{"s3://b/logs/j-1/containers/application_1_2/"}, []string{key1, key2}, matches, 0, runLog, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"# emrgrep — application_1_2",
		"- **Generated**: 2026-08-04 20:30:00 AEST",
		"- **Pattern**: `ERROR`",
		"- **Scanned**: `s3://b/logs/j-1/containers/application_1_2/`",
		"- **Files with matches**: 2",
		"## Files with matches",
		"## Matches",
		// Each file is its own section: heading, then only its lines.
		"### " + key1 + "\n\n```sh\n    44: line with ERROR text\n   812: Caused by: something\n```",
		"### " + key2 + "\n\n```sh\n     7: another ERROR\ninner.log:3: zip ERROR\n```",
		"## Run summary",
		"emrgrep: completed in 1.0s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q\n---\n%s", want, got)
		}
	}
	// Files appear in first-match order, interleaving undone.
	if strings.Index(got, "### "+key1) > strings.Index(got, "### "+key2) {
		t.Error("file sections out of first-match order")
	}
}

// A run with no matches still writes a well-formed report.
func TestWriteMDReportNoMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application_9_9.md")
	err := writeMDReport(path, "application_9_9", "FATAL", []string{"s3://b/p/"}, nil, nil, 0,
		"emrgrep: scanning s3://b/p/\n", time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.Contains(got, "- **Files with matches**: 0") {
		t.Errorf("missing zero count:\n%s", got)
	}
	if strings.Count(got, "```sh\n(none)\n```") != 2 {
		t.Errorf("files and matches sections must both show (none):\n%s", got)
	}
}

// Log content containing a ``` run must not close the fence early.
func TestWriteMDReportFenceEscaping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application_3_3.md")
	matches := []mdMatch{{Key: "s3://b/k", LineNo: 1, Text: "evil line with ``` fence"}}
	if err := writeMDReport(path, "application_3_3", "fence", []string{"s3://b/p/"}, []string{"s3://b/k"}, matches, 0, "log\n", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "````sh\n     1: evil line with ``` fence\n````") {
		t.Errorf("fence not widened:\n%s", string(data))
	}
}

// Reports group under a local-date directory: ~/logscan/<yyyy-mm-dd>/.
func TestReportPathDateDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	now := time.Date(2026, 8, 5, 23, 30, 0, 0, time.FixedZone("AEST", 10*3600))
	got, err := reportPath("application_1_2", now)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "logscan", "2026-08-05", "application_1_2.md")
	if got != want {
		t.Fatalf("reportPath = %q, want %q", got, want)
	}
}

// When the in-memory cap drops matches, the report says so: a reader
// must never mistake a truncated list for the whole story.
func TestWriteMDReportStatesTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application_4_4.md")
	matches := []mdMatch{{Key: "s3://b/k", LineNo: 1, Text: "ERROR one"}}
	if err := writeMDReport(path, "application_4_4", "ERROR", []string{"s3://b/p/"},
		[]string{"s3://b/k"}, matches, 99, "log\n", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "- **Matches listed**: 1 of 100") {
		t.Errorf("report must state the truncation:\n%s", got)
	}
	if !strings.Contains(string(got), "-max-total-matches") {
		t.Errorf("report must name the way to bound the scan:\n%s", got)
	}
}

// A complete report carries no truncation line at all.
func TestWriteMDReportNoTruncationLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application_5_5.md")
	if err := writeMDReport(path, "application_5_5", "ERROR", []string{"s3://b/p/"}, nil, nil, 0,
		"log\n", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "Matches listed") {
		t.Errorf("no truncation line expected:\n%s", got)
	}
}
