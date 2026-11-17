package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// noConfiguration makes sure run() cannot get past the configuration, and so
// cannot reach a database, whatever the developer's environment holds.
func noConfiguration(t *testing.T) {
	t.Helper()
	t.Setenv("REGISTRY_DATABASE_URL", "")
	t.Setenv("REGISTRY_PUBLIC_URL", "")
}

// The version is answered without a configuration: anything else run() does
// fails on the missing database URL.
func TestVersionNeedsNoConfiguration(t *testing.T) {
	noConfiguration(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run(nil, log, io.Discard); err == nil || !strings.Contains(err.Error(), "REGISTRY_DATABASE_URL is required") {
		t.Fatalf("run() without a configuration = %v, want the missing database URL", err)
	}
	for _, arg := range []string{"version", "--version", "-version"} {
		var out bytes.Buffer
		if err := run([]string{arg}, log, &out); err != nil {
			t.Fatalf("run(%q): %v", arg, err)
		}
		if got := out.String(); !strings.HasPrefix(got, "openlinker-registry "+version) || !strings.HasSuffix(got, "\n") {
			t.Errorf("run(%q) printed %q", arg, got)
		}
	}
}

// "version" is only a command on its own; with more arguments run() goes on
// to the configuration, as it does for every other spelling.
func TestVersionIsNotAFlagAmongOthers(t *testing.T) {
	noConfiguration(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var out bytes.Buffer
	err := run([]string{"version", "-listen", ":0"}, log, &out)
	if err == nil || !strings.Contains(err.Error(), "REGISTRY_DATABASE_URL is required") {
		t.Fatalf("run = %v, want the missing database URL", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}

func TestVersionLine(t *testing.T) {
	oldVersion, oldCommit := version, commit
	defer func() { version, commit = oldVersion, oldCommit }()

	version, commit = "v1.2.3", "0123456789abcdef0123456789abcdef01234567"
	if got, want := versionLine(), "openlinker-registry v1.2.3 (0123456789abcdef0123456789abcdef01234567)"; got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
	if got := buildCommit(); got != commit {
		t.Errorf("buildCommit() = %q, want the stamped commit", got)
	}
}
