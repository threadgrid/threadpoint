// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/threadgrid/threadpoint/discover"
	"github.com/threadgrid/threadpoint/layout"
	"github.com/threadgrid/threadpoint/provider"
	"github.com/threadgrid/threadpoint/review"
	"github.com/threadgrid/threadpoint/safefs"
	"github.com/threadgrid/threadpoint/stage"
)

type classificationFlag []string

var stageCommandAfterProjectRootBorrow func()

func (values *classificationFlag) String() string { return strings.Join(*values, ",") }

func (values *classificationFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runStage(ctx context.Context, app *cli, args []string) (returnErr error) {
	if len(args) > 0 {
		switch args[0] {
		case "list":
			return runStageList(app, args[1:])
		case "diff":
			return runStageDiff(app, args[1:])
		case "edit":
			return runStageEdit(ctx, app, args[1:])
		case "difftool":
			return runStageDifftool(ctx, app, args[1:])
		case "mergetool":
			return runStageMergetool(ctx, app, args[1:])
		case "discard":
			return runStageDiscard(app, args[1:])
		}
	}

	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	planOnly := fs.Bool("plan", false, "preview stages")
	apply := fs.Bool("apply", false, "create review copies")
	yes := fs.Bool("yes", false, "confirm stage creation")
	format := fs.String("format", app.format, "text or json")
	providersRaw := fs.String("providers", "", "comma-separated providers to include")
	excludeProvidersRaw := fs.String("exclude-providers", "", "comma-separated providers to exclude")
	var classifications classificationFlag
	var skipDirNames skipDirNamesFlag
	fs.Var(&classifications, "classify", "source=project-shared or source=project-local")
	fs.Var(&skipDirNames, "skip-dirs", "comma-separated directory basenames to skip")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("stage does not accept positional arguments")
	}
	if *planOnly == *apply {
		return usageErrorf("stage requires exactly one of --plan or --apply")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "stage")
	if err != nil {
		return usageError(err)
	}
	root, err := resolveRootFlagValue(fs, *rootFlag)
	if err != nil {
		return err
	}
	providers, err := parseProviders(*providersRaw)
	if err != nil {
		return usageError(err)
	}
	excluded, err := parseProviders(*excludeProvidersRaw)
	if err != nil {
		return usageError(err)
	}
	classify, err := parseClassifications(classifications)
	if err != nil {
		return usageError(err)
	}
	normalizedSkipDirNames, err := parseSkipDirNames(skipDirNames)
	if err != nil {
		return usageError(err)
	}
	home, err := app.productHome()
	if err != nil {
		return err
	}
	inputs, err := discoverStageInputs(ctx, root, providers, excluded, strings.TrimSpace(*providersRaw) != "", normalizedSkipDirNames)
	if err != nil {
		return classifyStageError(err)
	}
	opts := stage.Options{Root: root, ThreadpointHome: home, Inputs: inputs, Classify: classify}
	if *planOnly {
		report, err := stage.Preview(ctx, opts)
		if err != nil {
			return classifyStageError(err)
		}
		return writeStageReport(app, outputFormat, "threadpoint stage --plan", report)
	}
	if err := confirmMutation(app, *yes, "stage", "Create private review copies? [y/N]: "); err != nil {
		return err
	}
	locks, err := safefs.AcquireLocks(home, []string{root}, "stage")
	if err != nil {
		return classifyRuntimeError(err)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Release()) }()
	productRoot, projectRoot, err := borrowStageCommandRoots(locks, root)
	if err != nil {
		return classifyRuntimeError(err)
	}
	report, err := stage.CreateFromRoots(ctx, opts, productRoot, projectRoot)
	if err != nil {
		return classifyStageError(err)
	}
	return writeStageReport(app, outputFormat, "threadpoint stage", report)
}

func runStageList(app *cli, args []string) error {
	fs := flag.NewFlagSet("stage list", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	format := fs.String("format", app.format, "text or json")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		return usageErrorf("stage list does not accept positional arguments")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "stage list")
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
	records, err := stage.List(root, home)
	if err != nil {
		return classifyStageError(err)
	}
	report := &stage.Report{SchemaVersion: "threadpoint.stage-list.v1", Root: root, Stages: records}
	return writeStageReport(app, outputFormat, "threadpoint stage list", report)
}

func runStageDiff(app *cli, args []string) error {
	id, args := leadingStageID(args)
	fs := flag.NewFlagSet("stage diff", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" || fs.NArg() > 0 {
		return usageErrorf("stage diff requires exactly one stage ID")
	}
	root, err := resolveRootFlagValue(fs, *rootFlag)
	if err != nil {
		return err
	}
	home, err := app.productHome()
	if err != nil {
		return err
	}
	diff, err := stage.Diff(root, home, id)
	if err != nil {
		return classifyStageError(err)
	}
	fmt.Fprintln(app.stderr, "threadpoint: stage diff prints raw local artifact content; review it before sharing.")
	fmt.Fprint(app.stdout, diff)
	return nil
}

