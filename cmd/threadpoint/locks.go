// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/threadgrid/threadpoint/safefs"
)

type locksClearReport struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Cleared       bool   `json:"cleared"`
	Stale         bool   `json:"stale"`
	Forced        bool   `json:"forced"`
}

func runLocks(_ context.Context, app *cli, args []string) error {
	if len(args) == 0 {
		return usageErrorf("lock subcommand is required")
	}
	switch args[0] {
	case "clear":
		return runLocksClear(app, args[1:])
	default:
		return usageErrorf("unknown lock subcommand %q", args[0])
	}
}

func runLocksClear(app *cli, args []string) error {
	fs := flag.NewFlagSet("lock clear", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	force := fs.Bool("force", false, "remove a fresh lock after explicit confirmation")
	yes := fs.Bool("yes", false, "confirm forced lock cleanup")
	format := fs.String("format", app.format, "text or json")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("lock clear does not accept positional arguments")
	}
	if *yes && !*force {
		return usageErrorf("lock clear --yes requires --force")
	}
	if *force && !*yes {
		return refusedError(fmt.Errorf("lock clear --force requires --yes"))
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "lock clear")
	if err != nil {
		return usageError(err)
	}
	root, err := resolveRootFlagValue(fs, *rootFlag)
	if err != nil {
		return err
	}
	home, err := app.productHome()
	if err != nil {
		return err
	}
	result, err := safefs.ClearProjectLock(home, root, *force)
	if err != nil {
		return classifyRuntimeError(err)
	}
	report := locksClearReport{
		SchemaVersion: "threadpoint.locks_clear.v1",
		OK:            true,
		Command:       "threadpoint lock clear",
		Cleared:       result.Cleared,
		Stale:         result.Stale,
		Forced:        result.Forced,
	}
	if outputFormat == outputFormatJSON {
		return app.printJSON(report)
	}
	if report.Cleared {
		if report.Forced {
			fmt.Fprintln(app.stdout, "Cleared a force-confirmed threadpoint project lock.")
		} else {
			fmt.Fprintln(app.stdout, "Cleared a stale threadpoint project lock.")
		}
		return nil
	}
	fmt.Fprintln(app.stdout, "No threadpoint project lock was present.")
	return nil
}
