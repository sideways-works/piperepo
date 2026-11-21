// Command hooks points git at the repository's hooks, and checks a commit
// message for the commit-msg hook.
//
//	bazel run //:hooks
//	bazel run //:commit-msg -- commit-msg <file>
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sideways-works/piperepo/tools/internal/devtool"
)

var (
	errUsage  = errors.New("usage: bazel run //:hooks, or //:commit-msg -- commit-msg <file>")
	errRefuse = errors.New("commit message refused")
	subjectRE = regexp.MustCompile(`^(feat|fix|chore|refactor|docs|test|perf|build|ci|style)(\([a-z0-9-]+\))?: .+`)
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		if !errors.Is(err, errRefuse) {
			fmt.Fprintln(os.Stderr, err)
		}

		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 2 && args[0] == "commit-msg" {
		return commitMsg(args[len(args)-1])
	}

	switch {
	case len(args) == 0:
		root, err := devtool.Workspace("hooks")
		if err != nil {
			return err
		}

		if err := devtool.Run(ctx, root, "git", "config", "core.hooksPath", "tools/hooks"); err != nil {
			return err
		}

		fmt.Fprintln(os.Stdout, "hooks: tools/hooks (pre-commit runs bazel run //:lint)")

		return nil
	default:
		return errUsage
	}
}

// commitMsg checks the message file git names, relative to where git ran the hook.
func commitMsg(file string) error {
	msg, err := os.ReadFile(devtool.Caller(file))
	if err != nil {
		return fmt.Errorf("commit-msg: %w", err)
	}

	if problem := messageProblem(string(msg)); problem != "" {
		fmt.Fprintln(os.Stderr, "commit-msg: "+problem)

		return errRefuse
	}

	return nil
}

// messageProblem says what is wrong with a commit message, if anything: it
// is one conventional subject line of at most 72 characters, with no body
// and no trailers. Comment lines and trailing blank lines don't count.
func messageProblem(msg string) string {
	var lines []string

	for line := range strings.SplitSeq(strings.TrimSuffix(msg, "\n"), "\n") {
		if !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}

	for len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) == 0 {
		lines = []string{""}
	}

	subject := lines[0]

	switch {
	case len(lines) != 1:
		return fmt.Sprintf("one subject line and nothing else (found %d lines)", len(lines))
	case utf8.RuneCountInString(subject) > 72:
		return fmt.Sprintf("subject is %d characters, at most 72", utf8.RuneCountInString(subject))
	case !subjectRE.MatchString(subject):
		return "subject must be conventional: type(scope)?: summary"
	}

	return ""
}
