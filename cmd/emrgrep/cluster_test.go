package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/emr"
	emrtypes "github.com/aws/aws-sdk-go-v2/service/emr/types"
)

// fakeEMR pages through canned cluster summaries and serves
// DescribeCluster from a map.
type fakeEMR struct {
	pages       [][]emrtypes.ClusterSummary
	described   map[string]*emrtypes.Cluster
	listErr     error
	describeErr error
	gotStates   []emrtypes.ClusterState
}

func (f *fakeEMR) ListClusters(ctx context.Context, in *emr.ListClustersInput, _ ...func(*emr.Options)) (*emr.ListClustersOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.gotStates = in.ClusterStates
	page := 0
	if in.Marker != nil {
		page = int((*in.Marker)[0] - '0')
	}
	out := &emr.ListClustersOutput{Clusters: f.pages[page]}
	if page+1 < len(f.pages) {
		m := string(rune('0' + page + 1))
		out.Marker = &m
	}
	return out, nil
}

func (f *fakeEMR) DescribeCluster(ctx context.Context, in *emr.DescribeClusterInput, _ ...func(*emr.Options)) (*emr.DescribeClusterOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return &emr.DescribeClusterOutput{Cluster: f.described[aws.ToString(in.ClusterId)]}, nil
}

func summary(id, name string, created time.Time) emrtypes.ClusterSummary {
	return emrtypes.ClusterSummary{
		Id:   aws.String(id),
		Name: aws.String(name),
		Status: &emrtypes.ClusterStatus{
			State:    emrtypes.ClusterStateWaiting,
			Timeline: &emrtypes.ClusterTimeline{CreationDateTime: aws.Time(created)},
		},
	}
}

func TestResolveClustersSingleMatchAcrossPages(t *testing.T) {
	f := &fakeEMR{pages: [][]emrtypes.ClusterSummary{
		{summary("j-AAA", "other", time.Now())},
		{summary("j-BBB", "hbase-prod", time.Now())},
	}}
	clusters, err := resolveClusters(context.Background(), f, "hbase-prod")
	if err != nil || len(clusters) != 1 || clusters[0].ID != "j-BBB" {
		t.Fatalf("clusters=%v err=%v", clusters, err)
	}
	// The server-side state filter must request only active clusters.
	want := []emrtypes.ClusterState{emrtypes.ClusterStateRunning, emrtypes.ClusterStateWaiting}
	if len(f.gotStates) != 2 || f.gotStates[0] != want[0] || f.gotStates[1] != want[1] {
		t.Fatalf("ClusterStates filter: %v", f.gotStates)
	}
}

func TestResolveClustersNoMatch(t *testing.T) {
	f := &fakeEMR{pages: [][]emrtypes.ClusterSummary{{summary("j-AAA", "other", time.Now())}}}
	_, err := resolveClusters(context.Background(), f, "hbase-prod")
	if err == nil || !strings.Contains(err.Error(), "no running or waiting EMR cluster") {
		t.Fatalf("err: %v", err)
	}
}

// Duplicate names: ALL matching clusters are returned, newest first —
// the application under investigation may live on any of them.
func TestResolveClustersAllReturnedNewestFirst(t *testing.T) {
	now := time.Now()
	f := &fakeEMR{pages: [][]emrtypes.ClusterSummary{{
		summary("j-OLD", "hbase-prod", now.Add(-48*time.Hour)),
		summary("j-NEW", "hbase-prod", now),
		summary("j-MID", "hbase-prod", now.Add(-24*time.Hour)),
	}}}
	clusters, err := resolveClusters(context.Background(), f, "hbase-prod")
	if err != nil || len(clusters) != 3 {
		t.Fatalf("clusters=%v err=%v", clusters, err)
	}
	for i, want := range []string{"j-NEW", "j-MID", "j-OLD"} {
		if clusters[i].ID != want {
			t.Fatalf("order: %v", clusters)
		}
	}
}

func TestResolveClustersListError(t *testing.T) {
	f := &fakeEMR{listErr: errors.New("throttled")}
	if _, err := resolveClusters(context.Background(), f, "x"); err == nil {
		t.Fatal("list error must surface")
	}
}

// Each same-name cluster becomes a scope with its OWN log destination;
// one unresolvable cluster is skipped without blocking the rest.
func TestClusterScopesPerClusterDestinations(t *testing.T) {
	f := &fakeEMR{described: map[string]*emrtypes.Cluster{
		"j-A": {LogUri: aws.String("s3://bucket-a/logs/")},
		"j-B": {LogUri: aws.String("s3://bucket-b/other/")},
		"j-C": {}, // no log destination
	}}
	var warn strings.Builder
	clusters := []clusterMatch{{ID: "j-A"}, {ID: "j-B"}, {ID: "j-C"}}
	scopes, failed := clusterScopes(context.Background(), f, clusters, "", "", &warn)
	if !failed {
		t.Fatal("unresolvable cluster must set failed")
	}
	if len(scopes) != 2 {
		t.Fatalf("scopes: %v", scopes)
	}
	if scopes[0].Bucket != "bucket-a" || scopes[0].Prefix != "logs/j-A/" {
		t.Fatalf("scope A: %+v", scopes[0])
	}
	if scopes[1].Bucket != "bucket-b" || scopes[1].Prefix != "other/j-B/" {
		t.Fatalf("scope B: %+v", scopes[1])
	}
	if !strings.Contains(warn.String(), "skipping cluster j-C") {
		t.Fatalf("skip warning missing: %q", warn.String())
	}
}

