// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/threadgrid/threadpoint/doctor"
	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/provider"
	"github.com/threadgrid/threadpoint/prune"
	"github.com/threadgrid/threadpoint/restore"
	"github.com/threadgrid/threadpoint/safefs"
)

type initReport struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Root          string `json:"root"`
	Initialized   bool   `json:"initialized"`
}

var applyPrune = prune.Apply

var initAfterProjectRootBorrow func()

func runInit(_ context.Context, app *cli, args []string) (returnErr error) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	format := fs.String("format", app.format, "text or json")
	yes := fs.Bool("yes", false, "confirm layout initialization")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("init does not accept positional arguments")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "init")
	if err != nil {
		return usageError(err)
	}
	root, err := resolveRootFlagValueAllowMissing(fs, *rootFlag)
	if err != nil {
		return err
	}
	if err := confirmMutation(app, *yes, "init", fmt.Sprintf("Create or update the shared agent knowledgebase layout at %q? [y/N]: ", root)); err != nil {
		return err
	}
	threadpointHome, err := app.productHome()
	if err != nil {
		return err
	}
	physicalRoot, err := safefs.ResolveRootForMutation(root)
	if err != nil {
		return err
	}
	if err := safefs.RejectExistingSymlinkAncestors(physicalRoot); err != nil {
		return err
	}
	// #nosec G301 -- the selected project root is user-visible and follows the
	// caller's umask; all bootstrap children are created after the root is pinned.
	if err := os.MkdirAll(physicalRoot, 0o777); err != nil {
		return err
	}
	locks, err := safefs.AcquireLocks(threadpointHome, []string{root}, "init")
	if err != nil {
		return classifyRuntimeError(err)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Release()) }()
	projectRoot, err := locks.BorrowProjectRoot(root)
	if err != nil {
		return err
	}
	if initAfterProjectRootBorrow != nil {
		initAfterProjectRootBorrow()
	}
	validateProjectRoot := func() error { return validateInitProjectRoot(locks, root, projectRoot) }
	if err := layout.EnsureProjectBootstrapInRootValidated(projectRoot, validateProjectRoot); err != nil {
		return err
	}
	if err := validateProjectRoot(); err != nil {
		return err
	}
	if outputFormat == outputFormatJSON {
		return app.printJSON(initReport{
			SchemaVersion: "threadpoint.init.v1",
			OK:            true,
			Command:       "threadpoint init",
			Root:          root,
			Initialized:   true,
		})
	}
	if !app.quiet {
		_, err := fmt.Fprintf(app.stderr, "Initialized shared agent knowledgebase layout at %q.\n", root)
		return err
	}
	return nil
}

func validateInitProjectRoot(locks *safefs.LockSet, selected string, retained *os.Root) error {
	if locks == nil || retained == nil {
		return errors.New("init lock and retained project root are required")
	}
	current, err := locks.BorrowProjectRoot(selected)
	if err != nil {
		return err
	}
	currentInfo, currentErr := current.Stat(".")
	retainedInfo, retainedErr := retained.Stat(".")
	if currentErr != nil || retainedErr != nil || !os.SameFile(currentInfo, retainedInfo) {
		return errors.Join(errors.New("selected project root no longer identifies the locked generation"), currentErr, retainedErr)
	}
	return nil
}

