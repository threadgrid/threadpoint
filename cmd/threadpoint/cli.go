// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	tphome "github.com/threadgrid/threadpoint/internal/home"
	"github.com/threadgrid/threadpoint/provider"
	"github.com/threadgrid/threadpoint/redact"
	"github.com/threadgrid/threadpoint/restore"
)

type cli struct {
	stdout          io.Writer
	stderr          io.Writer
	stdin           io.Reader
	commands        []command
	release         releaseClient
	format          string
	formatExplicit  bool
	quiet           bool
	nonInteractive  bool
	threadpointHome string
	restoreApply    func(context.Context, restore.Options) (*restore.Report, error)
	restoreRun      func(context.Context, restore.Options, io.Reader, io.Writer, io.Writer) (*restore.Report, error)
}

type command struct {
	name      string
	summary   string
	usage     string
	details   string
	output    string
	failure   string
	flagLines []string
	run       func(context.Context, *cli, []string) error
}

const (
	threadpointGlossaryURL    = "https://github.com/threadgrid/threadpoint/blob/main/docs/glossary.md"
	threadpointLayoutGuideURL = "https://github.com/threadgrid/threadpoint/blob/main/docs/canonical-layout-guide.md"
	threadpointProductName    = "threadpoint"
	longFormatFlag            = "--format"
	longHelpFlag              = "--help"
	longRootFlag              = "--root"
)

