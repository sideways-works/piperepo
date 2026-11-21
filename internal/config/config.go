// Package config reads piperepo's configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is one instance's configuration.
type Config struct {
	Listen string `yaml:"listen"`
	S3     S3     `yaml:"s3"`
	// TokenFile holds the credential every request but /health needs.
	TokenFile string `yaml:"token_file"`
	// KeyFile is an armored OpenPGP secret key, or for apk a PEM RSA key;
	// SeedFile, in its place, makes the key from a seed every instance of
	// the store shares.
	KeyFile   string `yaml:"key_file"`
	SeedFile  string `yaml:"seed_file"`
	Overwrite bool   `yaml:"overwrite"`
	// MaxSize is the largest package taken, in bytes; 0 takes any.
	MaxSize int64 `yaml:"max_size"`
	// Reconcile is how often the instance checks the index names what the
	// bucket holds.
	Reconcile time.Duration `yaml:"reconcile"`
	// Grace is how long a replaced index generation stays readable.
	Grace   time.Duration `yaml:"grace"`
	TempDir string        `yaml:"temp_dir"`
	Apt     Apt           `yaml:"apt"`
	Arch    Arch          `yaml:"arch"`
	APK     APK           `yaml:"apk"`
	// Origin names the repository in its indexes.
	Origin string `yaml:"origin"`
}

// S3 is the bucket the packages and indexes live in.
type S3 struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	PathStyle bool   `yaml:"path_style"`
}

// Apt is an apt repository's suite, component and architectures.
type Apt struct {
	Suite         string   `yaml:"suite"`
	Component     string   `yaml:"component"`
	Architectures []string `yaml:"architectures"`
}

// Arch is a pacman repository's database name and architectures.
type Arch struct {
	Repo          string   `yaml:"repo"`
	Architectures []string `yaml:"architectures"`
}

// APK is an Alpine repository's key name and architectures.
type APK struct {
	KeyName       string   `yaml:"key_name"`
	Architectures []string `yaml:"architectures"`
}

// ErrConfig is a configuration refused.
var ErrConfig = errors.New("config")

// Load reads a configuration file; an unknown key is refused.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the file the command line names
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}

	return Parse(b)
}

// Parse reads a configuration and fills its defaults.
func Parse(b []byte) (*Config, error) {
	c := &Config{Listen: "127.0.0.1:8080", Reconcile: 10 * time.Second, Grace: 10 * time.Minute, MaxSize: 2 << 30}

	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)

	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}

	var bad []string

	if c.S3.Bucket == "" {
		bad = append(bad, "s3.bucket: the bucket the packages go to")
	}

	if c.TokenFile == "" {
		bad = append(bad, "token_file: the file with the credential uploads and reads need")
	}

	switch {
	case c.KeyFile == "" && c.SeedFile == "":
		bad = append(bad, "key_file or seed_file: the signing key, or the seed every instance makes it from; without one nothing could be signed")
	case c.KeyFile != "" && c.SeedFile != "":
		bad = append(bad, "key_file and seed_file: one of them")
	}

	if c.Reconcile <= 0 || c.Grace <= 0 {
		bad = append(bad, "reconcile and grace: durations above zero")
	}

	if len(bad) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrConfig, strings.Join(bad, "; "))
	}

	return c, nil
}

// Secret reads a file holding one secret, without its trailing newline.
func Secret(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // a file the configuration names
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}

	b = bytes.TrimRight(b, "\r\n")
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrConfig, path)
	}

	return b, nil
}