func runPrune(ctx context.Context, app *cli, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	root := projectRootFlag(fs)
	planOnly := fs.Bool("plan", true, "print a prune plan")
	apply := fs.Bool("apply", false, "remove verified native artifacts")
	yes := fs.Bool("yes", false, "confirm scripted prune apply")
	format := fs.String("format", app.format, "text or json")
	backupNamespace := backupNamespaceFlag(fs)
	confirm := fs.String("confirm", "", "confirmation phrase")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("prune does not accept positional arguments")
	}
	if flagProvided(fs, "plan") && *planOnly && *apply {
		return usageErrorf("prune accepts only one of --plan or --apply")
	}
	if !*planOnly && !*apply {
		return usageErrorf("prune requires --plan or --apply")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "prune")
	if err != nil {
		return usageError(err)
	}
	resolvedRoot, err := resolveRootFlagValue(fs, *root)
	if err != nil {
		return err
	}
	threadpointHome, err := app.productHome()
	if err != nil {
		return err
	}
	opts := prune.Options{
		Root:            resolvedRoot,
		ThreadpointHome: threadpointHome,
		PlanOnly:        *planOnly,
		Apply:           *apply,
		Yes:             *yes,
		BackupDir:       *backupNamespace,
		Confirm:         *confirm,
	}
	if *apply {
		report, applyErr := applyPrune(ctx, opts)
		var outputErr error
		if report != nil {
			if outputFormat == outputFormatText {
				_, outputErr = fmt.Fprint(app.stdout, prune.FormatReportText(report))
			} else {
				outputErr = app.printJSON(report)
			}
		}
		applyErr = classifyRuntimeError(applyErr)
		if outputErr != nil {
			return errors.Join(applyErr, outputErr)
		}
		return applyErr
	}
	plan, err := prune.BuildPlan(ctx, opts)
	if err != nil {
		return classifyRuntimeError(err)
	}
	if outputFormat == outputFormatText {
		fmt.Fprint(app.stdout, prune.FormatPlanText(plan))
		return nil
	}
	return app.printJSON(plan)
}