func newCLI(stdout io.Writer, stderr io.Writer, stdin io.Reader) *cli {
	app := &cli{
		stdout:       stdout,
		stderr:       stderr,
		stdin:        stdin,
		release:      defaultReleaseClient(),
		format:       outputFormatText,
		restoreApply: restore.Apply,
		restoreRun:   restore.RunInteractive,
	}
	app.commands = []command{
		{
			name:    "init",
			summary: "Create the shared agent knowledgebase layout if missing.",
			usage:   "threadpoint init [--root path] [--format text|json] [--yes]",
			details: "Creates the shared AGENTS.md and .agents skeleton when they do not already exist. Local project memory is created only by an import workflow that needs it. Requires confirmation before writing.",
			output:  "Reports the selected root on stderr on text success (unless --quiet), with no stdout; --format json writes a JSON init report to stdout.",
			failure: "Usage errors exit 2. Unexpected filesystem failures exit 1.",
			flagLines: []string{
				`  --root path          ` + rootFlagUsage,
				`  --format text|json  output format (default "text")`,
				`  --yes               confirm layout initialization`,
			},
			run: runInit,
		},
		{
			name:    "lock",
			summary: "Clear the selected project's stale mutation lock.",
			usage:   "threadpoint lock clear [--root path] [--force --yes] [--format text|json]",
			details: "Clears the one cooperative mutation lock for the selected project root when stale. Force cleanup requires --force --yes and can overlap a still-running writer.",
			output:  "Writes a text cleanup result by default, or JSON with --format json.",
			failure: "Usage errors exit 2. Protected locks and missing force confirmation exit 4. Unexpected filesystem failures exit 1.",
			flagLines: []string{
				`  --root path          ` + rootFlagUsage,
				`  --force              remove a fresh lock; requires --yes`,
				`  --yes                confirm forced lock cleanup`,
				`  --format text|json   output format (default "text")`,
			},
			run: runLocks,
		},
		{
			name:    "stage",
			summary: "Copy native project artifacts into private, scope-pinned review stages.",
			usage:   "threadpoint stage [--root path] [--plan|--apply] [flags]\n  threadpoint stage <list|diff|edit|difftool|mergetool|discard> [flags]",
			details: "Discovers importable project artifacts, classifies Git-tracked files as project-shared and pre-existing ignored files as project-local, and keeps review copies only under THREADPOINT_HOME. Threadpoint only manages ignores for canonical local outputs; ignored native artifacts receive a consolidation warning. Unclassified files require --classify.",
			output:  "Writes a stage plan or report in text by default, or JSON with --format json.",
			failure: "Usage errors exit 2. Missing classifications and unsafe mutations exit 4.",
			flagLines: []string{
				`  --root path                   ` + rootFlagUsage,
				`  --plan                        preview stages without writing`,
				`  --apply                       create private review copies; requires --yes`,
				`  --yes                         confirm stage creation`,
				`  --classify source=scope       repeat or comma-separate project-shared/project-local mappings`,
				`  --skip-dirs list              comma-separated directory basenames to skip`,
				`  --format text|json            output format (default "text")`,
				fmt.Sprintf(`  --providers list              comma-separated providers to include: %s`, provider.ListString()),
				`  --exclude-providers list      comma-separated providers to exclude`,
				`  list [--root path]`,
				`  diff ID [--root path]`,
				`  edit ID [--root path]`,
				`  difftool ID [--root path]`,
				`  mergetool ID [--root path]`,
				`  discard ID --yes [--root path]`,
			},
			run: runStage,
		},
		{
			name:    "commit",
			summary: "Commit one reviewed project stage into its immutable scope.",
			usage:   "threadpoint commit ID [--root path] [--plan|--apply] [--yes] [--format text|json]",
			details: "Writes only the staged item's project-shared or project-local target, validates its Git classification, removes that exact review stage on success, and never runs git add or git commit.",
			output:  "Writes a commit plan or report in text by default, or JSON with --format json.",
			failure: "Usage errors exit 2. Scope, Git, stale-stage, and confirmation refusals exit 4.",
			flagLines: []string{
				`  --root path          ` + rootFlagUsage,
				`  --plan               preview the staged target`,
				`  --apply              write the reviewed project artifact; requires --yes`,
				`  --yes                confirm commit apply`,
				`  --format text|json   output format (default "text")`,
			},
			run: runCommit,
		},
		{
			name:    "prune",
			summary: "Remove native agent artifacts that still match their latest reviewed commit.",
			usage:   "threadpoint prune [--root path] [--plan|--apply] [flags]",
			details: "Uses threadpoint backups to verify native artifacts still match their imported source snapshots before removing them.",
			output:  "Writes a text prune plan or apply report by default, or JSON with --format json.",
			failure: "Usage errors exit 2. Missing confirmation, blocked candidates, or changed artifacts exit 4. Unexpected failures exit 1.",
			flagLines: []string{
				`  --root path          ` + rootFlagUsage,
				`  --plan               print a prune plan (default)`,
				`  --apply              remove verified native artifacts`,
				`  --yes                confirm scripted prune apply`,
				`  --format text|json   output format (default "text")`,
				`  --backup-namespace name  backup namespace under .threadpoint/backups`,
				`  --confirm phrase     confirmation phrase: "remove replaced agent artifacts"`,
			},
			run: runPrune,
		},
		{
			name:    "restore",
			summary: "Restore files from threadpoint backups.",
			usage:   "threadpoint restore [--root path] [--plan|--apply] [flags]\n  threadpoint restore list [--root path] [--backup-namespace name] [--format text|json]",
			details: "Lists backup runs, plans a rollback from a selected run, applies clean restores with confirmation, or opens an interactive conflict resolver.",
			output:  "Writes text for list, --plan, or --apply by default, or JSON with --format json. Interactive mode writes conflicts, diffs, and prompts to stdout.",
			failure: "Usage errors exit 2. Missing confirmation, noninteractive interactive mode, or overwrite conflicts exit 4. Unexpected failures exit 1.",
			flagLines: []string{
				`  --root path          ` + rootFlagUsage,
				`  --backup id          restore from a specific backup run`,
				`  --latest             restore from the latest backup run (default when --backup is omitted)`,
				`  --plan               print a restore plan`,
				`  --apply              apply clean restore candidates; requires --yes or --confirm`,
				`  --yes                confirm scripted restore apply`,
				`  --format text|json   output format (default "text")`,
				`  --backup-namespace name  backup namespace under .threadpoint/backups`,
				`  --confirm phrase     confirmation phrase: "restore threadpoint backup"`,
			},
			run: runRestore,
		},
		{
			name:    "status",
			summary: "Inspect memory inventory, validate the layout, and report detailed project workflow state.",
			usage:   "threadpoint status [--root path] [flags]",
			details: "Prints a human-readable project state report by default, or JSON with --format json. Checks only threadpoint-managed and known agent artifact paths for Git state.",
			output:  "Writes a text report to stdout by default, or JSON with --format json.",
			failure: "Usage errors exit 2. A completed report with errors exits 3; warnings and pending work exit 0. Unexpected failures exit 1.",
			flagLines: []string{
				`  --root path                   ` + rootFlagUsage,
				`  --format text|json            output format (default "text")`,
				fmt.Sprintf(`  --providers list              comma-separated providers to include: %s`, provider.ListString()),
				`  --exclude-providers list      comma-separated providers to exclude`,
				`  --backup-namespace name       backup namespace under .threadpoint/backups`,
				`  --skip-dirs list              comma-separated directory basenames to skip`,
			},
			run: runStatus,
		},
		{
			name:    "doctor",
			summary: "Check the threadpoint tool itself: version, install, home, and update availability.",
			usage:   "threadpoint doctor [--offline] [--format text|json]",
			details: "Runs tool self-checks: build and install identity, threadpoint home resolution, and (online) update availability. Does not inspect any project or repository.",
			output:  "Writes a text report to stdout by default, or JSON with --format json.",
			failure: "Usage errors exit 2. A completed report with blocking findings exits 3. Unexpected failures exit 1.",
			flagLines: []string{
				`  --offline           skip network checks (update availability)`,
				`  --format text|json  output format (default "text")`,
			},
			run: runDoctor,
		},
		{
			name:    "version",
			summary: "Print build and install metadata.",
			usage:   "threadpoint version [--format text|json]",
			details: "Reports the binary version, commit, build date, Go runtime, platform, executable path, and best-effort install source.",
			output:  "Writes a text report to stdout by default, or JSON with --format json.",
			failure: "Usage errors exit 2. Unexpected failures exit 1.",
			flagLines: []string{
				`  --format text|json  output format (default "text")`,
			},
			run: runVersion,
		},
		{
			name:    "update",
			summary: "Check release metadata, update managed installs, and configure reminders.",
			usage:   "threadpoint update [flags]\n  threadpoint update <check|rollback|reminder> [flags]",
			details: "Checks official release metadata, prompts before updating installer-managed bundles, rolls preview users back to stable, and manages opt-in update reminders.",
			output:  "Writes a text or JSON update report to stdout.",
			failure: "Usage errors exit 2. Missing installer metadata, unsupported channels, checksum or signature mismatches, attestation failures, invalid release metadata, or declined preview confirmation exit 4. Unexpected failures exit 1.",
			flagLines: []string{
				`  --repo owner/repo       release repository to inspect (default "threadgrid/threadpoint")`,
				`  --channel stable|preview release channel to update from (default "stable")`,
				`  --dir path              command directory`,
				`  --max-age duration      cache freshness window (default "15m")`,
				`  --no-cache              skip update cache`,
				`  --format text|json      output format (default "text")`,
				`  --yes                   confirm update for noninteractive use`,
				`  --skip-attestation      skip GitHub Artifact Attestation verification`,
				`  check       inspect stable or preview release metadata`,
				`  rollback    install the latest stable release`,
				`  reminder    manage reminders: status, enable, disable, dismiss`,
			},
			run: runUpdate,
		},
		{
			name:    "uninstall",
			summary: "Remove an installer-managed threadpoint bundle.",
			usage:   "threadpoint uninstall [--dir path] [--plan|--apply] [--yes] [--format text|json]",
			details: "Removes only bundles installed by the threadpoint installer after matching ownership metadata and the current binary checksum.",
			output:  "Writes a text uninstall plan or apply report by default, or JSON with --format json.",
			failure: "Usage errors exit 2. Missing metadata, unsupported channels, checksum mismatches, or missing --yes exit 4. Unexpected failures exit 1.",
			flagLines: []string{
				`  --dir path  command directory (default: recorded command symlink directory when safe, otherwise $HOME/.local/bin)`,
				`  --plan      print the uninstall plan (default)`,
				`  --apply     remove the installer-managed bundle and metadata`,
				`  --yes       required with --apply`,
				`  --format text|json  output format (default "text")`,
			},
			run: runUninstall,
		},
	}
	return app
}

