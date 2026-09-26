// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/threadgrid/threadpoint/safefs"
)

func TestInstallLockOwnerHelperIsStrictBoundedAndReplacementSafe(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "owner.lock")
	now := time.Now().UTC().Truncate(time.Second)
	owner := installLockOwner{
		PID: 1234, Created: now, Heartbeat: now,
		Command: filepath.Join(root, threadpointProductName), Token: strings.Repeat("a", 64),
	}
	body, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	writeInstallHelperFile(t, lockPath, append(body, '\n'))

	var output bytes.Buffer
	if err := runInstallLockOwnerInspect(&output, []string{lockPath}); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(output.String())
	if len(fields) != 3 || fields[1] != "1234" || fields[2] != owner.Token {
		t.Fatalf("lock owner inspection = %q", output.String())
	}
	identity := fields[0]
	heartbeat := now.Add(time.Minute)
	if err := runInstallLockHeartbeat([]string{lockPath, identity, owner.Token, heartbeat.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	updatedBody, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var updated installLockOwner
	if err := json.Unmarshal(updatedBody, &updated); err != nil || !updated.Heartbeat.Equal(heartbeat) {
		t.Fatalf("updated lock owner = %#v, err=%v", updated, err)
	}
	if got := installHelperPathIdentity(t, lockPath); got != identity {
		t.Fatalf("heartbeat changed lock identity: %s -> %s", identity, got)
	}

	malformed := filepath.Join(root, "malformed.lock")
	writeInstallHelperFile(t, malformed, append(bytes.TrimSuffix(body, []byte("}")), []byte(",\"unknown\":true}\n")...))
	if err := runInstallLockOwnerInspect(io.Discard, []string{malformed}); err == nil {
		t.Fatal("lock owner with unknown field accepted")
	}
	duplicate := filepath.Join(root, "duplicate.lock")
	duplicateBody := strings.Replace(string(body), `"pid":1234`, `"pid":1234,"pid":4321`, 1)
	writeInstallHelperFile(t, duplicate, append([]byte(duplicateBody), '\n'))
	if err := runInstallLockOwnerInspect(io.Discard, []string{duplicate}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate lock owner key error = %v", err)
	}
	fifo := filepath.Join(root, "fifo.lock")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := runInstallLockOwnerInspect(io.Discard, []string{fifo}); err == nil {
		t.Fatal("FIFO lock owner accepted")
	}
	oversized := filepath.Join(root, "oversized.lock")
	file, err := os.OpenFile(oversized, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxInstallLockOwnerBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runInstallLockOwnerInspect(io.Discard, []string{oversized}); err == nil {
		t.Fatal("oversized lock owner accepted")
	}

	reviewed := lockPath + ".reviewed"
	replacementBody := []byte("replacement must remain untouched\n")
	inheritedInstallHelperAfterLockOwnerSnapshot = func(path string) {
		inheritedInstallHelperAfterLockOwnerSnapshot = nil
		if err := os.Rename(path, reviewed); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(path, replacementBody, 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { inheritedInstallHelperAfterLockOwnerSnapshot = nil })
	if err := runInstallLockHeartbeat([]string{lockPath, identity, owner.Token, now.Add(2 * time.Minute).Format(time.RFC3339)}); err == nil {
		t.Fatal("heartbeat accepted a final-component lock replacement")
	}
	if got, err := os.ReadFile(lockPath); err != nil || !bytes.Equal(got, replacementBody) {
		t.Fatalf("replacement lock changed: %q, err=%v", got, err)
	}
	var reviewedOwner installLockOwner
	if got, err := os.ReadFile(reviewed); err != nil || json.Unmarshal(got, &reviewedOwner) != nil || !reviewedOwner.Heartbeat.Equal(heartbeat) {
		t.Fatalf("reviewed lock generation changed: %#v, err=%v", reviewedOwner, err)
	}
}

func TestInstallLockGuardRevalidatesParentAfterFlockAndWaitsForEOF(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "guard-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	guardPath := filepath.Join(parent, ".guard")
	displaced := parent + "-displaced"
	inheritedInstallHelperAfterLockAcquire = func(string) {
		if err := os.Rename(parent, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var stdout bytes.Buffer
	err := runInstallLockGuard(&stdout, strings.NewReader(""), []string{guardPath, "1"})
	inheritedInstallHelperAfterLockAcquire = nil
	if err == nil || stdout.Len() != 0 {
		t.Fatalf("post-flock parent ABA result: stdout=%q err=%v", stdout.String(), err)
	}

	guardPath = filepath.Join(parent, ".stable-guard")
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	guardDone := make(chan error, 1)
	go func() {
		guardDone <- runInstallLockGuard(stdoutWriter, stdinReader, []string{guardPath, "2"})
		_ = stdoutWriter.Close()
	}()
	line, err := bufio.NewReader(stdoutReader).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("guard readiness = %q, %v", line, err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	contender, err := safefs.AcquireKernelFileLockRoot(context.Background(), root, filepath.Base(guardPath), guardPath, 0, time.Millisecond)
	if contender != nil {
		_ = contender.Release()
	}
	_ = root.Close()
	if err == nil {
		t.Fatal("guard released before control-pipe EOF")
	}
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-guardDone:
		if err != nil {
			t.Fatalf("guard shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guard did not release after control-pipe EOF")
	}
}

func TestAcquireInstallLifecycleLockCleansUpPartialCapabilities(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (string, string)
	}{
		{
			name: "product home is a file",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				productHome := filepath.Join(t.TempDir(), "product")
				if err := os.WriteFile(productHome, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				return productHome, filepath.Join(t.TempDir(), threadpointProductName)
			},
		},
		{
			name: "command directory is missing",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				return filepath.Join(t.TempDir(), ".threadpoint"), filepath.Join(t.TempDir(), "missing", threadpointProductName)
			},
		},
		{
			name: "anchor name is a directory",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				productHome := filepath.Join(t.TempDir(), ".threadpoint")
				commandDir := t.TempDir()
				commandPath := filepath.Join(commandDir, threadpointProductName)
				if err := os.Mkdir(installLifecycleAnchorPath(commandPath), 0o700); err != nil {
					t.Fatal(err)
				}
				return productHome, commandPath
			},
		},
		{
			name: "transactions name is a file",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				productHome := filepath.Join(t.TempDir(), ".threadpoint")
				installs := filepath.Join(productHome, "installs")
				if err := os.MkdirAll(installs, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(installs, "transactions"), []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				return productHome, filepath.Join(t.TempDir(), threadpointProductName)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			productHome, commandPath := test.setup(t)
			lock, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
			if err == nil {
				_ = lock.Release()
				t.Fatal("unsafe lifecycle shape was accepted")
			}
			for _, path := range []string{installLifecycleLockPath(productHome, commandPath), installLifecycleAnchorPath(commandPath)} {
				if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() {
					t.Fatalf("partial lifecycle acquisition retained lock file %s", path)
				}
			}
		})
	}
}

func TestInstallLifecycleLockDetectsReplacedCapabilities(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, fixture installTransactionFixture, lock *installLifecycleLock)
	}{
		{
			name: "heartbeat failure",
			mutate: func(_ *testing.T, _ installTransactionFixture, lock *installLifecycleLock) {
				lock.mu.Lock()
				lock.heartbeatErr = errors.New("injected heartbeat failure")
				lock.mu.Unlock()
			},
		},
		{
			name: "lock parent replacement",
			mutate: func(t *testing.T, fixture installTransactionFixture, _ *installLifecycleLock) {
				t.Helper()
				path := filepath.Join(fixture.productHome, "installs")
				if err := os.Rename(path, path+"-detached"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "command parent replacement",
			mutate: func(t *testing.T, fixture installTransactionFixture, _ *installLifecycleLock) {
				t.Helper()
				if err := os.Rename(fixture.commandDir, fixture.commandDir+"-detached"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(fixture.commandDir, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "transactions replacement",
			mutate: func(t *testing.T, fixture installTransactionFixture, _ *installLifecycleLock) {
				t.Helper()
				path := filepath.Join(fixture.productHome, "installs", "transactions")
				if err := os.Rename(path, path+"-detached"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock file replacement",
			mutate: func(t *testing.T, _ installTransactionFixture, lock *installLifecycleLock) {
				t.Helper()
				name := filepath.Base(lock.path)
				if err := lock.lockParent.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := lock.lockParent.WriteFile(name, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "anchor file replacement",
			mutate: func(t *testing.T, _ installTransactionFixture, lock *installLifecycleLock) {
				t.Helper()
				name := filepath.Base(lock.anchorPath)
				if err := lock.commandRoot.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := lock.commandRoot.WriteFile(name, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "bundle root replacement",
			mutate: func(t *testing.T, _ installTransactionFixture, lock *installLifecycleLock) {
				t.Helper()
				bundle := filepath.Join(t.TempDir(), "bundle")
				if err := os.Mkdir(bundle, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := lock.PinBundleRoot(bundle); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(bundle, bundle+"-detached"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(bundle, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "transaction backup replacement",
			mutate: func(t *testing.T, _ installTransactionFixture, lock *installLifecycleLock) {
				t.Helper()
				backup, err := createPinnedInstallTransactionBackup(lock)
				if err != nil {
					t.Fatal(err)
				}
				if err := lock.retainTransactionBackup(backup); err != nil {
					t.Fatal(err)
				}
				if err := backup.transactionsRoot.Rename(backup.base, backup.base+"-detached"); err != nil {
					t.Fatal(err)
				}
				if err := backup.transactionsRoot.Mkdir(backup.base, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, false)
			lock, err := acquireInstallLifecycleLock(context.Background(), fixture.productHome, fixture.commandPath)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, fixture, lock)
			if err := lock.Validate(); err == nil {
				t.Fatal("replaced lifecycle capability remained valid")
			}
			_ = lock.Release()
		})
	}
}

func TestInstallLifecycleLockHeartbeatPreventsLiveReclaim(t *testing.T) {
	oldInterval, oldLease := installLockHeartbeatInterval, installLockLease
	installLockHeartbeatInterval = 5 * time.Millisecond
	installLockLease = 80 * time.Millisecond
	t.Cleanup(func() {
		installLockHeartbeatInterval = oldInterval
		installLockLease = oldLease
	})

	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	first, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Release() }()

	ctx, cancel := context.WithTimeout(context.Background(), 220*time.Millisecond)
	defer cancel()
	if second, err := acquireInstallLifecycleLock(ctx, productHome, commandPath); err == nil {
		_ = second.Release()
		t.Fatal("a live heartbeat lock was reclaimed by a concurrent lifecycle operation")
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("original lifecycle lock lost ownership: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatalf("lock was not available after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallLifecycleLockReclaimsDeadOwner(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	path := installLifecycleLockPath(productHome, commandPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(installLockOwner{
		PID:       1 << 30,
		Created:   time.Now().UTC(),
		Heartbeat: time.Now().UTC(),
		Command:   commandPath,
		Token:     strings.Repeat("a", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatalf("dead lifecycle owner was not reclaimed: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallLifecycleLockSerializesShellOwner(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	lock, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join("..", "..", "scripts", "install.sh")
	helperPath := buildThreadpointInstallLockHelper(t)
	tryShellLock := func() error {
		command := exec.CommandContext(context.Background(), "sh", "-c", `
THREADPOINT_INSTALL_SH_TEST_MODE=1
THREADPOINT_INSTALL_LOCK_ATTEMPTS=1
export THREADPOINT_INSTALL_SH_TEST_MODE THREADPOINT_INSTALL_LOCK_ATTEMPTS
. "$1"
acquire_install_lifecycle_lock "$2" "$3" "$4" || exit 42
release_install_lifecycle_lock
`, "sh", scriptPath, productHome, commandPath, helperPath)
		return command.Run()
	}
	if err := tryShellLock(); err == nil {
		_ = lock.Release()
		t.Fatal("shell installer entered the Go lifecycle lock")
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
			_ = lock.Release()
			t.Fatalf("unexpected shell lock contention result: %v", err)
		}
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := tryShellLock(); err != nil {
		t.Fatalf("shell installer could not acquire the released Go lifecycle lock: %v", err)
	}
}

func TestInstallLifecycleLockSurvivesCommandDirectoryReplacement(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandDir := t.TempDir()
	commandPath := filepath.Join(commandDir, threadpointProductName)
	first, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	oldCommandDir := commandDir + "-old"
	if err := os.Rename(commandDir, oldCommandDir); err != nil {
		_ = first.Release()
		t.Fatal(err)
	}
	if err := os.Mkdir(commandDir, 0o755); err != nil {
		_ = first.Release()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	if second, err := acquireInstallLifecycleLock(ctx, productHome, commandPath); err == nil {
		_ = second.Release()
		_ = first.Release()
		t.Fatal("replacement command directory created a duplicate lifecycle lock namespace")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallLifecycleLockSurvivesProductInstallsDirectoryReplacement(t *testing.T) {
	productHome := filepath.Join(t.TempDir(), ".threadpoint")
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	first, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatal(err)
	}
	installsDir := filepath.Join(productHome, "installs")
	if err := os.Rename(installsDir, installsDir+"-old"); err != nil {
		_ = first.Release()
		t.Fatal(err)
	}
	if err := os.Mkdir(installsDir, 0o700); err != nil {
		_ = first.Release()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	if second, err := acquireInstallLifecycleLock(ctx, productHome, commandPath); err == nil {
		_ = second.Release()
		_ = first.Release()
		t.Fatal("replacement product installs directory created a duplicate lifecycle lock namespace")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireInstallLifecycleLock(context.Background(), productHome, commandPath)
	if err != nil {
		t.Fatalf("lock was not available after the original owner released it: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallLifecycleGuardUsesSharedCommandDirectoryIdentity(t *testing.T) {
	commandPath := filepath.Join(t.TempDir(), threadpointProductName)
	first := installLifecycleGuardPath(filepath.Join(t.TempDir(), ".threadpoint-a"), commandPath)
	second := installLifecycleGuardPath(filepath.Join(t.TempDir(), ".threadpoint-b"), commandPath)
	sum := sha256.Sum256([]byte(filepath.Clean(commandPath)))
	want := filepath.Join(filepath.Dir(commandPath), "."+filepath.Base(commandPath)+".install-"+hex.EncodeToString(sum[:])+".guard")
	if first != want || second != want {
		t.Fatalf("shared lifecycle guard paths = %q, %q; want %q", first, second, want)
	}
}

func TestInstallLifecycleReleasePreservesLastMomentLockReplacement(t *testing.T) {
	tests := []struct {
		name       string
		targetPath func(*installLifecycleLock) string
	}{
		{name: "lock", targetPath: func(lock *installLifecycleLock) string { return lock.path }},
		{name: "anchor", targetPath: func(lock *installLifecycleLock) string { return lock.anchorPath }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstallTransactionFixture(t, false)
			lock := fixture.acquire(t)
			targetPath := test.targetPath(lock)
			targetBase := filepath.Base(targetPath)
			hookCalled := false
			previousHook := installRemovalBeforeQuarantine
			installRemovalBeforeQuarantine = func(root *os.Root, base string) {
				if base != targetBase {
					return
				}
				hookCalled = true
				if err := root.Remove(base); err != nil {
					t.Fatal(err)
				}
				if err := root.WriteFile(base, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { installRemovalBeforeQuarantine = previousHook })

			if err := lock.Release(); err == nil {
				t.Fatal("lifecycle release accepted a last-moment lock replacement")
			}
			if !hookCalled {
				t.Fatal("lifecycle release did not use identity-safe quarantine")
			}
			body, err := os.ReadFile(targetPath)
			if err != nil || string(body) != "replacement" {
				t.Fatalf("lifecycle release changed replacement: body=%q err=%v", body, err)
			}
		})
	}
}

func TestInstallLockAcquisitionReleasesEarlierLocksAfterInvalidEntry(t *testing.T) {
	for _, boundary := range []string{"guard", "owner", "anchor"} {
		t.Run(boundary, func(t *testing.T) {
			home, commandDir := t.TempDir(), t.TempDir()
			command := filepath.Join(commandDir, "fixture-command")
			path := installLifecycleGuardPath(home, command)
			switch boundary {
			case "owner":
				path = installLifecycleLockPath(home, command)
			case "anchor":
				path = installLifecycleAnchorPath(command)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			lock, err := acquireInstallLifecycleLock(ctx, home, command)
			if err == nil {
				_ = lock.Release()
				t.Fatal("directory accepted as lock entry")
			}
			if info, err := os.Lstat(path); err != nil || !info.IsDir() {
				t.Fatalf("invalid entry was removed: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			// A failed later acquisition must release every earlier kernel and owner
			// lock so a repaired installation can immediately retry.
			retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
			defer retryCancel()
			lock, err = acquireInstallLifecycleLock(retryCtx, home, command)
			if err != nil {
				t.Fatalf("failed acquisition leaked a lock: %v", err)
			}
			if err := lock.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallLockAcquisitionCleansUpWhenOwnerHandleCannotBeRetained(t *testing.T) {
	for _, failure := range []string{"owner-open", "anchor-open", "owner-identity", "anchor-identity"} {
		t.Run(failure, func(t *testing.T) {
			home, commandDir := t.TempDir(), t.TempDir()
			command := filepath.Join(commandDir, "fixture-command")
			original := openInstallLockOwnerFile
			t.Cleanup(func() { openInstallLockOwnerFile = original })
			sentinel := errors.New("owner handle unavailable")
			calls, stop := 0, 1
			if strings.HasPrefix(failure, "anchor-") {
				stop = 2
			}
			openInstallLockOwnerFile = func(root *os.Root, name string) (*os.File, error) {
				calls++
				if calls != stop {
					return original(root, name)
				}
				if strings.HasSuffix(failure, "open") {
					return nil, sentinel
				}
				return os.CreateTemp(t.TempDir(), "foreign-owner-")
			}
			lock, err := acquireInstallLifecycleLock(context.Background(), home, command)
			if err == nil || lock != nil {
				if lock != nil {
					_ = lock.Release()
				}
				t.Fatalf("failed retention accepted: %v", err)
			}
			if strings.HasSuffix(failure, "open") && !errors.Is(err, sentinel) {
				t.Fatalf("lost open error: %v", err)
			}
			openInstallLockOwnerFile = original
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			lock, err = acquireInstallLifecycleLock(ctx, home, command)
			if err != nil {
				t.Fatalf("failed retention leaked ownership: %v", err)
			}
			if err := lock.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallerLinkRecoveryRequiresRetainedLock(t *testing.T) {
	target := filepath.Join(t.TempDir(), "command")
	err := recoverInstallerLink(installMetadata{LinkPath: target, BinaryPath: filepath.Join(t.TempDir(), "binary")}, nil)
	if err == nil {
		t.Fatal("link recovery succeeded without retained ownership")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("unowned recovery created command link: %v", err)
	}
}
