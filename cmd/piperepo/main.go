// Command piperepo serves one package repository from S3: apt, pacman, apk
// or rpm, signed, with an upload API.
//
//	piperepo serve --format apt --config piperepo.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sideways-works/piperepo/internal/config"
	"github.com/sideways-works/piperepo/internal/format"
	"github.com/sideways-works/piperepo/internal/repo"
	"github.com/sideways-works/piperepo/internal/server"
	"github.com/sideways-works/piperepo/internal/sign"
	"github.com/sideways-works/piperepo/internal/store"
)

// exitAfterPackage, set to 1, ends the process once an upload's package
// is written and before its index: the reconcile test's crash.
const exitAfterPackage = "PIPEREPO_TEST_EXIT_AFTER_PACKAGE"

var errUsage = errors.New("usage: piperepo serve --format apt|arch|apk|rpm --config <file>")

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "piperepo:", err)
		os.Exit(1)
	}
}

func run(args []string, logTo io.Writer) error {
	if len(args) == 0 || args[0] != "serve" {
		return errUsage
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	name := fs.String("format", "", "apt, arch, apk or rpm")
	path := fs.String("config", "", "the configuration file")

	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}

	if *name == "" || *path == "" {
		return errUsage
	}

	log := slog.New(slog.NewJSONHandler(logTo, nil)).With("format", *name)

	c, err := config.Load(*path)
	if err != nil {
		return err
	}

	r, token, err := build(*name, c, log)
	if err != nil {
		return err
	}

	srv, err := server.New(r, token, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go reconcile(ctx, r, c.Reconcile, log)

	hs := &http.Server{Addr: c.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}

	go func() {
		<-ctx.Done()

		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		_ = hs.Shutdown(sctx) //nolint:errcheck // stopping either way
	}()

	log.Info("serving", "listen", c.Listen, "bucket", c.S3.Bucket)

	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// build is the repository a configuration names, and its credential.
func build(name string, c *config.Config, log *slog.Logger) (*repo.Repo, []byte, error) {
	token, err := config.Secret(c.TokenFile)
	if err != nil {
		return nil, nil, err
	}

	signer, err := signerOf(name, c)
	if err != nil {
		return nil, nil, err
	}

	o := format.Options{Origin: c.Origin, Suite: c.Apt.Suite, Component: c.Apt.Component, Archs: c.Apt.Architectures}

	switch name {
	case "arch":
		o.Repo, o.Archs = c.Arch.Repo, c.Arch.Architectures
	case "apk":
		o.KeyName, o.Archs = c.APK.KeyName, c.APK.Architectures
	}

	f, err := format.New(name, o, signer)
	if err != nil {
		return nil, nil, err
	}

	r := &repo.Repo{
		Store: store.NewS3(store.S3Config{
			Endpoint: c.S3.Endpoint, Region: c.S3.Region, Bucket: c.S3.Bucket,
			AccessKey: c.S3.AccessKey, SecretKey: c.S3.SecretKey, PathStyle: c.S3.PathStyle,
		}),
		Format: f, Overwrite: c.Overwrite, MaxSize: c.MaxSize, Grace: c.Grace, PointerTTL: time.Second, TempDir: c.TempDir, Log: log,
	}

	if os.Getenv(exitAfterPackage) == "1" {
		r.AfterPackage = func() { os.Exit(3) }
	}

	return r, token, nil
}

// signerOf reads the key, or makes it from the seed.
func signerOf(name string, c *config.Config) (format.Signer, error) {
	if c.SeedFile != "" {
		seed, err := config.Secret(c.SeedFile)
		if err != nil {
			return format.Signer{}, err
		}

		if !format.OpenPGP(name) {
			k, err := sign.RSAFromSeed(seed)

			return format.Signer{RSA: k}, err
		}

		k, err := sign.OpenPGPFromSeed(seed, originOr(c.Origin)+" signing key")

		return format.Signer{PGP: k}, err
	}

	data, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return format.Signer{}, fmt.Errorf("key_file: %w", err)
	}

	if !format.OpenPGP(name) {
		k, err := sign.RSAFromPEM(data)

		return format.Signer{RSA: k}, err
	}

	f, err := os.Open(c.KeyFile)
	if err != nil {
		return format.Signer{}, fmt.Errorf("key_file: %w", err)
	}
	defer f.Close()

	k, err := sign.OpenPGPFromArmored(f)

	return format.Signer{PGP: k}, err
}

func originOr(o string) string {
	if o == "" {
		return "piperepo"
	}

	return o
}

// reconcile publishes, now and on every tick, whatever the bucket holds
// that the index doesn't name.
func reconcile(ctx context.Context, r *repo.Repo, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			log.Warn("reconcile failed", "err", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