func (app *cli) run(ctx context.Context, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	remaining, help, err := app.parseGlobalFlags(args)
	if err != nil {
		return err
	}
	if help || len(remaining) == 0 {
		return app.printHelp()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	topic, wantsHelp, err := resolveCommandHelp(remaining, commandHelpTopics)
	if err != nil {
		return err
	}
	if wantsHelp == groupHelp && app.format == outputFormatJSON {
		wantsHelp = noHelp
	}
	if wantsHelp != noHelp {
		return topic.render(app)
	}

	cmd, ok := app.lookup(remaining[0])
	if !ok {
		return usageErrorf("unknown command %q", remaining[0])
	}
	if err := cmd.run(ctx, app, remaining[1:]); err != nil {
		return err
	}
	maybePrintAutomaticUpdateReminder(ctx, app, remaining)
	return nil
}

func (app *cli) parseGlobalFlags(args []string) ([]string, bool, error) {
	app.format = outputFormatText
	app.formatExplicit = false
	app.quiet = false
	app.nonInteractive = false

	fs := flag.NewFlagSet("threadpoint", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "threadpoint home")
	format := fs.String("format", outputFormatText, "text or json")
	quiet := fs.Bool("quiet", false, "suppress nonessential output")
	nonInteractive := fs.Bool("non-interactive", false, "disable interactive prompts")
	if err := parseFlagsWithRootHint(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, true, nil
		}
		return nil, false, usageError(err)
	}
	if flagProvided(fs, "format") {
		normalized, err := normalizeOutputFormat(*format)
		if err != nil {
			return nil, false, usageError(err)
		}
		app.format = normalized
		app.formatExplicit = true
	}
	app.quiet = *quiet
	app.nonInteractive = *nonInteractive
	if flagProvided(fs, "home") && strings.TrimSpace(*home) == "" {
		return nil, false, usageErrorf("--home cannot be empty")
	}
	threadpointHome, err := tphome.ResolveWithOverride("", *home)
	if err != nil {
		return nil, false, err
	}
	app.threadpointHome = threadpointHome
	return fs.Args(), false, nil
}

