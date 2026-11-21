package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMessageProblem(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"fix(cli): a bug\n", ""},
		{"feat: a thing", ""},
		{"docs(book): a page\n\n\n", ""},
		{"fix: a bug\n# Please enter the commit message\n#\n", ""},
		{"fix: a bug\n\nA body.\n", "one subject line and nothing else (found 3 lines)"},
		{"fix: a bug\nCo-Authored-By: x\n", "one subject line and nothing else (found 2 lines)"},
		{"fix: " + strings.Repeat("é", 68) + "\n", "subject is 73 characters, at most 72"},
		{"fix: " + strings.Repeat("é", 67) + "\n", ""},
		{"Fix the bug\n", "subject must be conventional: type(scope)?: summary"},
		{"revert: x\n", "subject must be conventional: type(scope)?: summary"},
		{"fix(Cli): x\n", "subject must be conventional: type(scope)?: summary"},
		{"", "subject must be conventional: type(scope)?: summary"},
	}

	for _, c := range cases {
		if got := messageProblem(c.msg); got != c.want {
			t.Errorf("%q: got %q, want %q", c.msg, got, c.want)
		}
	}
}

func TestCommitMsgReadsTheFileGitNames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BUILD_WORKING_DIRECTORY", dir)

	if err := os.WriteFile(filepath.Join(dir, "MSG"), []byte("fix: a bug\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), []string{"commit-msg", "MSG"}); err != nil {
		t.Fatalf("a good message was refused: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "MSG"), []byte("a bug\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), []string{"commit-msg", "MSG"}); !errors.Is(err, errRefuse) {
		t.Fatalf("a bad message passed: %v", err)
	}
}

func TestUnknownArgumentsAreAUsageError(t *testing.T) {
	if err := run(context.Background(), []string{"pre-push"}); !errors.Is(err, errUsage) {
		t.Fatalf("got %v", err)
	}
}
