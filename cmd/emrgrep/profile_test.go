package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/ryandam9/emrgrep/internal/config"
)

// sharedConfigFiles writes an isolated ~/.aws/{config,credentials} pair
// with the given contents and points the SDK at it, so profile
// resolution can be observed without touching the developer's real AWS
// configuration.
func sharedConfigFiles(t *testing.T, configBody, credentialsBody string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	creds := filepath.Join(dir, "credentials")
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg, configBody)
	write(creds, credentialsBody)
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

// sharedConfig is the common fixture: two profiles in different
// regions, each declared in the config file and credentialed in the
// credentials file.
func sharedConfig(t *testing.T) {
	t.Helper()
	sharedConfigFiles(t,
		"[default]\nregion = us-east-1\n\n[profile prod-emr]\nregion = ap-southeast-2\n",
		"[default]\naws_access_key_id = AKIADEFAULTDEFAULTAA\naws_secret_access_key = defaultsecretdefaultsecretdefaultsecret0\n\n"+
			"[prod-emr]\naws_access_key_id = AKIAPRODEMRPRODEMRAA\naws_secret_access_key = prodemrsecretprodemrsecretprodemrsecret0\n")
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

// The region may live in ~/.aws/credentials rather than ~/.aws/config:
// a profile that exists only there, with its own "region =" line, is
// resolved whole — credentials and region together. This is the SDK's
// doing (LoadSharedConfigProfile merges both files), and it is the
// arrangement most people actually have, so it is pinned here.
func TestRegionFromCredentialsFile(t *testing.T) {
	sharedConfigFiles(t,
		"[default]\nregion = us-east-1\n",
		"[default]\naws_access_key_id = AKIADEFAULTDEFAULTAA\naws_secret_access_key = defaultsecretdefaultsecretdefaultsecret0\n\n"+
			"[creds-only]\naws_access_key_id = AKIACREDSONLYCREDSAA\naws_secret_access_key = credsonlysecretcredsonlysecretcredson0\nregion = eu-west-1\n")

	region, key := load(t, "-profile", "creds-only")
	if region != "eu-west-1" {
		t.Errorf("region from ~/.aws/credentials = %q, want eu-west-1", region)
	}
	if key != "AKIACREDSONLYCREDSAA" {
		t.Errorf("credentials-only profile used key %q, want the creds-only key", key)
	}
}

// When both files declare a region for the same profile, the
// credentials file wins — the SDK merges it over the config file's
// value. Worth pinning: it decides which region the EMR lookup uses.
func TestCredentialsRegionBeatsConfigRegion(t *testing.T) {
	sharedConfigFiles(t,
		"[default]\nregion = us-east-1\n\n[profile both]\nregion = us-west-1\n",
		"[default]\naws_access_key_id = AKIADEFAULTDEFAULTAA\naws_secret_access_key = defaultsecretdefaultsecretdefaultsecret0\n\n"+
			"[both]\naws_access_key_id = AKIABOTHBOTHBOTHBOAA\naws_secret_access_key = bothsecretbothsecretbothsecretboths0\nregion = ap-south-1\n")

	if region, _ := load(t, "-profile", "both"); region != "ap-south-1" {
		t.Errorf("region = %q, want the credentials file's ap-south-1", region)
	}
}

// A profile with credentials but no region anywhere leaves the loaded
// config regionless — the condition run() turns into a usage error.
func TestProfileWithoutRegionResolvesEmpty(t *testing.T) {
	sharedConfigFiles(t,
		"[default]\nregion = us-east-1\n",
		"[default]\naws_access_key_id = AKIADEFAULTDEFAULTAA\naws_secret_access_key = defaultsecretdefaultsecretdefaultsecret0\n\n"+
			"[no-region]\naws_access_key_id = AKIANOREGIONNOREGIAA\naws_secret_access_key = noregionsecretnoregionsecretnoregio0\n")

	if region, _ := load(t, "-profile", "no-region"); region != "" {
		t.Errorf("region = %q, want empty for a profile that declares none", region)
	}
}
