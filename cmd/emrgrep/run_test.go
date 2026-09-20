package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// stubS3 is a minimal path-style S3: ListObjectsV2 over an in-memory
// key set and GetObject for the bodies. It is enough to drive run()
// end to end, which is where the scope loop, the cross-scope match
// budget, the report wiring, and exit-code combination actually live.
type stubS3 struct {
	objects map[string][]byte // key -> body
	// phantoms are keys the listing reports but GetObject refuses,
	// the real-world case of an object deleted between LIST and GET.
	phantoms map[string]bool
	gets     atomic.Int64
	lists    atomic.Int64
}

func (s *stubS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("x-amz-bucket-region", "us-east-1")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Path style: /<bucket>[/<key>]
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	if bucket == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("list-type") == "2" {
		s.lists.Add(1)
		s.list(w, r.URL.Query().Get("prefix"))
		return
	}
	body, ok := s.objects[key]
	if ok && s.phantoms[key] {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
		return
	}
	s.gets.Add(1)
	w.Header().Set("ETag", `"etag-`+key+`"`)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (s *stubS3) list(w http.ResponseWriter, prefix string) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated>`)
	for key, body := range s.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size>`+
			`<LastModified>2026-01-01T00:00:00.000Z</LastModified>`+
			`<ETag>&quot;etag-%s&quot;</ETag><StorageClass>STANDARD</StorageClass></Contents>`,
			key, len(body), key)
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(b.String()))
}

// withStubS3 points run() at a local stub for the duration of a test.
func withStubS3(t *testing.T, objects map[string][]byte) *stubS3 {
	t.Helper()
	stub := &stubS3{objects: objects, phantoms: map[string]bool{}}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	prevLoad, prevOpts := loadAWSConfig, s3ClientOptions
	loadAWSConfig = func(ctx context.Context, _ ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		return aws.Config{
			Region:           "us-east-1",
			Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
			RetryMaxAttempts: 1,
		}, nil
	}
	s3ClientOptions = func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(srv.URL)
	}
	t.Cleanup(func() { loadAWSConfig, s3ClientOptions = prevLoad, prevOpts })
	return stub
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const appPrefix = "logs/j-1/containers/application_1700000000000_0042/"

