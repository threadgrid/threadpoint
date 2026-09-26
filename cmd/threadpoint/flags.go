// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/threadgrid/threadpoint/project"
	"github.com/threadgrid/threadpoint/walk"
)

const (
	outputFormatText = "text"
	outputFormatJSON = "json"
	rootFlagUsage    = "project root override (optional; otherwise discover from current directory)"
)

func projectRootFlag(fs *flag.FlagSet) *string {
	return fs.String("root", "", rootFlagUsage)
}

func interactiveInput(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func flagProvided(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			found = true
		}
	})
	return found
}

func backupNamespaceFlag(fs *flag.FlagSet) *string {
	return fs.String("backup-namespace", "", "backup namespace under .threadpoint/backups")
}

type skipDirNamesFlag []string

func (values *skipDirNamesFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *skipDirNamesFlag) Set(value string) error {
	*values = append(*values, strings.Split(value, ",")...)
	return nil
}

func parseSkipDirNames(values skipDirNamesFlag) ([]string, error) {
	return walk.NormalizeSkipDirNames(values)
}

func normalizeOutputFormat(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", outputFormatText:
		return outputFormatText, nil
	case outputFormatJSON:
		return outputFormatJSON, nil
	default:
		return "", fmt.Errorf("unknown output format %q", raw)
	}
}

func resolveCommandOutputFormat(app *cli, fs *flag.FlagSet, raw string, commandName string) (string, error) {
	if flagProvided(fs, "format") {
		format, err := normalizeOutputFormat(raw)
		if err != nil {
			return "", fmt.Errorf("unknown %s format %q", commandName, raw)
		}
		if app.formatExplicit && format != app.format {
			return "", fmt.Errorf("conflicting --format values: global %q, command %q", app.format, format)
		}
		return format, nil
	}
	if app.formatExplicit {
		return app.format, nil
	}
	return outputFormatText, nil
}

func confirmMutation(app *cli, yes bool, commandName string, prompt string) error {
	if yes {
		return nil
	}
	if app.nonInteractive || !interactiveInput(app.stdin) {
		return refusedError(fmt.Errorf("%s requires explicit confirmation; rerun with --yes for noninteractive use", commandName))
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = fmt.Sprintf("Proceed with %s? [y/N]: ", commandName)
	}
	fmt.Fprint(app.stderr, prompt)
	line, err := bufio.NewReader(app.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return refusedError(fmt.Errorf("read %s confirmation: %w", commandName, err))
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return refusedError(fmt.Errorf("%s confirmation declined", commandName))
	}
}

func resolveRootFlagValue(fs *flag.FlagSet, root string) (string, error) {
	return resolveRootFlagValueWithOptions(fs, root, false)
}

func resolveRootFlagValueAllowMissing(fs *flag.FlagSet, root string) (string, error) {
	return resolveRootFlagValueWithOptions(fs, root, true)
}

func resolveRootFlagValueWithOptions(fs *flag.FlagSet, root string, allowMissing bool) (string, error) {
	if flagProvided(fs, "root") && strings.TrimSpace(root) == "" {
		return "", usageErrorf("--root cannot be empty")
	}
	selection, err := project.Discover(project.Options{
		Root:         root,
		Explicit:     flagProvided(fs, "root"),
		AllowMissing: allowMissing,
	})
	if err != nil {
		return "", usageError(err)
	}
	return selection.ProjectRoot, nil
}
