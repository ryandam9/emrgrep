package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// stubHTTP answers every request with a canned response (or error),
// standing in for the S3 endpoint during HeadBucket probes.
type stubHTTP struct {
	status int
	header http.Header
	err    error
}

func (s *stubHTTP) Do(req *http.Request) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     s.header,
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Request:    req,
	}, nil
}

func stubConfig(h *stubHTTP, region string) aws.Config {
	return aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:  h,
		// Never retry in tests: one canned response per call is enough.
		RetryMaxAttempts: 1,
	}
}

// A wrong-region HeadBucket gets a 301 whose x-amz-bucket-region header
// names the real region — the case behind IllegalLocationConstraint
// failures like a us-east-1 client probing an ap-southeast-4 bucket.
func TestResolveBucketRegionFromRedirect(t *testing.T) {
	h := &stubHTTP{status: 301, header: http.Header{"X-Amz-Bucket-Region": []string{"ap-southeast-4"}}}
	region, ok := resolveBucketRegion(context.Background(), stubConfig(h, "us-east-1"), "b")
	if !ok || region != "ap-southeast-4" {
		t.Fatalf("got %q ok=%v, want ap-southeast-4", region, ok)
	}
}

// Same header on a 403: region detection works even without HeadBucket
// permission.
func TestResolveBucketRegionFromForbidden(t *testing.T) {
	h := &stubHTTP{status: 403, header: http.Header{"X-Amz-Bucket-Region": []string{"eu-central-2"}}}
	region, ok := resolveBucketRegion(context.Background(), stubConfig(h, "us-east-1"), "b")
	if !ok || region != "eu-central-2" {
		t.Fatalf("got %q ok=%v, want eu-central-2", region, ok)
	}
}

// A successful HeadBucket in the probe region reports via the same
// header (deserialized into BucketRegion).
func TestResolveBucketRegionFromSuccess(t *testing.T) {
	h := &stubHTTP{status: 200, header: http.Header{"X-Amz-Bucket-Region": []string{"us-west-2"}}}
	region, ok := resolveBucketRegion(context.Background(), stubConfig(h, "us-west-2"), "b")
	if !ok || region != "us-west-2" {
		t.Fatalf("got %q ok=%v, want us-west-2", region, ok)
	}
}

// Transport failures (no network, no endpoint) must report failure so
// the caller falls back to the configured region.
func TestResolveBucketRegionTransportFailure(t *testing.T) {
	h := &stubHTTP{err: errors.New("dial tcp: no route to host")}
	if region, ok := resolveBucketRegion(context.Background(), stubConfig(h, "us-east-1"), "b"); ok {
		t.Fatalf("transport failure must not resolve a region, got %q", region)
	}
}
