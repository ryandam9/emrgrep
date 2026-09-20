package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/emr"
	emrtypes "github.com/aws/aws-sdk-go-v2/service/emr/types"
)

// emrAPI is the slice of the EMR client used for cluster resolution;
// the fake in tests implements the same interface.
type emrAPI interface {
	ListClusters(ctx context.Context, in *emr.ListClustersInput, optFns ...func(*emr.Options)) (*emr.ListClustersOutput, error)
	DescribeCluster(ctx context.Context, in *emr.DescribeClusterInput, optFns ...func(*emr.Options)) (*emr.DescribeClusterOutput, error)
}

// clusterMatch is one EMR cluster whose name matched -cluster-name.
type clusterMatch struct {
	ID      string
	State   string
	Created time.Time
}

func (m clusterMatch) String() string {
	created := "unknown creation time"
	if !m.Created.IsZero() {
		created = "created " + m.Created.Format(time.RFC3339)
	}
	return fmt.Sprintf("%s (%s, %s)", m.ID, m.State, created)
}

// resolveClusters finds every EMR cluster with the given name that is
// currently RUNNING or WAITING (filtered server-side), newest first.
// All of them are scanned — same-name clusters are siblings, and the
// application being investigated may live on any of them. Logs of
// terminated clusters can still be scanned via -cluster-id.
func resolveClusters(ctx context.Context, client emrAPI, name string) ([]clusterMatch, error) {
	var matches []clusterMatch
	input := &emr.ListClustersInput{
		ClusterStates: []emrtypes.ClusterState{
			emrtypes.ClusterStateRunning,
			emrtypes.ClusterStateWaiting,
		},
	}
	for {
		out, err := client.ListClusters(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("listing EMR clusters: %w", err)
		}
		for _, c := range out.Clusters {
			if aws.ToString(c.Name) != name {
				continue
			}
			m := clusterMatch{ID: aws.ToString(c.Id)}
			if c.Status != nil {
				m.State = string(c.Status.State)
				if c.Status.Timeline != nil {
					m.Created = aws.ToTime(c.Status.Timeline.CreationDateTime)
				}
			}
			matches = append(matches, m)
		}
		if out.Marker == nil || aws.ToString(out.Marker) == "" {
			break
		}
		input.Marker = out.Marker
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("no running or waiting EMR cluster named %q found (terminated clusters can be targeted with -cluster-id)", name)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Created.After(matches[j].Created) })
	return matches, nil
}

// scanScope is one bucket/prefix a run will cover; multi-cluster
// resolution produces one per cluster.
type scanScope struct {
	Bucket  string
	Prefix  string
	Cluster string // cluster ID, "" for plain bucket/prefix scans
}

// clusterScopes turns the cluster flags into concrete scan scopes.
// Each cluster resolves its own log destination unless an explicit
// bucket overrides it. A cluster whose destination cannot be resolved
// is reported and skipped (failed=true) rather than blocking the
// remaining clusters.
func clusterScopes(ctx context.Context, client emrAPI, clusters []clusterMatch, bucket, prefix string, warn io.Writer) (scopes []scanScope, failed bool) {
	for _, c := range clusters {
		b, p := bucket, prefix
		if b == "" {
			var err error
			b, p, err = clusterLogDestination(ctx, client, c.ID)
			if err != nil {
				fmt.Fprintf(warn, "emrgrep: %v; skipping cluster %s\n", err, c.ID)
				failed = true
				continue
			}
		}
		scopes = append(scopes, scanScope{Bucket: b, Prefix: joinClusterPrefix(p, c.ID), Cluster: c.ID})
	}
	return scopes, failed
}

// joinClusterPrefix appends an EMR cluster ID to the key prefix so
// every LIST and GET is scoped to bucket/prefix/<cluster-id>/.
func joinClusterPrefix(prefix, clusterID string) string {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + clusterID + "/"
}

// joinAppPrefix appends a YARN application's container-log directory
// ("containers/<app-id>/") to a scope prefix. EMR's log layout is
// deterministic — bucket/prefix/<cluster-id>/containers/<app-id>/… —
// so the application's objects are reached by prefix alone, without
// enumerating the rest of the cluster's keys.
func joinAppPrefix(prefix, appID string) string {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + "containers/" + appID + "/"
}

// clusterLogDestination reads the cluster's "Log destination in
// Amazon S3" (LogUri) via DescribeCluster, so -bucket/-prefix need not
// be passed at all. Works for terminated clusters too.
func clusterLogDestination(ctx context.Context, client emrAPI, clusterID string) (bucket, prefix string, err error) {
	out, err := client.DescribeCluster(ctx, &emr.DescribeClusterInput{ClusterId: aws.String(clusterID)})
	if err != nil {
		return "", "", fmt.Errorf("describing EMR cluster %s: %w", clusterID, err)
	}
	logURI := ""
	if out.Cluster != nil {
		logURI = aws.ToString(out.Cluster.LogUri)
	}
	if logURI == "" {
		return "", "", fmt.Errorf("EMR cluster %s has no S3 log destination configured; pass -bucket and -prefix explicitly", clusterID)
	}
	bucket, prefix, err = parseS3URI(logURI)
	if err != nil {
		return "", "", fmt.Errorf("EMR cluster %s log destination: %w", clusterID, err)
	}
	return bucket, prefix, nil
}

// parseS3URI splits s3://bucket/prefix (also the legacy s3n:// and
// s3a:// schemes EMR log URIs sometimes carry) into bucket and prefix.
func parseS3URI(uri string) (bucket, prefix string, err error) {
	rest := ""
	switch {
	case strings.HasPrefix(uri, "s3://"):
		rest = uri[len("s3://"):]
	case strings.HasPrefix(uri, "s3n://"):
		rest = uri[len("s3n://"):]
	case strings.HasPrefix(uri, "s3a://"):
		rest = uri[len("s3a://"):]
	default:
		return "", "", fmt.Errorf("unsupported S3 URI %q (expected s3://bucket/prefix)", uri)
	}
	bucket, prefix, _ = strings.Cut(rest, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("no bucket in S3 URI %q", uri)
	}
	return bucket, prefix, nil
}
