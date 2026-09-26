// SPDX-License-Identifier: Apache-2.0

// Package review opens explicit local tools for private stage review content.
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/threadgrid/threadpoint/internal/shellcmd"
)

// Streams supplies the terminal streams for explicitly requested review tools.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

const maxContentBytes = 8 << 20

// Edit opens the configured editor on a private temporary copy and returns the
// edited bytes. Source names are used only to select a helpful file extension.
func Edit(ctx context.Context, streams Streams, source string, initial []byte) ([]byte, error) {
	command := strings.TrimSpace(os.Getenv("VISUAL"))
	if command == "" {
		command = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if command == "" {
		return nil, errors.New("stage edit requires $VISUAL or $EDITOR")
	}
	dir, err := os.MkdirTemp("", "threadpoint-stage-review-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	// #nosec G302 -- this is a private, traversable scratch directory; 0o700 is the required owner-only directory mode.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	path, err := writeScratch(dir, "edit", source, initial)
	if err != nil {
		return nil, err
	}
	if err := run(ctx, streams, command, []string{path}, nil); err != nil {
		return nil, err
	}
	return readScratch(path)
}

// Diff opens the explicitly configured diff command on private source and
// staged copies. LOCAL and REMOTE mirror the positional arguments.
func Diff(ctx context.Context, streams Streams, source string, local, remote []byte) error {
	command := strings.TrimSpace(os.Getenv("DIFF"))
	if command == "" {
		return errors.New("stage difftool requires $DIFF")
	}
	dir, err := os.MkdirTemp("", "threadpoint-stage-review-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// #nosec G302 -- this is a private, traversable scratch directory; 0o700 is the required owner-only directory mode.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	localPath, err := writeScratch(dir, "source", source, local)
	if err != nil {
		return err
	}
	remotePath, err := writeScratch(dir, "staged", source, remote)
	if err != nil {
		return err
	}
	err = run(ctx, streams, command, []string{localPath, remotePath}, map[string]string{"LOCAL": localPath, "REMOTE": remotePath})
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil
	}
	return err
}

// Merge opens the explicitly configured merge command. BASE is the frozen
// source snapshot, LOCAL is the staged review, REMOTE is the current source,
// and MERGED is the private result file returned after a successful command.
func Merge(ctx context.Context, streams Streams, source string, base, local, remote []byte) ([]byte, error) {
	command := strings.TrimSpace(os.Getenv("MERGE"))
	if command == "" {
		return nil, errors.New("stage mergetool requires $MERGE")
	}
	dir, err := os.MkdirTemp("", "threadpoint-stage-review-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	// #nosec G302 -- this is a private, traversable scratch directory; 0o700 is the required owner-only directory mode.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	basePath, err := writeScratch(dir, "base", source, base)
	if err != nil {
		return nil, err
	}
	localPath, err := writeScratch(dir, "local", source, local)
	if err != nil {
		return nil, err
	}
	remotePath, err := writeScratch(dir, "remote", source, remote)
	if err != nil {
		return nil, err
	}
	mergedPath, err := writeScratch(dir, "merged", source, local)
	if err != nil {
		return nil, err
	}
	if err := run(ctx, streams, command, []string{basePath, localPath, remotePath, mergedPath}, map[string]string{
		"BASE": basePath, "LOCAL": localPath, "REMOTE": remotePath, "MERGED": mergedPath,
	}); err != nil {
		return nil, err
	}
	return readScratch(mergedPath)
}

func writeScratch(dir, stem, source string, body []byte) (string, error) {
	path := filepath.Join(dir, stem+extension(source))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func readScratch(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("review tool output is not a regular file")
	}
	if info.Size() > maxContentBytes {
		return nil, fmt.Errorf("review tool output exceeds %d bytes", maxContentBytes)
	}
	// #nosec G304 -- path is validated above as a bounded, regular, non-symlink scratch file.
	return os.ReadFile(path)
}

func extension(source string) string {
	ext := filepath.Ext(filepath.Base(source))
	if len(ext) < 2 || len(ext) > 17 {
		return ".md"
	}
	for _, char := range ext[1:] {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return ".md"
		}
	}
	return ext
}

func run(ctx context.Context, streams Streams, command string, args []string, values map[string]string) error {
	if streams.Stdin == nil || streams.Stdout == nil || streams.Stderr == nil {
		return errors.New("review streams are required")
	}
	cmd, err := shellcmd.CommandContext(ctx, command, args...)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = streams.Stdin, streams.Stdout, streams.Stderr
	if len(values) > 0 {
		cmd.Env = environment(values)
	}
	return cmd.Run()
}

func environment(values map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(values))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if _, override := values[key]; !override {
			result = append(result, value)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
