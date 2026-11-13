package registry

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClipMakesUpstreamTextStorable(t *testing.T) {
	for in, want := range map[string]string{
		"plain":       "plain",
		"nul\x00byte": "nulbyte",
		"bad\xffutf8": "bad�utf8",
		"日本語日本語":      "日本語", // cut on a rune boundary at 10 bytes
	} {
		got := clip(in, 10)
		if got != want || !utf8.ValidString(got) || strings.Contains(got, "\x00") || len(got) > 10 {
			t.Errorf("clip(%q) = %q, want %q", in, got, want)
		}
	}
	if boundedText("a\x00b", 10) || boundedText("\xff", 10) || boundedText("toolong", 3) || !boundedText("ok", 2) {
		t.Fatal("boundedText must refuse NUL bytes, invalid UTF-8 and over-long text")
	}
}
