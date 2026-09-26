// SPDX-License-Identifier: Apache-2.0

package home

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveWithOverrideUsesDefaultHome(t *testing.T) {
	home := t.TempDir()

	got, err := ResolveWithOverride(home, "")
	if err != nil {
		t.Fatal(err)
	}
	want := mustResolveTrustedBoundary(t, filepath.Join(home, ".threadpoint"))
	if got != want {
		t.Fatalf("home = %q, want %q", got, want)
	}
}

func TestResolveAndResolveUserHomeUseAbsoluteHomes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("THREADPOINT_HOME", "")
	got, err := Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".threadpoint"); got != want {
		t.Fatalf("Resolve(%q) = %q, want %q", home, got, want)
	}
	if got, err := ResolveUserHome(""); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("ResolveUserHome(empty) = %q, %v", got, err)
	}
}

func TestResolveWithOverrideUsesEnvAndIgnoresEmptyEnv(t *testing.T) {
	home := t.TempDir()
	productHome := filepath.Join(t.TempDir(), "threadpoint-home")
	t.Setenv("THREADPOINT_HOME", productHome)

	got, err := ResolveWithOverride(home, "")
	if err != nil {
		t.Fatal(err)
	}
	wantProductHome := mustResolveTrustedBoundary(t, productHome)
	if got != wantProductHome {
		t.Fatalf("home = %q, want env %q", got, wantProductHome)
	}

	t.Setenv("THREADPOINT_HOME", "   ")
	got, err = ResolveWithOverride(home, "")
	if err != nil {
		t.Fatal(err)
	}
	want := mustResolveTrustedBoundary(t, filepath.Join(home, ".threadpoint"))
	if got != want {
		t.Fatalf("empty env home = %q, want default %q", got, want)
	}
}

func TestResolveWithOverrideFlagOverridesEnv(t *testing.T) {
	home := t.TempDir()
	envHome := filepath.Join(t.TempDir(), "env-home")
	flagHome := filepath.Join(t.TempDir(), "flag-home")
	t.Setenv("THREADPOINT_HOME", envHome)

	got, err := ResolveWithOverride(home, flagHome)
	if err != nil {
		t.Fatal(err)
	}
	want := mustResolveTrustedBoundary(t, flagHome)
	if got != want {
		t.Fatalf("home = %q, want flag %q", got, want)
	}
}

func TestResolveWithOverrideRejectsRelativeHomes(t *testing.T) {
	home := t.TempDir()
	if _, err := ResolveWithOverride(home, "relative/threadpoint"); err == nil || !strings.Contains(err.Error(), "threadpoint home must be absolute") {
		t.Fatalf("expected relative flag home rejection, got %v", err)
	}

	t.Setenv("THREADPOINT_HOME", "relative/threadpoint")
	if _, err := ResolveWithOverride(home, ""); err == nil || !strings.Contains(err.Error(), "threadpoint home must be absolute") {
		t.Fatalf("expected relative env home rejection, got %v", err)
	}
}

func TestResolveWithOverrideRejectsUnsafeHomes(t *testing.T) {
	home := t.TempDir()
	for name, path := range map[string]string{
		"filesystem root": string(filepath.Separator),
		"user home":       home,
		"agents root":     filepath.Join(home, ".agents"),
	} {
		if _, err := ResolveWithOverride(home, path); err == nil {
			t.Fatalf("expected %s to be rejected", name)
		}
	}
}

func TestResolveWithOverrideRejectsSymlinkAndNonDirectoryHome(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "threadpoint-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ResolveWithOverride(home, link); err == nil || !strings.Contains(err.Error(), "threadpoint home cannot be a symlink") {
		t.Fatalf("expected symlink home rejection, got %v", err)
	}

	fileHome := filepath.Join(home, "threadpoint-file")
	if err := os.WriteFile(fileHome, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWithOverride(home, fileHome); err == nil || !strings.Contains(err.Error(), "threadpoint home is not a directory") {
		t.Fatalf("expected non-directory home rejection, got %v", err)
	}
}

func TestResolveWithOverrideTrustsConfiguredHomeAncestorsAndReturnsPhysicalBoundary(t *testing.T) {
	base := t.TempDir()
	physical := filepath.Join(base, "physical")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(base, "logical")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	configured := filepath.Join(logical, "threadpoint-home")

	got, err := ResolveWithOverride(t.TempDir(), configured)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(physical, "threadpoint-home")
	if got != want {
		t.Fatalf("resolved home = %q, want physical configured boundary %q", got, want)
	}
}

func TestResolveWithOverrideRejectsTrustedAncestorThatResolvesToBroadAgentsRoot(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	logicalHome := filepath.Join(base, "logical-home")
	if err := os.Symlink(home, logicalHome); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	configured := filepath.Join(logicalHome, ".agents")
	if _, err := ResolveWithOverride(home, configured); err == nil || !strings.Contains(err.Error(), "cannot resolve to the entire .agents directory") {
		t.Fatalf("expected physically broad configured home rejection, got %v", err)
	}
}

func mustResolveTrustedBoundary(t *testing.T, path string) string {
	t.Helper()
	resolved, err := resolveTrustedBoundary(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestResolveRejectsUnavailableUserHome(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("HOME resolution is Unix-specific")
	}
	t.Setenv("HOME", "")
	if _, err := ResolveWithOverride("", ""); err == nil {
		t.Fatal("accepted unavailable user home")
	}
}
func TestResolveRejectsLoopingAncestor(t *testing.T) {
	home := t.TempDir()
	loop := filepath.Join(home, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ResolveWithOverride(home, filepath.Join(loop, "state")); err == nil {
		t.Fatal("accepted looping ancestor")
	}
}