func runStageEdit(ctx context.Context, app *cli, args []string) (returnErr error) {
	id, args := leadingStageID(args)
	fs := flag.NewFlagSet("stage edit", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" || fs.NArg() > 0 {
		return usageErrorf("stage edit requires exactly one stage ID")
	}
	if app.nonInteractive || !interactiveInput(app.stdin) {
		return refusedError(errors.New("stage edit requires an interactive terminal"))
	}
	root, err := resolveRootFlagValue(fs, *rootFlag)
	if err != nil {
		return err
	}
	home, err := app.productHome()
	if err != nil {
		return err
	}
	records, err := stage.List(root, home)
	if err != nil {
		return classifyStageError(err)
	}
	var target *stage.Record
	for index := range records {
		if records[index].ID == id {
			target = &records[index]
			break
		}
	}
	if target == nil {
		return refusedError(fmt.Errorf("staged item %s was not found", fs.Arg(0)))
	}
	// ReplaceContent verifies the durable stage identity. The initial bytes are
	// read through the stage diff API's own regular-file checks before opening.
	_, err = stage.Diff(root, home, target.ID)
	if err != nil {
		return classifyStageError(err)
	}
	// The editor is intentionally explicit: $VISUAL, then $EDITOR; it does not
	// select an editor when neither variable is configured.
	initial, err := stage.ReadContent(root, home, target.ID)
	if err != nil {
		return classifyStageError(err)
	}
	edited, err := review.Edit(ctx, review.Streams{Stdin: app.stdin, Stdout: app.stdout, Stderr: app.stderr}, target.Source, initial)
	if err != nil {
		return refusedError(err)
	}
	// Publish under the project lock. The interactive editor deliberately runs
	// outside the lock (a lock held across an editor session risks stale-lock
	// reclamation), so refuse the write if a concurrent commit, discard, or edit
	// changed the stage after the editor began.
	locks, err := safefs.AcquireLocks(home, []string{root}, "stage edit")
	if err != nil {
		return classifyRuntimeError(err)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Release()) }()
	productRoot, projectRoot, err := borrowStageCommandRoots(locks, root)
	if err != nil {
		return classifyRuntimeError(err)
	}
	current, err := stage.ReadContentFromRoot(root, home, target.ID, productRoot)
	if err != nil {
		return classifyStageError(err)
	}
	if !bytes.Equal(current, initial) {
		return refusedError(errors.New("stage changed since it was opened for editing; re-run stage edit"))
	}
	if err := stage.ReplaceContentFromRoots(root, home, target.ID, edited, productRoot, projectRoot); err != nil {
		return classifyStageError(err)
	}
	return nil
}

func runStageDifftool(ctx context.Context, app *cli, args []string) error {
	id, args := leadingStageID(args)
	fs := flag.NewFlagSet("stage difftool", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" || fs.NArg() > 0 {
		return usageErrorf("stage difftool requires exactly one stage ID")
	}
	if app.nonInteractive || !interactiveInput(app.stdin) {
		return refusedError(errors.New("stage difftool requires an interactive terminal"))
	}
	root, err := resolveRootFlagValue(fs, *rootFlag)
	if err != nil {
		return err
	}
	home, err := app.productHome()
	if err != nil {
		return err
	}
	record, source, content, err := stage.ReadReview(root, home, id)
	if err != nil {
		return classifyStageError(err)
	}
	if err := review.Diff(ctx, review.Streams{Stdin: app.stdin, Stdout: app.stdout, Stderr: app.stderr}, record.Source, source, content); err != nil {
		return refusedError(err)
	}
	return nil
}