// An explicit bucket/prefix overrides every cluster's destination but
// still scopes per cluster ID.
func TestClusterScopesExplicitBucket(t *testing.T) {
	f := &fakeEMR{} // DescribeCluster must not be needed
	clusters := []clusterMatch{{ID: "j-A"}, {ID: "j-B"}}
	scopes, failed := clusterScopes(context.Background(), f, clusters, "mybucket", "logs", &strings.Builder{})
	if failed || len(scopes) != 2 {
		t.Fatalf("scopes=%v failed=%v", scopes, failed)
	}
	if scopes[0].Prefix != "logs/j-A/" || scopes[1].Prefix != "logs/j-B/" || scopes[0].Bucket != "mybucket" {
		t.Fatalf("scopes: %v", scopes)
	}
}

func TestCombineExit(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{-1, 1, 1},
		{1, 0, 0}, // matches beat no-matches
		{0, 3, 3}, // partial beats matched
		{3, 2, 2}, // fatal beats partial
		{2, 0, 2}, // fatal sticks
		{0, 1, 0}, // no-matches never downgrades a match
		{-1, 2, 2},
	}
	for _, tc := range cases {
		if got := combineExit(tc.a, tc.b); got != tc.want {
			t.Errorf("combineExit(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestClusterLogDestination(t *testing.T) {
	f := &fakeEMR{described: map[string]*emrtypes.Cluster{
		"j-A": {LogUri: aws.String("s3://my-emr-logs/logs/")},
		"j-B": {LogUri: aws.String("s3n://legacy-bucket/emr/logs")},
		"j-C": {}, // no logging configured
	}}
	b, p, err := clusterLogDestination(context.Background(), f, "j-A")
	if err != nil || b != "my-emr-logs" || p != "logs/" {
		t.Fatalf("j-A: %q %q %v", b, p, err)
	}
	b, p, err = clusterLogDestination(context.Background(), f, "j-B")
	if err != nil || b != "legacy-bucket" || p != "emr/logs" {
		t.Fatalf("j-B (s3n): %q %q %v", b, p, err)
	}
	if _, _, err = clusterLogDestination(context.Background(), f, "j-C"); err == nil || !strings.Contains(err.Error(), "no S3 log destination") {
		t.Fatalf("j-C: %v", err)
	}
}

func TestParseS3URI(t *testing.T) {
	cases := []struct {
		uri, bucket, prefix string
		wantErr             bool
	}{
		{"s3://b/logs/", "b", "logs/", false},
		{"s3://b/a/b/c", "b", "a/b/c", false},
		{"s3://b", "b", "", false},
		{"s3a://b/x", "b", "x", false},
		{"https://example.com/x", "", "", true},
		{"s3://", "", "", true},
	}
	for _, tc := range cases {
		b, p, err := parseS3URI(tc.uri)
		if tc.wantErr != (err != nil) || b != tc.bucket || p != tc.prefix {
			t.Errorf("parseS3URI(%q) = %q %q %v", tc.uri, b, p, err)
		}
	}
}

func TestJoinClusterPrefix(t *testing.T) {
	cases := []struct{ prefix, id, want string }{
		{"logs/", "j-1ABC", "logs/j-1ABC/"},
		{"logs", "j-1ABC", "logs/j-1ABC/"},
		{"", "j-1ABC", "j-1ABC/"},
		{"a/b/c/", "j-X", "a/b/c/j-X/"},
	}
	for _, tc := range cases {
		if got := joinClusterPrefix(tc.prefix, tc.id); got != tc.want {
			t.Errorf("joinClusterPrefix(%q, %q) = %q, want %q", tc.prefix, tc.id, got, tc.want)
		}
	}
}

func TestJoinAppPrefix(t *testing.T) {
	cases := []struct{ prefix, appID, want string }{
		{"logs/j-1ABC/", "application_1_2", "logs/j-1ABC/containers/application_1_2/"},
		{"logs/j-1ABC", "application_1_2", "logs/j-1ABC/containers/application_1_2/"},
		{"j-1ABC/", "application_1700000000000_0042", "j-1ABC/containers/application_1700000000000_0042/"},
	}
	for _, tc := range cases {
		if got := joinAppPrefix(tc.prefix, tc.appID); got != tc.want {
			t.Errorf("joinAppPrefix(%q, %q) = %q, want %q", tc.prefix, tc.appID, got, tc.want)
		}
	}
}