// parseFlagsWithRootHint recognizes a misplaced root flag using registrations,
// not parser error wording. The consumed argument count identifies the failing
// token, preserving earlier errors and the parser's values and remaining args.
func parseFlagsWithRootHint(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if err == nil || fs.Lookup("root") != nil {
		return err
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) < 2 || arg[0] != '-' || arg == "--" {
			break
		}
		name := strings.TrimPrefix(arg, "-")
		name = strings.TrimPrefix(name, "-")
		name, _, hasValue := strings.Cut(name, "=")
		if name == "root" {
			if len(args)-fs.NArg() == i+1 {
				return fmt.Errorf("%w; --root is a project command flag; use threadpoint <command> --root PATH", err)
			}
			break
		}
		option := fs.Lookup(name)
		if option == nil {
			break
		}
		if !hasValue {
			boolean, ok := option.Value.(interface{ IsBoolFlag() bool })
			if !ok || !boolean.IsBoolFlag() {
				i++
			}
		}
	}
	return err
}

func (app *cli) productHome() (string, error) {
	if strings.TrimSpace(app.threadpointHome) != "" {
		return app.threadpointHome, nil
	}
	return tphome.ResolveWithOverride("", "")
}

func (app *cli) lookup(name string) (command, bool) {
	for _, cmd := range app.commands {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

func (app *cli) printHelp() error {
	fmt.Fprintln(app.stderr, "Threadpoint maps native agent memory, instructions, and skills into a shared knowledgebase.")
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Usage:")
	fmt.Fprintln(app.stderr, "  threadpoint [global flags] <command> [flags]")
	fmt.Fprintln(app.stderr, "  threadpoint help <command>")
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Commands:")
	for _, cmd := range app.commands {
		fmt.Fprintf(app.stderr, "  %-8s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Global flags:")
	fmt.Fprintln(app.stderr, `  --home path           threadpoint home (default "$HOME/.threadpoint")`)
	fmt.Fprintln(app.stderr, `  --format text|json    output format for text-capable commands (default "text")`)
	fmt.Fprintln(app.stderr, "  --quiet               suppress nonessential output")
	fmt.Fprintln(app.stderr, "  --non-interactive     disable interactive prompts")
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Project commands discover their root from the current directory.")
	fmt.Fprintln(app.stderr, "The optional command flag --root PATH selects an exact root; --root . disables ancestor discovery.")
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, `Use "threadpoint help <command>" for command-specific help.`)
	app.printReferenceLinks()
	return nil
}

func (app *cli) printReferenceLinks() {
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Reference:")
	fmt.Fprintf(app.stderr, "  Glossary: %s\n", threadpointGlossaryURL)
	fmt.Fprintf(app.stderr, "  Layout guide: %s\n", threadpointLayoutGuideURL)
}

func (app *cli) printCommandHelp(name string) error {
	cmd, ok := app.lookup(name)
	if !ok {
		return fmt.Errorf("unknown command %q", name)
	}
	fmt.Fprintln(app.stderr, cmd.summary)
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, "Usage:")
	fmt.Fprintf(app.stderr, "  %s\n", cmd.usage)
	fmt.Fprintln(app.stderr)
	fmt.Fprintln(app.stderr, cmd.details)
	fmt.Fprintln(app.stderr)
	if cmd.output != "" {
		fmt.Fprintln(app.stderr, "Output:")
		fmt.Fprintf(app.stderr, "  %s\n", cmd.output)
		fmt.Fprintln(app.stderr)
	}
	if cmd.failure != "" {
		fmt.Fprintln(app.stderr, "Failure:")
		fmt.Fprintf(app.stderr, "  %s\n", cmd.failure)
		fmt.Fprintln(app.stderr)
	}
	fmt.Fprintln(app.stderr, "Flags:")
	for _, line := range cmd.flagLines {
		fmt.Fprintln(app.stderr, line)
	}
	app.printReferenceLinks()
	return nil
}

func hasHelpFlag(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == longHelpFlag {
			return true
		}
		if helpScannerConsumesValue(arg) && i+1 < len(args) {
			i++
		}
	}
	return false
}

func (app *cli) printJSON(value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(app.stdout, string(body))
	return nil
}

func (app *cli) outputRedaction(root string, threadpointHome string) redact.Options {
	prefixes := []redact.Prefix{
		{Value: root, Label: "$ROOT"},
		{Value: threadpointHome, Label: "$THREADPOINT_HOME"},
	}
	if home, err := os.UserHomeDir(); err == nil {
		prefixes = append(prefixes, redact.Prefix{Value: home, Label: "$HOME"})
	}
	return redact.Options{Prefixes: prefixes}
}

func (app *cli) printRedactedJSON(value any, options redact.Options) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	redacted, _ := redact.StringWithOptions(string(body), options)
	fmt.Fprintln(app.stdout, redacted)
	return nil
}

func (app *cli) printRedactedText(value string, options redact.Options) {
	value, _ = redact.StringWithOptions(value, options)
	fmt.Fprint(app.stdout, value)
}
