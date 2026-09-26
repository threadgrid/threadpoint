// SPDX-License-Identifier: Apache-2.0

package safepath

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"CLAUDE.md", "claude.md"},
		{"  Spaced Name ", "spaced-name"},
		{"...dots...", "dots"},
		{"@@@", ""},
		{"a/b", "a-b"},
	}
	for _, tc := range cases {
		if got := SanitizeName(tc.in); got != tc.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeSegmentFallsBackToImported(t *testing.T) {
	if got := SanitizeSegment("@@@"); got != "imported" {
		t.Errorf("SanitizeSegment(@@@) = %q, want imported", got)
	}
	if got := SanitizeSegment("Keep"); got != "keep" {
		t.Errorf("SanitizeSegment(Keep) = %q, want keep", got)
	}
}

func TestSanitizeRelStaysWithinDestination(t *testing.T) {
	cases := map[string]string{
		".codex/AGENTS.md": "codex/agents.md",
		"../../etc/passwd": "imported/etc/passwd", // ".." sanitizes to a contained "imported" segment
		"/abs/Path":        "abs/path",
		"":                 "artifact",
		".":                "artifact",
		"a/../../../b":     "imported/b",
	}
	for in, want := range cases {
		got := SanitizeRelative(in)
		if got != want {
			t.Errorf("SanitizeRelative(%q) = %q, want %q", in, got, want)
		}
		if filepath.IsAbs(got) || strings.HasPrefix(got, "../") || strings.Contains(got, "/../") {
			t.Errorf("SanitizeRelative(%q) escaped: %q", in, got)
		}
	}
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := filepath.FromSlash("/srv/root")
	for _, bad := range []string{"..", "../x", "/etc/passwd", "."} {
		if _, err := Join(root, bad); !errors.Is(err, ErrUnsafe) {
			t.Errorf("Join(%q) error = %v, want ErrUnsafe", bad, err)
		}
	}
	got, err := Join(root, ".agents/knowledge/x.md")
	if err != nil {
		t.Fatalf("Join valid path errored: %v", err)
	}
	if got != filepath.Join(root, ".agents", "knowledge", "x.md") {
		t.Fatalf("unexpected join result: %q", got)
	}
}

func TestIsUnder(t *testing.T) {
	root := filepath.Join("srv", "root")
	if !IsWithin(root, root) {
		t.Fatal("root must be under itself")
	}
	if !IsWithin(filepath.Join(root, "child"), root) {
		t.Fatal("child must be under root")
	}
	if IsWithin(filepath.Join("srv", "other"), root) {
		t.Fatal("sibling must not be under root")
	}
}

func FuzzSanitizeRel(f *testing.F) {
	for _, seed := range []string{"a/b", "../../etc/passwd", "/abs", ".", "", "..\\..\\x", "a/../../../b", "....//", "föö/bar", "x/./y"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, rel string) {
		got := SanitizeRelative(rel)
		if got == "" {
			t.Fatalf("SanitizeRelative(%q) returned empty", rel)
		}
		if filepath.IsAbs(got) || got == ".." || strings.HasPrefix(got, "../") ||
			strings.Contains(got, "/../") || strings.HasSuffix(got, "/..") {
			t.Fatalf("SanitizeRelative(%q) escaped: %q", rel, got)
		}
		root := filepath.FromSlash("/safe/root")
		joined := filepath.Join(root, filepath.FromSlash(got))
		if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
			t.Fatalf("SanitizeRelative(%q)=%q joined outside root: %q", rel, got, joined)
		}
	})
}
