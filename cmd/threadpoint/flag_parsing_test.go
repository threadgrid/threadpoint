// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestRootPlacementHintUsesFlagBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		hint bool
	}{
		{"bare root", []string{"--root"}, true},
		{"single dash", []string{"-root", "project"}, true},
		{"equals", []string{"--root=project"}, true},
		{"after boolean", []string{"--quiet", "--root", "project"}, true},
		{"after string", []string{"--label", "value", "--root=project"}, true},
		{"after integer", []string{"--count", "2", "--root=project"}, true},
		{"unrelated unknown", []string{"--bogus"}, false},
		{"root prefix", []string{"--rooted=project"}, false},
		{"invalid syntax", []string{"---root"}, false},
		{"terminator", []string{"--", "--root"}, false},
		{"positional", []string{"project", "--root"}, false},
		{"dash positional", []string{"-", "--root"}, false},
		{"string value", []string{"--label", "--root"}, false},
		{"equals value", []string{"--label=--root"}, false},
		{"missing value", []string{"--label"}, false},
		{"invalid integer value", []string{"--count", "--root"}, false},
		{"earlier integer failure", []string{"--count=bad", "--root"}, false},
		{"earlier boolean failure", []string{"--quiet=bad", "--root"}, false},
		{"earlier unknown", []string{"--bogus", "--root"}, false},
		{"earlier help", []string{"--help", "--root"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newFixture := func() *flag.FlagSet {
				fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				fs.String("label", "", "")
				fs.Bool("quiet", false, "")
				fs.Int("count", 0, "")
				return fs
			}
			fs := newFixture()
			err := parseFlagsWithRootHint(fs, tc.args)
			if tc.hint {
				if err == nil || !strings.Contains(err.Error(), "--root is a ") {
					t.Fatalf("missing root hint: %v", err)
				}
				return
			}
			expected := newFixture()
			wantErr := expected.Parse(tc.args)
			if (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
				t.Fatalf("changed parser error: got %v, want %v", err, wantErr)
			}
			if !reflect.DeepEqual(fs.Args(), expected.Args()) {
				t.Fatalf("remaining args = %q, want %q", fs.Args(), expected.Args())
			}
			for _, name := range []string{"label", "quiet", "count"} {
				if got, want := fs.Lookup(name).Value.String(), expected.Lookup(name).Value.String(); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestRootPlacementHintAllowsRegisteredRoot(t *testing.T) {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", "", "")
	if err := parseFlagsWithRootHint(fs, []string{"--root=project"}); err != nil || *root != "project" {
		t.Fatalf("registered root = %q, error %v", *root, err)
	}
}

func TestGlobalParserReportsMisplacedRoot(t *testing.T) {
	app := newCLI(io.Discard, io.Discard, strings.NewReader(""))
	_, _, err := app.parseGlobalFlags([]string{"--root=project", "status"})
	if err == nil || exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "use threadpoint <command> --root PATH") {
		t.Fatalf("global root hint: %v", err)
	}
}
