package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// The SDK default (WhenSupported) logs a WARN per GetObject for
// objects without modern checksums; the client must be pinned to
// WhenRequired so stderr stays clean on older buckets.
func TestClientChecksumValidationWhenRequired(t *testing.T) {
	client := newS3Client(aws.Config{Region: "us-east-1"})
	if got := client.Options().ResponseChecksumValidation; got != aws.ResponseChecksumValidationWhenRequired {
		t.Fatalf("ResponseChecksumValidation = %v, want WhenRequired", got)
	}
}