func runRestore(ctx context.Context, app *cli, args []string) error {
	if len(args) > 0 && args[0] == "list" {
		return runRestoreList(app, args[1:])
	}
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	root := projectRootFlag(fs)
	backupID := fs.String("backup", "", "backup run id")
	latest := fs.Bool("latest", false, "restore from latest backup run")
	planOnly := fs.Bool("plan", false, "print a restore plan")
	apply := fs.Bool("apply", false, "apply clean restore candidates")
	yes := fs.Bool("yes", false, "confirm scripted restore apply")
	format := fs.String("format", app.format, "text or json")
	backupNamespace := backupNamespaceFlag(fs)
	confirm := fs.String("confirm", "", "confirmation phrase")
	if err := fs.Parse(args); err != nil {
		return usageErrorf("%v; to list backups use threadpoint restore list --root PATH", err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("restore does not accept positional arguments; to list backups use threadpoint restore list --root PATH")
	}
	if *planOnly && *apply {
		return usageErrorf("restore accepts only one of --plan or --apply")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "restore")
	if err != nil {
		return usageError(err)
	}
	resolvedRoot, err := resolveRootFlagValue(fs, *root)
	if err != nil {
		return err
	}
	threadpointHome, err := app.productHome()
	if err != nil {
		return err
	}
	opts := restore.Options{
		Root:            resolvedRoot,
		ThreadpointHome: threadpointHome,
		OutputRedaction: app.outputRedaction(resolvedRoot, threadpointHome),
		BackupID:        *backupID,
		Latest:          *latest,
		PlanOnly:        *planOnly,
		Apply:           *apply,
		Yes:             *yes,
		BackupDir:       *backupNamespace,
		Confirm:         *confirm,
	}
	if *planOnly {
		plan, err := restore.BuildPlan(ctx, opts)
		if err != nil {
			return classifyRuntimeError(err)
		}
		if outputFormat == outputFormatText {
			app.printRedactedText(restore.FormatPlanText(plan), opts.OutputRedaction)
			return nil
		}
		return app.printRedactedJSON(plan, opts.OutputRedaction)
	}
	if *apply {
		applyRestore := app.restoreApply
		if applyRestore == nil {
			applyRestore = restore.Apply
		}
		report, applyErr := applyRestore(ctx, opts)
		outputErr := writeRestorationReport(app, outputFormat, report)
		applyErr = classifyRuntimeError(applyErr)
		if outputErr != nil {
			return errors.Join(applyErr, outputErr)
		}
		return applyErr
	}
	if app.nonInteractive || !interactiveInput(app.stdin) {
		return refusedError(errors.New("restore interactive mode requires an interactive terminal; use restore list, restore --plan, or restore --apply --yes for noninteractive runs"))
	}
	runRestore := app.restoreRun
	if runRestore == nil {
		runRestore = restore.RunInteractive
	}
	report, applyErr := runRestore(ctx, opts, app.stdin, app.stdout, app.stderr)
	outputErr := writeRestorationReport(app, outputFormat, report)
	applyErr = classifyRuntimeError(applyErr)
	if outputErr != nil {
		return errors.Join(applyErr, outputErr)
	}
	return applyErr
}

func runRestoreList(app *cli, args []string) error {
	fs := flag.NewFlagSet("restore list", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	root := projectRootFlag(fs)
	format := fs.String("format", app.format, "text or json")
	backupNamespace := backupNamespaceFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("restore list does not accept positional arguments")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "restore list")
	if err != nil {
		return usageError(err)
	}
	resolvedRoot, err := resolveRootFlagValue(fs, *root)
	if err != nil {
		return err
	}
	home, err := app.productHome()
	if err != nil {
		return err
	}
	runs, err := restore.ListRuns(restore.Options{Root: resolvedRoot, ThreadpointHome: home, BackupDir: *backupNamespace})
	if err != nil {
		return classifyRuntimeError(err)
	}
	if outputFormat == outputFormatText {
		_, err := fmt.Fprint(app.stdout, restore.FormatRunsText(runs))
		return err
	}
	return app.printRedactedJSON(runs, app.outputRedaction(resolvedRoot, home))
}

func writeRestorationReport(app *cli, outputFormat string, report *restore.Report) error {
	if report == nil {
		return nil
	}
	if outputFormat == outputFormatText {
		_, err := fmt.Fprint(app.stdout, restore.FormatReportText(report))
		return err
	}
	return app.printJSON(report)
}

func runStatus(ctx context.Context, app *cli, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	root := projectRootFlag(fs)
	format := fs.String("format", app.format, "text or json")
	providersRaw := fs.String("providers", "", "comma-separated providers to include")
	excludeProvidersRaw := fs.String("exclude-providers", "", "comma-separated providers to exclude")
	backupNamespace := backupNamespaceFlag(fs)
	var skipDirNames skipDirNamesFlag
	fs.Var(&skipDirNames, "skip-dirs", "comma-separated directory basenames to skip")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("status does not accept positional arguments")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "status")
	if err != nil {
		return usageError(err)
	}
	providers, err := parseProviders(*providersRaw)
	if err != nil {
		return usageError(err)
	}
	excludeProviders, err := parseProviders(*excludeProvidersRaw)
	if err != nil {
		return usageError(err)
	}
	resolvedRoot, err := resolveRootFlagValue(fs, *root)
	if err != nil {
		return err
	}
	threadpointHome, err := app.productHome()
	if err != nil {
		return err
	}
	normalizedSkipDirNames, err := parseSkipDirNames(skipDirNames)
	if err != nil {
		return usageError(err)
	}
	report, err := doctor.Run(ctx, doctor.Options{
		Root:                      resolvedRoot,
		SkipDirNames:              normalizedSkipDirNames,
		ThreadpointHome:           threadpointHome,
		Providers:                 providers,
		ExcludeProviders:          excludeProviders,
		ProviderSelectionExplicit: strings.TrimSpace(*providersRaw) != "",
		BackupDir:                 *backupNamespace,
	})
	if err != nil {
		return classifyRuntimeError(err)
	}
	switch outputFormat {
	case outputFormatText:
		app.printRedactedText(doctor.FormatText(report), app.outputRedaction(resolvedRoot, threadpointHome))
	case outputFormatJSON:
		if err := app.printJSON(report); err != nil {
			return err
		}
	}
	if !report.OK {
		return validationError(errors.New("threadpoint status found errors"))
	}
	return nil
}

// toolDoctorReport is the threadpoint doctor (tool self-check) result. Unlike
// status, doctor inspects the threadpoint tool itself — build/install identity,
// home resolution, and update availability — and never touches a project.
type toolDoctorReport struct {
	SchemaVersion     string              `json:"schema_version"`
	Command           string              `json:"command"`
	OK                bool                `json:"ok"`
	Findings          []toolDoctorFinding `json:"findings"`
	SuggestedCommands []string            `json:"suggested_commands,omitempty"`
}

type toolDoctorFinding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

