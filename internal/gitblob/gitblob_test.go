package gitblob

import "testing"

// Expected values were produced with `git hash-object --stdin` on the same bytes.
func TestSHA1MatchesGit(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"empty", "", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"},
		{"lf", "line one\nline two\n", "e5c5c5583f49a34e86ce622b59363df99e09d4c6"},
		{"crlf is hashed raw, not normalized", "line one\r\nline two\r\n", "cf9b2a85b62bc2fd67c5ed43a1d0009df848ac8a"},
		{"multibyte utf-8 length is in bytes", "名前: héllo 世界 🚀\n", "0b3006e23f215d22aced4615f8b5d86000aefa65"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SHA1([]byte(tc.data)); got != tc.want {
				t.Fatalf("SHA1(%q) = %s, want %s", tc.data, got, tc.want)
			}
		})
	}
}