func runStageMergetool(ctx context.Context, app *cli, args []string) (returnErr error) {
	id, args := leadingStageID(args)
	fs := flag.NewFlagSet("stage mergetool", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" || fs.NArg() > 0 {
		return usageErrorf("stage mergetool requires exactly one stage ID")
	}
	if app.nonInteractive || !interactiveInput(app.stdin) {
		return refusedError(errors.New("stage mergetool requires an interactive terminal"))
	}
	root, err := resolveRootFlagValue(fs, *rootFlag)
	if err != nil {
		return err
	}
	home, err := app.productHome()
	if err != nil {
		return err
	}
	record, base, local, err := stage.ReadReview(root, home, id)
	if err != nil {
		return classifyStageError(err)
	}
	remote, err := stage.ReadCurrentSource(root, home, id)
	if err != nil {
		return classifyStageError(err)
	}
	if bytes.Equal(base, remote) {
		return refusedError(errors.New("stage source has not changed since staging; use stage edit or stage difftool"))
	}
	merged, err := review.Merge(ctx, review.Streams{Stdin: app.stdin, Stdout: app.stdout, Stderr: app.stderr}, record.Source, base, local, remote)
	if err != nil {
		return refusedError(err)
	}
	// Publish under the project lock. The interactive merge runs outside the lock,
	// so refuse the rebase if a concurrent mutation changed the stage's frozen
	// source or review copy after the merge began.
	locks, err := safefs.AcquireLocks(home, []string{root}, "stage mergetool")
	if err != nil {
		return classifyRuntimeError(err)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Release()) }()
	productRoot, projectRoot, err := borrowStageCommandRoots(locks, root)
	if err != nil {
		return classifyRuntimeError(err)
	}
	_, currentBase, currentLocal, err := stage.ReadReviewFromRoot(root, home, id, productRoot)
	if err != nil {
		return classifyStageError(err)
	}
	if !bytes.Equal(currentBase, base) || !bytes.Equal(currentLocal, local) {
		return refusedError(errors.New("stage changed since the merge began; re-run stage mergetool"))
	}
	currentRemote, err := stage.ReadCurrentSourceFromRoots(root, home, id, productRoot, projectRoot)
	if err != nil {
		return classifyStageError(err)
	}
	if !bytes.Equal(currentRemote, remote) {
		return refusedError(errors.New("stage source changed while the merge was in progress; re-run stage mergetool"))
	}
	if err := stage.RebaseMergedReviewFromRoots(root, home, record.ID, remote, merged, productRoot, projectRoot); err != nil {
		return classifyStageError(err)
	}
	return nil
}

func runStageDiscard(app *cli, args []string) (returnErr error) {
	id, args := leadingStageID(args)
	fs := flag.NewFlagSet("stage discard", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	yes := fs.Bool("yes", false, "confirm discard")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" || fs.NArg() > 0 {
		return usageErrorf("stage discard requires exactly one stage ID")
	}
	if err := confirmMutation(app, *yes, "stage discard", "Discard this private review copy? [y/N]: "); err != nil {
		return err
	}
	root, err := resolveRootFlagValue(fs, *rootFlag)
	if err != nil {
		return err
	}
	home, err := app.productHome()
	if err != nil {
		return err
	}
	locks, err := safefs.AcquireLocks(home, []string{root}, "stage discard")
	if err != nil {
		return classifyRuntimeError(err)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Release()) }()
	productRoot, projectRoot, err := borrowStageCommandRoots(locks, root)
	if err != nil {
		return classifyRuntimeError(err)
	}
	if err := stage.DiscardFromRoots(root, home, id, productRoot, projectRoot); err != nil {
		return classifyStageError(err)
	}
	return nil
}

func runCommit(ctx context.Context, app *cli, args []string) (returnErr error) {
	id, args := leadingStageID(args)
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	fs.SetOutput(app.stderr)
	rootFlag := projectRootFlag(fs)
	planOnly := fs.Bool("plan", false, "preview commit")
	apply := fs.Bool("apply", false, "write reviewed output")
	yes := fs.Bool("yes", false, "confirm commit")
	format := fs.String("format", app.format, "text or json")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" || fs.NArg() > 0 {
		return usageErrorf("commit requires exactly one stage ID")
	}
	if *planOnly == *apply {
		return usageErrorf("commit requires exactly one of --plan or --apply")
	}
	outputFormat, err := resolveCommandOutputFormat(app, fs, *format, "commit")
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
	records, err := stage.List(root, home)
	if err != nil {
		return classifyStageError(err)
	}
	var record *stage.Record
	for index := range records {
		if records[index].ID == id {
			record = &records[index]
			break
		}
	}
	if record == nil {
		return refusedError(fmt.Errorf("staged item %s was not found", id))
	}
	if *planOnly {
		report := &stage.Report{SchemaVersion: "threadpoint.commit-plan.v1", Root: root, Stages: []stage.Record{*record}}
		return writeStageReport(app, outputFormat, "threadpoint commit --plan", report)
	}
	if err := confirmMutation(app, *yes, "commit", "Commit this reviewed project artifact? [y/N]: "); err != nil {
		return err
	}
	locks, err := safefs.AcquireLocks(home, []string{root}, "commit")
	if err != nil {
		return classifyRuntimeError(err)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Release()) }()
	productRoot, projectRoot, err := borrowStageCommandRoots(locks, root)
	if err != nil {
		return classifyRuntimeError(err)
	}
	if record.Scope == stage.ScopeProjectLocal {
		validateProjectRoot := func() error { return validateStageCommandProjectRoot(locks, root, projectRoot) }
		if err := layout.EnsureProjectLocalInRootValidated(projectRoot, validateProjectRoot); err != nil {
			return classifyStageError(err)
		}
	}
	if _, err := locks.BorrowProjectRoot(root); err != nil {
		return classifyRuntimeError(err)
	}
	if _, err := locks.BorrowProductRoot(); err != nil {
		return classifyRuntimeError(err)
	}
	report, err := stage.CommitFromRoots(ctx, stage.CommitOptions{Root: root, ThreadpointHome: home, ID: record.ID}, productRoot, projectRoot)
	if err != nil {
		return classifyStageError(err)
	}
	if outputFormat == outputFormatJSON {
		return app.printJSON(report)
	}
	fmt.Fprintf(app.stdout, "command: threadpoint commit\nid: %s\nscope: %s\ntarget: %s\nnext: %s\n", report.ID, report.Scope, report.Target, report.GitNextStep)
	return nil
}