func runDoctor(ctx context.Context, app *cli, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	offline := fs.Bool("offline", false, "skip network checks")
	format := fs.String("format", app.format, "text or json")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("doctor does not accept positional arguments")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "doctor")
	if err != nil {
		return usageError(err)
	}
	productHome, err := app.productHome()
	if err != nil {
		return err
	}
	report := buildToolDoctorReport(ctx, app, productHome, *offline)
	switch outputFormat {
	case outputFormatText:
		app.printRedactedText(formatToolDoctorText(report), app.outputRedaction("", productHome))
	case outputFormatJSON:
		if err := app.printJSON(report); err != nil {
			return err
		}
	}
	if !report.OK {
		return validationError(errors.New("threadpoint doctor found errors"))
	}
	return nil
}

func buildToolDoctorReport(ctx context.Context, app *cli, productHome string, offline bool) toolDoctorReport {
	report := toolDoctorReport{
		SchemaVersion: "threadpoint.doctor.v1",
		Command:       "threadpoint doctor",
		OK:            true,
	}
	add := func(severity, code, message string) {
		report.Findings = append(report.Findings, toolDoctorFinding{Severity: severity, Code: code, Message: message})
		if severity == "error" {
			report.OK = false
		}
	}

	versionInfo := buildVersionReport(productHome)
	add("info", "version", fmt.Sprintf("threadpoint %s (commit %s), install source %s", versionInfo.Version, versionInfo.Commit, versionInfo.InstallSource))
	if versionInfo.MetadataPath != "" {
		add("info", "install-ownership", "installer-managed binary ownership verified")
	}
	add("info", "home", fmt.Sprintf("threadpoint home resolves to %s", productHome))
	if state, err := readUpdateReminderStateWithHome(productHome); err == nil {
		add("info", "update-reminders", fmt.Sprintf("automatic update reminders enabled: %v", state.Enabled))
	}

	if offline {
		add("info", "update-availability", "skipped (offline)")
		return report
	}
	check := runUpdateCheckReportWithOptions(ctx, app.release, defaultUpdateRepo, updateCheckOptions{
		Channel:         updateChannelStable,
		ThreadpointHome: productHome,
		UseCache:        true,
		MaxAge:          defaultExplicitUpdateCacheMaxAge,
	})
	switch {
	case check.UpdateAvailable && check.Critical:
		add("error", "update-critical", fmt.Sprintf("a critical threadpoint update is available: %s", check.LatestVersion))
		report.SuggestedCommands = append(report.SuggestedCommands, "threadpoint update")
	case check.UpdateAvailable:
		add("warning", "update-available", fmt.Sprintf("a newer threadpoint release is available: %s", check.LatestVersion))
		report.SuggestedCommands = append(report.SuggestedCommands, "threadpoint update")
	case check.LatestVersion == "":
		reason := check.Reason
		if reason == "" {
			reason = "could not determine latest release metadata"
		}
		add("warning", "update-availability", fmt.Sprintf("could not determine update availability: %s", reason))
	default:
		add("info", "update-availability", "threadpoint is up to date")
	}
	return report
}

func formatToolDoctorText(report toolDoctorReport) string {
	var out strings.Builder
	if report.OK {
		fmt.Fprintln(&out, "Threadpoint doctor: OK")
	} else {
		fmt.Fprintln(&out, "Threadpoint doctor: errors found")
	}
	for _, finding := range report.Findings {
		fmt.Fprintf(&out, "  [%s] %s: %s\n", finding.Severity, finding.Code, finding.Message)
	}
	if len(report.SuggestedCommands) == 0 {
		fmt.Fprintln(&out, "Suggested next commands: none")
	} else {
		fmt.Fprintln(&out, "Suggested next commands:")
		for _, command := range report.SuggestedCommands {
			fmt.Fprintf(&out, "  %s\n", command)
		}
	}
	return out.String()
}

func parseProviders(raw string) ([]provider.ID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var providers []provider.ID
	for _, part := range strings.Split(raw, ",") {
		providerID, ok := provider.Parse(part)
		if !ok {
			return nil, fmt.Errorf("unknown provider %q", part)
		}
		providers = append(providers, providerID)
	}
	return providers, nil
}
