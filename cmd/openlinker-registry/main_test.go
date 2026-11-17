package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// The version is answered without a configuration: no database URL is set
// here, and anything else run() does would fail on that.
func TestVersionNeedsNoConfiguration(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var out bytes.Buffer
	err := run([]string{"version", "-listen", ":0"}, log, &out)
	if err == nil || !strings.HasPrefix(err.Error(), "config:") {
		t.Fatalf("run = %v, want a configuration error", err)
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
