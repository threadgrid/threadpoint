// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const updateReminderForceAge = 30 * 24 * time.Hour

type updateReminderState struct {
	Enabled              bool      `json:"enabled"`
	DismissedVersion     string    `json:"dismissed_version,omitempty"`
	LastAutomaticCheckAt time.Time `json:"last_automatic_check_at,omitempty"`
}

type updateReminderMutationReport struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Operation     string `json:"operation"`
	Version       string `json:"version,omitempty"`
	Enabled       bool   `json:"enabled"`
}

func defaultUpdateReminderState() updateReminderState {
	return updateReminderState{Enabled: false}
}

func runUpdateDismiss(app *cli, args []string) error {
	fs := flag.NewFlagSet("update reminder dismiss", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	version := fs.String("version", "", "release version to dismiss")
	format := fs.String("format", app.format, "text or json")
	yes := fs.Bool("yes", false, "confirm reminder dismissal")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "update")
	if err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("update reminder dismiss does not accept positional arguments")
	}
	cleanVersion := strings.TrimSpace(*version)
	if cleanVersion == "" {
		return usageErrorf("update reminder dismiss requires --version")
	}
	if err := confirmMutation(app, *yes, "update reminder dismiss", "Dismiss this update reminder in local threadpoint state? [y/N]: "); err != nil {
		return err
	}
	productHome, err := app.productHome()
	if err != nil {
		return err
	}
	state, err := mergeUpdateReminderStateWithHome(productHome, updateReminderMutation{DismissedVersion: stringPointer(cleanVersion)})
	if err != nil {
		return err
	}
	if outputFormat == outputFormatJSON {
		return app.printJSON(updateReminderMutationReport{
			SchemaVersion: "threadpoint.update_reminder.v1",
			OK:            true,
			Command:       "threadpoint update reminder dismiss",
			Operation:     "dismiss",
			Version:       cleanVersion,
			Enabled:       state.Enabled,
		})
	}
	fmt.Fprintf(app.stdout, "dismissed update reminder for %s\n", cleanVersion)
	return nil
}

func runUpdateReminders(app *cli, args []string) error {
	if len(args) == 0 {
		return usageErrorf("usage: threadpoint update reminder <status|enable|disable|dismiss>")
	}
	switch args[0] {
	case "dismiss":
		return runUpdateDismiss(app, args[1:])
	case "status":
		fs := flag.NewFlagSet("update reminder status", flag.ContinueOnError)
		fs.SetOutput(app.stderr)
		format := fs.String("format", app.format, "text or json")
		if err := fs.Parse(args[1:]); err != nil {
			return usageError(err)
		}
		outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "update")
		if err != nil {
			return usageError(err)
		}
		if fs.NArg() > 0 {
			return usageErrorf("update reminder status does not accept positional arguments")
		}
		productHome, err := app.productHome()
		if err != nil {
			return err
		}
		state, _ := readUpdateReminderStateWithHome(productHome)
		if outputFormat == outputFormatText {
			printUpdateReminderStateText(app.stdout, state)
			return nil
		}
		return app.printJSON(state)
	case "enable":
		outputFormat, yes, err := parseUpdateReminderMutationArgs(app, "update reminder enable", args[1:])
		if err != nil {
			return err
		}
		if err := confirmMutation(app, yes, "update reminder enable", "Enable automatic update reminders in local threadpoint state? [y/N]: "); err != nil {
			return err
		}
		productHome, err := app.productHome()
		if err != nil {
			return err
		}
		state, err := mergeUpdateReminderStateWithHome(productHome, updateReminderMutation{Enabled: boolPointer(true)})
		if err != nil {
			return err
		}
		if outputFormat == outputFormatJSON {
			return app.printJSON(updateReminderMutationReport{
				SchemaVersion: "threadpoint.update_reminder.v1",
				OK:            true,
				Command:       "threadpoint update reminder enable",
				Operation:     "enable",
				Enabled:       state.Enabled,
			})
		}
		fmt.Fprintln(app.stdout, "update reminders enabled")
		return nil
	case "disable":
		outputFormat, yes, err := parseUpdateReminderMutationArgs(app, "update reminder disable", args[1:])
		if err != nil {
			return err
		}
		if err := confirmMutation(app, yes, "update reminder disable", "Disable automatic update reminders in local threadpoint state? [y/N]: "); err != nil {
			return err
		}
		productHome, err := app.productHome()
		if err != nil {
			return err
		}
		state, err := mergeUpdateReminderStateWithHome(productHome, updateReminderMutation{Enabled: boolPointer(false)})
		if err != nil {
			return err
		}
		if outputFormat == outputFormatJSON {
			return app.printJSON(updateReminderMutationReport{
				SchemaVersion: "threadpoint.update_reminder.v1",
				OK:            true,
				Command:       "threadpoint update reminder disable",
				Operation:     "disable",
				Enabled:       state.Enabled,
			})
		}
		fmt.Fprintln(app.stdout, "update reminders disabled")
		return nil
	default:
		return usageErrorf("unknown update reminder subcommand %q", args[0])
	}
}

