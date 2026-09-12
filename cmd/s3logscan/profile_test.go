package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/ryandam9/s3-log-scan/internal/config"
)

// sharedConfig writes an isolated ~/.aws/{config,credentials} pair with
// two profiles in different regions, so profile resolution can be
// observed without touching the developer's real AWS configuration.
func sharedConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	creds := filepath.Join(dir, "credentials")
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg, "[default]\nregion = us-east-1\n\n[profile prod-emr]\nregion = ap-southeast-2\n")
	write(creds, ""+
		"[default]\naws_access_key_id = AKIADEFAULTDEFAULTAA\naws_secret_access_key = defaultsecretdefaultsecretdefaultsecret0\n\n"+
		"[prod-emr]\naws_access_key_id = AKIAPRODEMRPRODEMRAA\naws_secret_access_key = prodemrsecretprodemrsecretprodemrsecret0\n")
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	// Clear the ambient credential/region environment so only the
	// shared config under test is in play.
	for _, k := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func load(t *testing.T, args ...string) (string, string) {
	t.Helper()
	fs, opts := config.NewFlagSet("test", os.Stderr)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsLoadOptions(opts)...)
	if err != nil {
		t.Fatalf("load %v: %v", args, err)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieve credentials for %v: %v", args, err)
	}
	return cfg.Region, creds.AccessKeyID
}

// -profile selects that profile's credentials and region; with no
// -profile the default profile is used, i.e. the standard chain is
// untouched.
func TestProfileSelectsCredentials(t *testing.T) {
	sharedConfig(t)

	region, key := load(t, "-profile", "prod-emr")
	if key != "AKIAPRODEMRPRODEMRAA" {
		t.Errorf("-profile prod-emr used key %q, want the prod-emr key", key)
	}
	if region != "ap-southeast-2" {
		t.Errorf("-profile prod-emr resolved region %q, want ap-southeast-2", region)
	}

	region, key = load(t)
	if key != "AKIADEFAULTDEFAULTAA" {
		t.Errorf("no -profile used key %q, want the default key", key)
	}
	if region != "us-east-1" {
		t.Errorf("no -profile resolved region %q, want us-east-1", region)
	}
}

// -profile overrides $AWS_PROFILE; without -profile, $AWS_PROFILE still
// decides, so existing invocations keep working.
func TestProfileBeatsEnvironment(t *testing.T) {
	sharedConfig(t)
	t.Setenv("AWS_PROFILE", "default")

	if _, key := load(t, "-profile", "prod-emr"); key != "AKIAPRODEMRPRODEMRAA" {
		t.Errorf("-profile must beat $AWS_PROFILE, got key %q", key)
	}

	t.Setenv("AWS_PROFILE", "prod-emr")
	if _, key := load(t); key != "AKIAPRODEMRPRODEMRAA" {
		t.Errorf("$AWS_PROFILE must still apply without -profile, got key %q", key)
	}
}

// -region is an explicit override: it outranks the region the selected
// profile declares, while the profile still supplies the credentials.
func TestRegionOverridesProfileRegion(t *testing.T) {
	sharedConfig(t)

	region, key := load(t, "-profile", "prod-emr", "-region", "eu-central-1")
	if region != "eu-central-1" {
		t.Errorf("-region must win over the profile's region, got %q", region)
	}
	if key != "AKIAPRODEMRPRODEMRAA" {
		t.Errorf("-region must not disturb profile credentials, got key %q", key)
	}
}

// A profile that is not in the shared config fails at load time, which
// run() reports as a fatal configuration error (exit 2).
func TestUnknownProfileFails(t *testing.T) {
	sharedConfig(t)
	fs, opts := config.NewFlagSet("test", os.Stderr)
	if err := fs.Parse([]string{"-profile", "typo-emr"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	if _, err := awsconfig.LoadDefaultConfig(context.Background(), awsLoadOptions(opts)...); err == nil {
		t.Fatal("an unknown profile must fail to load")
	}
}