// The whole pipeline through run(): -app-id narrows the prefix,
// matches reach stdout, the summary reaches stderr, and a run with
// matches exits 0.
func TestRunEndToEndMatches(t *testing.T) {
	isolateHome(t)
	withStubS3(t, map[string][]byte{
		appPrefix + "c_01/stderr.gz": gz(t, "fine\nERROR boom\nfine\n"),
		appPrefix + "c_02/stderr.gz": gz(t, "nothing here\n"),
		"logs/j-1/other/ignored.gz":  gz(t, "ERROR elsewhere\n"),
	})
	var stdout, stderr bytes.Buffer
	code := run(runArgs("-bucket", "b", "-prefix", "logs/j-1/",
		"-app-id", "application_1700000000000_0042", "-grep", "ERROR", "-group=false"), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit %d want 0\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ERROR boom") {
		t.Errorf("match missing from stdout: %q", stdout.String())
	}
	// -app-id must scope the listing: the key outside containers/
	// matches the pattern but must never be read.
	if strings.Contains(stdout.String(), "ERROR elsewhere") {
		t.Error("-app-id did not scope the scan")
	}
	if !strings.Contains(stderr.String(), "scanning s3://b/"+appPrefix) {
		t.Errorf("scope echo missing: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "matched objects") {
		t.Errorf("summary missing: %q", stderr.String())
	}
}

// A completed run with no matches exits 1, and says so rather than
// looking like a failure.
func TestRunNoMatchesExitsOne(t *testing.T) {
	isolateHome(t)
	withStubS3(t, map[string][]byte{appPrefix + "c_01/stderr.gz": gz(t, "all quiet\n")})
	var stdout, stderr bytes.Buffer
	code := run(runArgs("-bucket", "b", "-prefix", "logs/j-1/",
		"-app-id", "application_1700000000000_0042", "-grep", "ERROR"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d want 1\nstderr: %s", code, stderr.String())
	}
}

// -max-total-matches stops the run, and the summary names the reason
// so an operator never reads a capped run as an exhaustive one.
func TestRunMaxTotalMatchesStopsTheRun(t *testing.T) {
	isolateHome(t)
	withStubS3(t, map[string][]byte{
		appPrefix + "c_01/stderr.gz": gz(t, strings.Repeat("ERROR line\n", 50)),
		appPrefix + "c_02/stderr.gz": gz(t, strings.Repeat("ERROR line\n", 50)),
	})
	var stdout, stderr bytes.Buffer
	code := run(runArgs("-bucket", "b", "-prefix", "logs/j-1/",
		"-app-id", "application_1700000000000_0042", "-grep", "ERROR",
		"-max-total-matches", "3", "-workers", "1", "-zip-workers", "1", "-group=false"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d want 0\nstderr: %s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "ERROR line"); got != 3 {
		t.Errorf("printed %d matches, want exactly 3:\n%s", got, stdout.String())
	}
	if !strings.Contains(stderr.String(), "-max-total-matches reached") {
		t.Errorf("summary must name the cap: %q", stderr.String())
	}
}

// -md writes the report where the flag documents, with the matches
// grouped per file, and reports the path on stderr.
func TestRunWritesMDReport(t *testing.T) {
	isolateHome(t)
	withStubS3(t, map[string][]byte{
		appPrefix + "c_01/stderr.gz": gz(t, "ERROR first\nquiet\nERROR second\n"),
	})
	var stdout, stderr bytes.Buffer
	code := run(runArgs("-bucket", "b", "-prefix", "logs/j-1/",
		"-app-id", "application_1700000000000_0042", "-grep", "ERROR", "-md"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d want 0\nstderr: %s", code, stderr.String())
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "logscan", time.Now().Format("2006-01-02"), "application_1700000000000_0042.md")
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("report not written to %s: %v\nstderr: %s", want, err, stderr.String())
	}
	report := string(data)
	for _, w := range []string{
		"# emrgrep — application_1700000000000_0042",
		"- **Pattern**: `ERROR`",
		"- **Files with matches**: 1",
		"ERROR first",
		"ERROR second",
		"## Run summary",
	} {
		if !strings.Contains(report, w) {
			t.Errorf("report missing %q\n---\n%s", w, report)
		}
	}
	if !strings.Contains(stderr.String(), "report written to "+want) {
		t.Errorf("stderr must name the report path: %q", stderr.String())
	}
}

// List-only mode reports keys and downloads nothing at all — the
// promise that makes a patternless run cheap.
func TestRunListOnlyDownloadsNothing(t *testing.T) {
	isolateHome(t)
	stub := withStubS3(t, map[string][]byte{
		appPrefix + "c_01/stderr.gz": gz(t, "ERROR one\n"),
		appPrefix + "c_02/stderr.gz": gz(t, "ERROR two\n"),
	})
	var stdout, stderr bytes.Buffer
	code := run(runArgs("-bucket", "b", "-prefix", "logs/j-1/",
		"-app-id", "application_1700000000000_0042"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d want 0\nstderr: %s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "s3://b/"+appPrefix); got != 2 {
		t.Errorf("listed %d keys, want 2:\n%s", got, stdout.String())
	}
	if n := stub.gets.Load(); n != 0 {
		t.Errorf("list-only mode issued %d GetObject calls, want 0", n)
	}
}

// -download stores each object as-is under the dated directory and
// leaves the bytes untouched (a .gz stays compressed).
func TestRunDownloadStoresFiles(t *testing.T) {
	isolateHome(t)
	body := gz(t, "ERROR kept verbatim\n")
	withStubS3(t, map[string][]byte{appPrefix + "c_01/stderr.gz": body})
	var stdout, stderr bytes.Buffer
	code := run(runArgs("-bucket", "b", "-prefix", "logs/j-1/",
		"-app-id", "application_1700000000000_0042", "-download"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d want 0\nstderr: %s", code, stderr.String())
	}
	home, _ := os.UserHomeDir()
	got, err := os.ReadFile(filepath.Join(home, "logscan", time.Now().Format("2006-01-02"),
		"application_1700000000000_0042", "c_01", "stderr.gz"))
	if err != nil {
		t.Fatalf("downloaded file missing: %v\nstderr: %s", err, stderr.String())
	}
	if !bytes.Equal(got, body) {
		t.Error("stored bytes differ from the object; -download must store as-is")
	}
}

// A GetObject failure is an object error, not a run failure: the scan
// completes, the match from the readable object still prints, and the
// exit code is 3 so automation can tell "no matches" from "the answer
// may be incomplete".
func TestRunObjectErrorExitsThree(t *testing.T) {
	isolateHome(t)
	stub := withStubS3(t, map[string][]byte{
		appPrefix + "c_01/stderr.gz": gz(t, "ERROR readable\n"),
		appPrefix + "c_02/stderr.gz": gz(t, "ERROR unreachable\n"),
	})
	// Listed, but deleted before the GET lands.
	stub.phantoms[appPrefix+"c_02/stderr.gz"] = true

	var stdout, stderr bytes.Buffer
	code := run(runArgs("-bucket", "b", "-prefix", "logs/j-1/",
		"-app-id", "application_1700000000000_0042", "-grep", "ERROR", "-group=false"), &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit %d want 3\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ERROR readable") {
		t.Errorf("the readable object's match must still print: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "object errors") {
		t.Errorf("summary must classify the failure: %q", stderr.String())
	}
}
