// Command lint is the lint every commit passes: go vet and golangci-lint,
// with the container tests' build tag too.
//
//	bazel run //:lint
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sideways-works/piperepo/tools/internal/devtool"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// every is the pattern of every package in the module.
const every = "./..."

// steps are the checks in order.
var steps = [][]string{
	{"go", "vet", every},
	{"go", "vet", "-tags", "containers", every},
	{"golangci-lint", "run", "--allow-parallel-runners", "--build-tags", "containers", every},
}

func run(ctx context.Context) error {
	root, err := devtool.Workspace("lint")
	if err != nil {
		return err
	}

	if err := devtool.SetGoEnv(); err != nil {
		return err
	}

	for _, s := range steps {
		if err := devtool.Run(ctx, root, s[0], s[1:]...); err != nil {
			return err
		}
	}

	return nil
}