func borrowStageCommandRoots(locks *safefs.LockSet, selectedRoot string) (*os.Root, *os.Root, error) {
	if locks == nil {
		return nil, nil, errors.New("stage command lock set is required")
	}
	projectRoot, err := locks.BorrowProjectRoot(selectedRoot)
	if err != nil {
		return nil, nil, err
	}
	if stageCommandAfterProjectRootBorrow != nil {
		stageCommandAfterProjectRootBorrow()
	}
	productRoot, err := locks.BorrowProductRoot()
	if err != nil {
		return nil, nil, err
	}
	return productRoot, projectRoot, nil
}

func validateStageCommandProjectRoot(locks *safefs.LockSet, selectedRoot string, retained *os.Root) error {
	if locks == nil || retained == nil {
		return errors.New("stage command lock and retained project root are required")
	}
	current, err := locks.BorrowProjectRoot(selectedRoot)
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

func leadingStageID(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func discoverStageInputs(ctx context.Context, root string, providers, excluded []provider.ID, explicit bool, skipDirNames []string) ([]stage.Input, error) {
	report, err := discover.Run(ctx, discover.Options{
		Root: root, SkipDirNames: skipDirNames, Providers: providers, ExcludeProviders: excluded, ProviderSelectionExplicit: explicit,
	})
	if err != nil {
		return nil, err
	}
	inputs := make([]stage.Input, 0, len(report.Artifacts))
	for _, artifact := range report.Artifacts {
		if artifact.ReadMode != discover.ReadImport || artifact.Directory || artifact.Symlink {
			continue
		}
		switch stage.Kind(artifact.Kind) {
		case stage.KindInstruction, stage.KindKnowledge, stage.KindRule, stage.KindSkill, stage.KindPrompt, stage.KindCommand, stage.KindAgent:
			inputs = append(inputs, stage.Input{
				Provider:      string(artifact.Provider),
				Source:        artifact.Path,
				Kind:          stage.Kind(artifact.Kind),
				RequiredScope: stage.Scope(artifact.CanonicalScope),
			})
		}
	}
	return inputs, nil
}

func parseClassifications(values []string) (map[string]stage.Scope, error) {
	result := map[string]stage.Scope{}
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key, value, ok := strings.Cut(part, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return nil, fmt.Errorf("invalid --classify value %q", part)
			}
			scope := stage.Scope(strings.TrimSpace(value))
			if scope != stage.ScopeProjectShared && scope != stage.ScopeProjectLocal {
				return nil, fmt.Errorf("invalid --classify scope %q", value)
			}
			key = strings.TrimPrefix(filepathToSlash(strings.TrimSpace(key)), "./")
			if previous, exists := result[key]; exists && previous != scope {
				return nil, fmt.Errorf("conflicting --classify values for %s", key)
			}
			result[key] = scope
		}
	}
	return result, nil
}

func filepathToSlash(value string) string { return strings.ReplaceAll(value, "\\", "/") }

func writeStageReport(app *cli, outputFormat, command string, report *stage.Report) error {
	if outputFormat == outputFormatJSON {
		return app.printJSON(report)
	}
	fmt.Fprintf(app.stdout, "command: %s\nroot: %s\nstages: %d\n", command, report.Root, len(report.Stages))
	for _, item := range report.Stages {
		fmt.Fprintf(app.stdout, "  - %s %s -> %s", item.Scope, item.Source, item.Target)
		if item.ID != "" {
			fmt.Fprintf(app.stdout, " (%s)", item.ID)
		}
		fmt.Fprintln(app.stdout)
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(app.stdout, "warning: %s\n", warning)
	}
	return nil
}

func classifyStageError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, stage.ErrClassificationRequired) || errors.Is(err, stage.ErrAlreadyStaged) {
		return refusedError(err)
	}
	return err
}
