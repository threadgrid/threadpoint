// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellLifecycleGuardSerializesGoDuringStaleReviewAndRelease(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	lockPath := installLifecycleLockPath(productHome, commandPath)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	staleBody, err := json.Marshal(installLockOwner{
		PID: 1 << 30, Created: time.Now().UTC(), Heartbeat: time.Now().UTC(),
		Command: commandPath, Token: strings.Repeat("a", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, staleBody, 0o600); err != nil {
		t.Fatal(err)
	}

	caseDir := t.TempDir()
	staleReady := filepath.Join(caseDir, "stale-ready")
	staleContinue := filepath.Join(caseDir, "stale-continue")
	acquired := filepath.Join(caseDir, "acquired")
	releaseStart := filepath.Join(caseDir, "release-start")
	releaseReady := filepath.Join(caseDir, "release-ready")
	releaseContinue := filepath.Join(caseDir, "release-continue")
	helperPath := buildThreadpointInstallLockHelper(t)
	scriptPath := filepath.Join("..", "..", "scripts", "install.sh")
	command := exec.CommandContext(t.Context(), "sh", "-c", `
THREADPOINT_INSTALL_SH_TEST_MODE=1
THREADPOINT_INSTALL_LOCK_TEST_STALE_READY=$5
THREADPOINT_INSTALL_LOCK_TEST_STALE_CONTINUE=$6
THREADPOINT_INSTALL_LOCK_TEST_RELEASE_READY=$9
THREADPOINT_INSTALL_LOCK_TEST_RELEASE_CONTINUE=${10}
export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_TEST_STALE_READY \
  THREADPOINT_INSTALL_LOCK_TEST_STALE_CONTINUE THREADPOINT_INSTALL_LOCK_TEST_RELEASE_READY \
  THREADPOINT_INSTALL_LOCK_TEST_RELEASE_CONTINUE
. "$1"
acquire_install_lifecycle_lock "$2" "$3" "$4"
: >"$7"
while [ ! -e "$8" ]; do sleep 0.01; done
release_install_lifecycle_lock
`, "sh", scriptPath, productHome, commandPath, helperPath, staleReady, staleContinue, acquired, releaseStart, releaseReady, releaseContinue)
	var shellOutput bytes.Buffer
	command.Stdout = &shellOutput
	command.Stderr = &shellOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	wantGoRefused := func(phase string) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		defer cancel()
		if lock, err := acquireInstallLifecycleLock(ctx, productHome, commandPath); err == nil {
			_ = lock.Release()
			t.Fatalf("Go lifecycle lock entered during shell %s", phase)
		}
	}
	waitForInstallLockTestFile(t, staleReady)
	wantGoRefused("stale review")
	if err := os.WriteFile(staleContinue, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForInstallLockTestFile(t, acquired)
	if err := os.WriteFile(releaseStart, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForInstallLockTestFile(t, releaseReady)
	wantGoRefused("release review")
	if err := os.WriteFile(releaseContinue, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("shell lifecycle owner failed: %v\n%s", err, shellOutput.String())
	}
	after, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatalf("Go lifecycle lock unavailable after shell release: %v", err)
	}
	if err := after.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestGoLifecycleGuardSerializesShellDuringRelease(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	lock, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	proceed := make(chan struct{})
	installLifecycleBeforeRelease = func() {
		close(entered)
		<-proceed
	}
	t.Cleanup(func() { installLifecycleBeforeRelease = nil })
	released := make(chan error, 1)
	go func() { released <- lock.Release() }()
	<-entered
	helperPath := buildThreadpointInstallLockHelper(t)
	scriptPath := filepath.Join("..", "..", "scripts", "install.sh")
	command := exec.CommandContext(t.Context(), "sh", "-c", `
THREADPOINT_INSTALL_SH_TEST_MODE=1
THREADPOINT_INSTALL_LOCK_ATTEMPTS=1
export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
. "$1"
acquire_install_lifecycle_lock "$2" "$3" "$4" || exit 42
release_install_lifecycle_lock
`, "sh", scriptPath, productHome, commandPath, helperPath)
	if err := command.Run(); err == nil {
		close(proceed)
		t.Fatal("shell lifecycle lock entered while Go release was between review and removal")
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
			close(proceed)
			t.Fatalf("unexpected shell contention result: %v", err)
		}
	}
	close(proceed)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}

func waitForInstallLockTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
