package model

import "testing"

// Only a complete semantic version is compared as one. Anything else gives
// 0, and callers order such versions by publication.
func TestCompareVersionsGrammar(t *testing.T) {
	ordered := [][2]string{
		{"1.0.0", "1.0.1"}, {"1.9.0", "1.10.0"}, {"v1.2.3", "1.2.4"}, {"1.0.0-alpha", "1.0.0"},
		{"1.0.0-alpha", "1.0.0-alpha.1"}, {"1.0.0-alpha.1", "1.0.0-alpha.beta"}, {"1.0.0-2", "1.0.0-10"},
		{"1.0.0-rc.1+build.5", "1.0.0"}, {"1.0.0-x-y", "1.0.0-x-z"},
	}
	for _, p := range ordered {
		if CompareVersions(p[0], p[1]) >= 0 || CompareVersions(p[1], p[0]) <= 0 {
			t.Errorf("%s must be lower than %s", p[0], p[1])
		}
	}
	for _, same := range [][2]string{{"1.0.0", "1.0.0+build"}, {"1.0.0+a", "1.0.0+b.c-d"}, {"v2.0.0", "2.0.0"}} {
		if CompareVersions(same[0], same[1]) != 0 {
			t.Errorf("%s and %s have the same precedence", same[0], same[1])
		}
	}
	for _, odd := range []string{
		"1.0.0-build_2", "1.0.0+build_2", "1.0.0-", "1.0.0+", "1.0.0-a..b", "1.0.0-01", "1.0.0-a.01", "01.0.0", "1.00.0",
		"1.0", "1.0.0.0", "1.0.0-é", "1.0.0 ", " 1.0.0", "1.0.0-a b", "1.0.0+a+b", "nightly", "", "v", "1.0.x", "1.0.0-a@b",
		"99999999999999999999.0.0",
	} {
		for _, other := range []string{"0.0.1", "1.0.0", "2.0.0"} {
			if CompareVersions(odd, other) != 0 || CompareVersions(other, odd) != 0 {
				t.Errorf("%q is not a semantic version and must not be ordered against %s", odd, other)
			}
		}
	}
}