func parseUpdateReminderMutationArgs(app *cli, name string, args []string) (string, bool, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	format := fs.String("format", app.format, "text or json")
	yes := fs.Bool("yes", false, "confirm reminder preference change")
	if err := fs.Parse(args); err != nil {
		return "", false, usageError(err)
	}
	if fs.NArg() > 0 {
		return "", false, usageErrorf("%s does not accept positional arguments", name)
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "update")
	if err != nil {
		return "", false, usageError(err)
	}
	return outputFormat, *yes, nil
}

func printUpdateReminderStateText(writer io.Writer, state updateReminderState) {
	fmt.Fprintf(writer, "update reminders enabled: %v\n", state.Enabled)
	if state.DismissedVersion != "" {
		fmt.Fprintf(writer, "dismissed version: %s\n", state.DismissedVersion)
	}
	if !state.LastAutomaticCheckAt.IsZero() {
		fmt.Fprintf(writer, "last automatic check: %s\n", state.LastAutomaticCheckAt.UTC().Format(time.RFC3339))
	}
}

func maybePrintAutomaticUpdateReminder(ctx context.Context, app *cli, args []string) {
	if !shouldRunAutomaticUpdateReminder(app, args) {
		return
	}
	productHome, err := app.productHome()
	if err != nil {
		return
	}
	now := time.Now().UTC()
	state, claimed, err := claimAutomaticUpdateReminderCheck(productHome, now)
	if err != nil || !claimed {
		return
	}

	report := runAutomaticUpdateReminderCheck(ctx, app.release, productHome)
	if !report.UpdateAvailable || report.Banner == "" {
		return
	}
	forced := updateReminderForced(report, now)
	if !forced && state.DismissedVersion == report.LatestVersion {
		return
	}
	report.ForcedReminder = forced
	report.Dismissed = false
	fmt.Fprintf(app.stderr, "\n%s\n", automaticUpdateReminderText(report, forced))
}

func runAutomaticUpdateReminderCheck(ctx context.Context, client releaseClient, productHome string) updateCheckReport {
	return runUpdateCheckReportWithOptions(ctx, client, defaultUpdateRepo, updateCheckOptions{
		CacheOnly:       true,
		Channel:         updateChannelStable,
		ThreadpointHome: productHome,
		UseCache:        true,
		MaxAge:          defaultAutomaticReminderCacheMaxAge,
	})
}

func shouldRunAutomaticUpdateReminder(app *cli, args []string) bool {
	if len(args) == 0 {
		return false
	}
	if app.quiet || app.nonInteractive {
		return false
	}
	if app.formatExplicit && app.format == outputFormatJSON {
		return false
	}
	switch args[0] {
	case "update":
		return false
	default:
		return interactiveOutput(app.stderr)
	}
}

func interactiveOutput(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func automaticUpdateReminderText(report updateCheckReport, forced bool) string {
	lines := []string{report.Banner}
	if forced {
		if report.Critical {
			lines = append(lines, "This reminder cannot be dismissed because the update is marked critical.")
		} else {
			lines = append(lines, "This reminder can no longer be dismissed because this install is at least 30 days and more than one stable release behind.")
		}
	} else if report.LatestVersion != "" {
		lines = append(lines, fmt.Sprintf("Run `threadpoint update reminder dismiss --version %s` to hide this reminder for this version.", report.LatestVersion))
	}
	return strings.Join(lines, "\n")
}

func updateReminderForced(report updateCheckReport, now time.Time) bool {
	if report.Critical {
		return true
	}
	if report.NewerStableReleaseCount <= 1 {
		return false
	}
	published, err := time.Parse(time.RFC3339, report.LatestPublishedAt)
	if err != nil {
		return false
	}
	return now.Sub(published) >= updateReminderForceAge
}

func updateReminderStatePathWithHome(productHome string) (string, bool) {
	threadpointHome, err := resolveThreadpointHomeForPath(productHome)
	if err != nil {
		return "", false
	}
	return filepath.Join(threadpointHome, "updates", "reminders.json"), true
}

func readUpdateReminderState() (updateReminderState, error) {
	return readUpdateReminderStateWithHome("")
}

func readUpdateReminderStateWithHome(productHome string) (updateReminderState, error) {
	return readUpdateReminderStateLocked(productHome)
}

func writeUpdateReminderStateWithHome(productHome string, state updateReminderState) error {
	return writeUpdateReminderStateLocked(productHome, state)
}
