// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
)

func TestParseSemVerPrerelease(t *testing.T) {
	cases := []struct {
		raw        string
		wantErr    bool
		prerelease []string
	}{
		{"v1.2.3", false, nil},
		{"1.2.3", false, nil},
		{"v0.1.0-rc.1", false, []string{"rc", "1"}},
		{"1.0.0+build.5", false, nil},
		{"1.0.0-rc.1+build.5", false, []string{"rc", "1"}},
		{"", true, nil},
		{"1.2", true, nil},
		{"1.2.x", true, nil},
		{"1.2.3-", true, nil},
	}
	for _, tc := range cases {
		got, err := parseSemVer(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSemVer(%q): expected error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSemVer(%q): unexpected error %v", tc.raw, err)
			continue
		}
		if !prereleaseEqual(got.prerelease, tc.prerelease) {
			t.Errorf("parseSemVer(%q) prerelease = %v, want %v", tc.raw, got.prerelease, tc.prerelease)
		}
	}
}

func TestCompareSemVerPrecedence(t *testing.T) {
	// Strictly increasing precedence chain (semver §11 example, extended past 1.0.0).
	ordered := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"1.1.0",
		"2.0.0",
	}
	parsed := make([]versionParts, len(ordered))
	for i, raw := range ordered {
		v, err := parseSemVer(raw)
		if err != nil {
			t.Fatalf("parseSemVer(%q): %v", raw, err)
		}
		parsed[i] = v
	}
	for i := range parsed {
		if got := compareSemVer(parsed[i], parsed[i]); got != 0 {
			t.Errorf("compareSemVer(%s, %s) = %d, want 0", ordered[i], ordered[i], got)
		}
		for j := i + 1; j < len(parsed); j++ {
			if got := compareSemVer(parsed[i], parsed[j]); got != -1 {
				t.Errorf("compareSemVer(%s, %s) = %d, want -1", ordered[i], ordered[j], got)
			}
			if got := compareSemVer(parsed[j], parsed[i]); got != 1 {
				t.Errorf("compareSemVer(%s, %s) = %d, want 1", ordered[j], ordered[i], got)
			}
		}
	}
}

func TestCompareSemVerReleaseBeatsPrerelease(t *testing.T) {
	rc, err := parseSemVer("v0.1.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	final, err := parseSemVer("v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := compareSemVer(rc, final); got != -1 {
		t.Fatalf("compareSemVer(rc, final) = %d, want -1 (final is newer)", got)
	}
	if got := compareSemVer(final, final); got != 0 {
		t.Fatalf("compareSemVer(final, final) = %d, want 0", got)
	}
}

func TestUpdateCacheRejectsNoncanonicalSemVerTags(t *testing.T) {
	for _, tag := range []string{"v1.2.3.4", "v01.2.3", "v1.2.3-alpha..1", "v1.2.3+"} {
		t.Run(tag, func(t *testing.T) {
			if version, err := parseCanonicalReleaseTag(tag); err == nil {
				t.Fatalf("noncanonical release tag was accepted: %+v", version)
			}
		})
	}
}

func TestReleaseSemVerGreaterHandlesValidAndInvalidVersions(t *testing.T) {
	for _, test := range []struct {
		left  string
		right string
		want  bool
	}{
		{left: "v1.2.0", right: "v1.1.9", want: true},
		{left: "v1.2.0", right: "v1.2.0", want: false},
		{left: "v1.2.0", right: "invalid", want: true},
		{left: "invalid", right: "v1.2.0", want: false},
		{left: "z", right: "a", want: true},
	} {
		if got := releaseSemVerGreater(test.left, test.right); got != test.want {
			t.Fatalf("releaseSemVerGreater(%q, %q) = %v, want %v", test.left, test.right, got, test.want)
		}
	}
}

func TestUpdateSemVerComparisonUsesReleasePrecedence(t *testing.T) {
	for _, test := range []struct {
		left  string
		right string
		want  int
	}{
		{left: "v2.0.0", right: "v1.99.99", want: 1},
		{left: "v1.3.0", right: "v1.2.9", want: 1},
		{left: "v1.2.4", right: "v1.2.3", want: 1},
		{left: "v1.2.3", right: "v1.2.3", want: 0},
		{left: "v1.2.3", right: "v1.2.3-rc.1", want: 1},
		{left: "v1.2.3-rc.1", right: "v1.2.3", want: -1},
		{left: "v1.2.3-rc.2", right: "v1.2.3-rc.10", want: -1},
		{left: "v1.2.3-1", right: "v1.2.3-alpha", want: -1},
		{left: "v1.2.3-beta", right: "v1.2.3-2", want: 1},
		{left: "v1.2.3-alpha", right: "v1.2.3-beta", want: -1},
		{left: "v1.2.3-alpha", right: "v1.2.3-alpha.1", want: -1},
	} {
		t.Run(test.left+"_"+test.right, func(t *testing.T) {
			left, err := parseSemVer(test.left)
			if err != nil {
				t.Fatal(err)
			}
			right, err := parseSemVer(test.right)
			if err != nil {
				t.Fatal(err)
			}
			if got := compareSemVer(left, right); got != test.want {
				t.Fatalf("compareSemVer(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
			}
		})
	}
	for _, raw := range []string{"", "v1.2", "v1.x.3", "v1.2.3-"} {
		if _, err := parseSemVer(raw); err == nil {
			t.Fatalf("parseSemVer(%q) should fail", raw)
		}
	}
}

func prereleaseEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
