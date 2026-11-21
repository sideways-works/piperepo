// Package devtool holds what the developer tools share: the checkout they
// work in, the Go settings of the shared machine and running commands.
package devtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotBazel says a tool was started outside bazel run.
var ErrNotBazel = errors.New("not run by Bazel")

// Workspace is the checkout bazel run was called in.
func Workspace(target string) (string, error) {
	dir := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if dir == "" {
		return "", fmt.Errorf("%w: run it with bazel run //:%s", ErrNotBazel, target)
	}

	return dir, nil
}

// Caller resolves a path a person gave against the directory bazel run was called in.
func Caller(path string) string {
	if filepath.IsAbs(path) {
		return path
	}

	if dir := os.Getenv("BUILD_WORKING_DIRECTORY"); dir != "" {
		return filepath.Join(dir, path)
	}

	return path
}

// Run runs a command in dir with the tool's own stdin, stdout and stderr.
func Run(ctx context.Context, dir, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir, c.Stdin, c.Stdout, c.Stderr = dir, os.Stdin, os.Stdout, os.Stderr

	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", filepath.Base(name), strings.Join(args, " "), err)
	}

	return nil
}

// Output runs a command in dir and returns its stdout; stderr is kept for the error.
func Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir

	var stderr strings.Builder

	c.Stderr = &stderr

	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", filepath.Base(name), strings.Join(args, " "), err, stderr.String())
	}

	return string(out), nil
}
