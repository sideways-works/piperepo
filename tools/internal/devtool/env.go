package devtool

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// GoEnv is what the shared machine needs for each Go command:
//   - -trimpath, so a second worktree reuses the build cache;
//   - a GOTMPDIR on disk, since /tmp is RAM.
//
// A GOFLAGS or GOTMPDIR already set is kept; -trimpath is added to it.
func GoEnv(goflags, gotmpdir, home string) (flags, tmp string) {
	flags = goflags
	if !slices.Contains(strings.Fields(goflags), "-trimpath") {
		flags = strings.TrimSpace("-trimpath " + goflags)
	}

	tmp = gotmpdir
	if tmp == "" {
		tmp = filepath.Join(home, ".cache", "piperun-gotmp")
	}

	return flags, tmp
}

// SetGoEnv puts GoEnv into this process's environment, for every command it starts.
func SetGoEnv() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("go env: %w", err)
	}

	flags, tmp := GoEnv(os.Getenv("GOFLAGS"), os.Getenv("GOTMPDIR"), home)

	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return fmt.Errorf("go env: %w", err)
	}

	if err := os.Setenv("GOFLAGS", flags); err != nil {
		return fmt.Errorf("go env: %w", err)
	}

	if err := os.Setenv("GOTMPDIR", tmp); err != nil {
		return fmt.Errorf("go env: %w", err)
	}

	return nil
}
