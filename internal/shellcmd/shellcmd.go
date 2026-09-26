// SPDX-License-Identifier: Apache-2.0

// Package shellcmd runs explicit user-configured commands through the platform
// shell so quoted paths and arguments behave like normal CLI configuration.
package shellcmd

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// CommandContext returns an exec.Cmd for command plus positional args.
func CommandContext(ctx context.Context, command string, args ...string) (*exec.Cmd, error) {
	script := strings.TrimSpace(command)
	if script == "" {
		return nil, errors.New("command must not be empty")
	}
	if len(args) > 4 {
		return nil, errors.New("command accepts at most four positional arguments")
	}
	return commandContextForOS(ctx, runtime.GOOS, script, args...)
}

func commandContextForOS(ctx context.Context, goos, script string, args ...string) (*exec.Cmd, error) {
	if goos == "windows" {
		for _, arg := range args {
			script += " " + quoteForOS(goos, arg)
		}
		// #nosec G204 -- script is an explicit user-configured command.
		return exec.CommandContext(ctx, "cmd", "/C", script), nil
	}
	if len(args) > 0 {
		// Parse the explicit command first, then invoke the parsed program with
		// its positional arguments. Saving the arguments before eval preserves
		// quoted paths and keeps a trailing shell comment from swallowing them.
		arguments := append([]string{"threadpoint-shellcmd", script, strconv.Itoa(len(args))}, args...)
		shellArgs := append([]string{"-c", `command=$1; count=$2; first=$3; second=$4; third=$5; fourth=$6; eval "set -- $command"; case "$count" in 1) exec "$@" "$first" ;; 2) exec "$@" "$first" "$second" ;; 3) exec "$@" "$first" "$second" "$third" ;; 4) exec "$@" "$first" "$second" "$third" "$fourth" ;; esac`, "threadpoint-shellcmd"}, arguments[1:]...)
		// #nosec G204 G702 -- script is an explicit user-configured command.
		return exec.CommandContext(ctx, "sh", shellArgs...), nil
	}
	// #nosec G204 -- script is an explicit user-configured command.
	return exec.CommandContext(ctx, "sh", "-c", script), nil
}

func quote(value string) string { return quoteForOS(runtime.GOOS, value) }

func quoteForOS(goos, value string) string {
	if goos == "windows" {
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
