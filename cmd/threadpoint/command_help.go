// SPDX-License-Identifier: Apache-2.0

package main

//go:generate go run ../../scripts/cli-help-flags.go -dir . -spdx Apache-2.0
import (
	"fmt"
	"strings"
)

type commandHelpTopic struct {
	path, args, description, flags string
	group, helpOnly, children      bool
	legacy                         string
	print                          func(*cli) error
}

type helpRequest int

const (
	noHelp helpRequest = iota
	explicitHelp
	groupHelp
)

// resolveCommandHelp validates paths without running operations or rendering help.
func resolveCommandHelp(args []string, topics map[string]commandHelpTopic) (commandHelpTopic, helpRequest, error) {
	if len(args) == 0 {
		return topics[""], explicitHelp, nil
	}
	if args[0] == "help" {
		path := strings.Join(args[1:], " ")
		topic, ok := topics[path]
		if !ok {
			return commandHelpTopic{}, explicitHelp, usageErrorf("unknown help topic %q", path)
		}
		return topic, explicitHelp, nil
	}
	path := args[0]
	request := noHelp
	if hasHelpFlag(args[1:]) {
		request = explicitHelp
	}
	topic, ok := topics[path]
	if !ok || topic.helpOnly {
		return commandHelpTopic{}, request, usageErrorf("unknown command %q", path)
	}
	i := 1
	for i < len(args) {
		if args[i] == "help" {
			if i+1 != len(args) {
				return commandHelpTopic{}, explicitHelp, usageErrorf("unexpected arguments after help")
			}
			return topic, explicitHelp, nil
		}
		if strings.HasPrefix(args[i], "-") || !topic.children {
			break
		}
		next, exists := topics[path+" "+args[i]]
		if !exists {
			return commandHelpTopic{}, request, usageErrorf("unknown %s subcommand %q", path, args[i])
		}
		path += " " + args[i]
		topic = next
		i++
	}
	if hasHelpFlag(args[i:]) {
		return topic, explicitHelp, nil
	}
	if topic.group && i == len(args) {
		return topic, groupHelp, nil
	}
	return topic, noHelp, nil
}

var commandHelpTopics = makeCommandHelpTopics()

func (topic commandHelpTopic) render(app *cli) error {
	if topic.print != nil {
		return topic.print(app)
	}
	if topic.legacy != "" {
		return app.printCommandHelp(topic.legacy)
	}
	usage := strings.TrimSpace(topic.path + " " + topic.args)
	text := "usage: threadpoint " + usage + "\n\n" + topic.description + "\n"
	if topic.flags != "" {
		text += "\n" + strings.TrimRight(topic.flags, "\n") + "\n"
	}
	_, err := fmt.Fprint(app.stderr, text)
	return err
}

func makeCommandHelpTopics() map[string]commandHelpTopic {
	topics := map[string]commandHelpTopic{}
	topics["init"] = commandHelpTopic{legacy: "init"}
	topics["stage"] = commandHelpTopic{legacy: "stage"}
	topics["commit"] = commandHelpTopic{legacy: "commit"}
	topics["prune"] = commandHelpTopic{legacy: "prune"}
	topics["restore"] = commandHelpTopic{legacy: "restore"}
	topics["status"] = commandHelpTopic{legacy: "status"}
	topics["doctor"] = commandHelpTopic{legacy: "doctor"}
	topics["version"] = commandHelpTopic{legacy: "version"}
	topics["update"] = commandHelpTopic{legacy: "update"}
	topics["uninstall"] = commandHelpTopic{legacy: "uninstall"}
	topics[""] = commandHelpTopic{print: (*cli).printHelp}
	topics["update check"] = commandHelpTopic{print: runUpdateCheckHelp}
	topics["update rollback"] = commandHelpTopic{print: func(app *cli) error { runUpdateApplyHelp(app, updateOperationRollback); return nil }}
	add := func(path, args, description, flags string, group bool) {
		topics[path] = commandHelpTopic{path: path, args: args, description: description, flags: flags, group: group}
	}
	add("stage list", "", "List the private review stages for the selected project.", "  --root PATH          project root override (optional; otherwise discover from current directory)\n  --format text|json   output format (default \"text\")\n", false)
	add("stage diff", "ID", "Print the raw local unified diff. Review content before sharing.", "  --root PATH          project root override (optional; otherwise discover from current directory)\n", false)
	add("stage edit", "ID", "Edit a private review copy using VISUAL or EDITOR. Requires an interactive terminal.", "  --root PATH          project root override (optional; otherwise discover from current directory)\n", false)
	add("stage difftool", "ID", "Compare the frozen source and review copy using DIFF. Requires an interactive terminal.", "  --root PATH          project root override (optional; otherwise discover from current directory)\n", false)
	add("stage mergetool", "ID", "Merge changed source into the review copy using MERGE. Requires an interactive terminal.", "  --root PATH          project root override (optional; otherwise discover from current directory)\n", false)
	add("stage discard", "ID", "Discard one private review copy; requires confirmation.", "  --root PATH          project root override (optional; otherwise discover from current directory)\n  --yes                confirm this operation\n", false)
	add("restore list", "", "List backup runs for the selected root without restoring files.", "  --root PATH          project root override (optional; otherwise discover from current directory)\n  --format text|json   output format (default \"text\")\n  --backup-namespace NAME  relative backup namespace\n", false)
	add("update reminder", "<status|enable|disable|dismiss>", "Manage automatic update reminders.", "Subcommands:\n  status     show reminder preferences\n  enable     turn automatic reminders on\n  disable    turn automatic reminders off\n  dismiss    hide one release version\n", true)
	add("update reminder status", "", "Show local reminder preferences and the dismissed version.", "  --format text|json   output format (default \"text\")\n", false)
	add("update reminder enable", "", "Enable automatic update reminders; requires confirmation.", "  --format text|json   output format (default \"text\")\n  --yes                confirm this operation\n", false)
	add("update reminder disable", "", "Disable automatic update reminders; requires confirmation.", "  --format text|json   output format (default \"text\")\n  --yes                confirm this operation\n", false)
	add("update reminder dismiss", "--version VERSION", "Dismiss the reminder for one release version; requires confirmation.", "  --format text|json   output format (default \"text\")\n  --yes                confirm this operation\n  --version VERSION    release version to dismiss (required)\n", false)
	add("lock", "<clear>", "Manage the selected project's mutation lock.", "Subcommands:\n  clear    remove a stale lock; force removal requires --force --yes\n", true)
	add("lock clear", "[flags]", "Clear the selected project's stale mutation lock. Force removal can overlap a running writer; stop the owning process first.", "  --root PATH          "+rootFlagUsage+"\n  --force              remove a fresh lock; requires --yes\n  --yes                confirm forced removal\n  --format text|json   output format\n", false)
	normalizeCommandHelpTopics(topics)
	return topics
}

func normalizeCommandHelpTopics(topics map[string]commandHelpTopic) {
	parents := make(map[string]bool)
	for path := range topics {
		if index := strings.LastIndex(path, " "); index >= 0 {
			parent := path[:index]
			if _, exists := topics[parent]; !exists {
				panic(fmt.Sprintf("help topic %q requires registered parent %q", path, parent))
			}
			parents[parent] = true
		}
	}
	for path, topic := range topics {
		topic.path = path
		topic.children = parents[path]
		topics[path] = topic
	}
}
