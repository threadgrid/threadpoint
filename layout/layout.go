// SPDX-License-Identifier: Apache-2.0

// Package layout defines, validates, and creates threadpoint's provider-neutral
// project layout.
package layout

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/threadgrid/threadpoint/backup"
	"github.com/threadgrid/threadpoint/internal/layoutname"
	"github.com/threadgrid/threadpoint/safefs"
)

var (
	//go:embed assets/project-agents.md
	projectGuideSeed string
	//go:embed assets/project-adapters.md
	projectAdaptersSeed string
	// beforeGitignoreCompare is a test seam for a concurrent editor save at the
	// final compare-and-swap boundary.
	beforeGitignoreCompare func()
)

var errGitignoreGenerationChanged = errors.New(".gitignore changed while merging required entries")

// Canonical project-layout names.
const (
	AgentsDir       = layoutname.AgentsDir
	GuideFile       = layoutname.GuideFile
	KnowledgeDir    = layoutname.KnowledgeDir
	RulesDir        = layoutname.RulesDir
	SkillsDir       = layoutname.SkillsDir
	PluginsDir      = layoutname.PluginsDir
	AdaptersDir     = layoutname.AdaptersDir
	ProjectsDir     = layoutname.ProjectsDir
	MarketplaceFile = layoutname.MarketplaceFile
)

// Issue describes one layout validation issue or warning.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Report states whether a workspace has the required shared layout.
type Report struct {
	Root     string  `json:"root"`
	OK       bool    `json:"ok"`
	Issues   []Issue `json:"issues,omitempty"`
	Warnings []Issue `json:"warnings,omitempty"`
}

// Validate checks whether root contains the required shared agent layout.
func Validate(root string) Report {
	report := Report{Root: root, OK: true}
	requiredFiles := []string{
		"AGENTS.md",
		filepath.Join(".agents", "adapters", "README.md"),
	}
	requiredDirs := []string{
		filepath.Join(".agents", "knowledge"),
		filepath.Join(".agents", "rules"),
	}
	optionalDirs := []string{
		filepath.Join(".agents", "skills"),
		filepath.Join(".agents", "plugins"),
	}

	for _, rel := range requiredFiles {
		path := filepath.Join(root, rel)
		info, err := os.Lstat(path)
		if err != nil {
			report.OK = false
			report.Issues = append(report.Issues, Issue{
				Path:    rel,
				Message: "required shared-agent bootstrap file is missing",
			})
			continue
		}
		if !info.Mode().IsRegular() {
			report.OK = false
			report.Issues = append(report.Issues, Issue{
				Path:    rel,
				Message: "required shared-agent bootstrap path is not a regular file",
			})
		}
	}

	for _, rel := range requiredDirs {
		path := filepath.Join(root, rel)
		info, err := os.Stat(path)
		if err != nil {
			report.OK = false
			report.Issues = append(report.Issues, Issue{
				Path:    rel,
				Message: "required shared-agent bootstrap directory is missing",
			})
			continue
		}
		if !info.IsDir() {
			report.OK = false
			report.Issues = append(report.Issues, Issue{
				Path:    rel,
				Message: "required shared-agent bootstrap path is not a directory",
			})
		}
	}

	for _, rel := range optionalDirs {
		path := filepath.Join(root, rel)
		info, err := os.Stat(path)
		if err != nil {
			report.Warnings = append(report.Warnings, Issue{
				Path:    rel,
				Message: "optional shared-agent extension directory is missing",
			})
			continue
		}
		if !info.IsDir() {
			report.Warnings = append(report.Warnings, Issue{
				Path:    rel,
				Message: "optional shared-agent extension path is not a directory",
			})
		}
	}

	return report
}

// EnsureShared creates the minimal shared agent layout under root.
func EnsureShared(root string) error {
	mutationRoot, err := ensureSharedDirectories(root)
	if err != nil {
		return err
	}
	if err := writeIfMissing(filepath.Join(mutationRoot, "AGENTS.md"), projectGuideSeed); err != nil {
		return err
	}
	return writeIfMissing(filepath.Join(mutationRoot, ".agents", "adapters", "README.md"), projectAdaptersSeed)
}

// EnsureProjectBootstrapInRoot creates the complete init layout through an
// already-retained project root. Callers keep ownership of root. No ambient
// pathname is reopened, so a selected logical root that is renamed or
// retargeted during initialization cannot redirect any bootstrap mutation.
func EnsureProjectBootstrapInRoot(root *os.Root) error {
	return EnsureProjectBootstrapInRootValidated(root, nil)
}

// EnsureProjectBootstrapInRootValidated is EnsureProjectBootstrapInRoot with
// a lock-generation validator invoked at every filesystem commit boundary.
func EnsureProjectBootstrapInRootValidated(root *os.Root, validate func() error) error {
	if root == nil {
		return errors.New("root is required")
	}
	if err := EnsureSharedDirectoriesInRootValidated(root, validate); err != nil {
		return err
	}
	if err := writeRootIfMissingValidated(root, "AGENTS.md", projectGuideSeed, validate); err != nil {
		return err
	}
	if err := writeRootIfMissingValidated(root, filepath.Join(".agents", "adapters", "README.md"), projectAdaptersSeed, validate); err != nil {
		return err
	}
	for _, rel := range projectSkillStateDirectoryPaths() {
		if err := safefs.RejectRootSymlinkAncestors(root, rel); err != nil {
			return err
		}
		if err := validateRootMutation(validate); err != nil {
			return err
		}
		// #nosec G301 -- skill runtime state is user-managed project-local data.
		if err := root.MkdirAll(rel, 0o777); err != nil {
			return err
		}
		if err := validateRootMutation(validate); err != nil {
			return err
		}
	}
	return ensureGitignoreEntriesInRootValidated(root, ".gitignore", []string{"/state/"}, validate)
}

// EnsureProjectSkillState creates the ignored project-local runtime
// location used by skills. Skill definitions remain under .agents/skills;
// mutable data belongs under state/skills/<skill-name>.
func EnsureProjectSkillState(root string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("root is required")
	}
	mutationRoot, err := safefs.ResolveRootForMutation(root)
	if err != nil {
		return err
	}
	for _, rel := range projectSkillStateDirectoryPaths() {
		path := filepath.Join(mutationRoot, rel)
		if err := safefs.RejectExistingSymlinkAncestors(path); err != nil {
			return err
		}
		// #nosec G301 -- skill runtime state is user-managed project-local data.
		if err := os.MkdirAll(path, 0o777); err != nil {
			return err
		}
	}
	return ensureGitignoreEntries(filepath.Join(mutationRoot, ".gitignore"), []string{"/state/"})
}

// EnsureProjectLocal creates the ignored, untracked project-local
// overlay. It never writes inside the shared .agents tree. The required ignores
// entries are anchored so a nested project cannot accidentally hide a parent
// artifact with the same name.
func EnsureProjectLocal(root string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("root is required")
	}
	mutationRoot, err := safefs.ResolveRootForMutation(root)
	if err != nil {
		return err
	}
	for _, rel := range localDirectoryPaths() {
		path := filepath.Join(mutationRoot, rel)
		if err := safefs.RejectExistingSymlinkAncestors(path); err != nil {
			return err
		}
		// #nosec G301 -- project-local files are user-visible project overlays.
		if err := os.MkdirAll(path, 0o777); err != nil {
			return err
		}
	}
	if err := writeIfMissing(filepath.Join(mutationRoot, "AGENTS.local.md"), "# Local Agent Memory\n\nKeep private project guidance in this canonical local file and `.agents.local/`. Do not create or maintain shared memory in native agent locations. This file is intentionally ignored by Git.\n"); err != nil {
		return err
	}
	return ensureGitignoreEntries(filepath.Join(mutationRoot, ".gitignore"), []string{
		"/AGENTS.local.md",
		"/.agents.local/",
	})
}

// EnsureProjectLocalInRoot creates the project-local overlay through a
// caller-retained project root. It never resolves the selected project path,
// so layout creation remains in the filesystem generation protected by the
// caller's cooperative project lock.
func EnsureProjectLocalInRoot(root *os.Root) error {
	return EnsureProjectLocalInRootValidated(root, nil)
}

// EnsureProjectLocalInRootValidated is EnsureProjectLocalInRoot
// with a lock-generation validator at each rooted mutation boundary.
func EnsureProjectLocalInRootValidated(root *os.Root, validate func() error) error {
	if root == nil {
		return errors.New("root is required")
	}
	for _, rel := range localDirectoryPaths() {
		if err := safefs.RejectRootSymlinkAncestors(root, rel); err != nil {
			return err
		}
		if err := validateRootMutation(validate); err != nil {
			return err
		}
		// #nosec G301 -- project-local files are user-visible project overlays.
		if err := root.MkdirAll(rel, 0o777); err != nil {
			return err
		}
		if err := validateRootMutation(validate); err != nil {
			return err
		}
	}
	if err := writeRootIfMissingValidated(root, "AGENTS.local.md", "# Local Agent Memory\n\nKeep private project guidance in this canonical local file and `.agents.local/`. Do not create or maintain shared memory in native agent locations. This file is intentionally ignored by Git.\n", validate); err != nil {
		return err
	}
	return ensureGitignoreEntriesInRootValidated(root, ".gitignore", []string{
		"/AGENTS.local.md",
		"/.agents.local/",
	}, validate)
}

// EnsureSharedDirectories creates only the canonical shared-agent
// directories. Mutations that have already planned canonical file contents
// use this variant so layout bootstrapping cannot change a reviewed target
// between preflight and the actual write.
func EnsureSharedDirectories(root string) error {
	_, err := ensureSharedDirectories(root)
	return err
}

// EnsureSharedDirectoriesInRoot creates the canonical directories through a
// pinned rooted filesystem, preserving the selected project inode across
// concurrent path renames.
func EnsureSharedDirectoriesInRoot(root *os.Root) error {
	return EnsureSharedDirectoriesInRootValidated(root, nil)
}

// EnsureSharedDirectoriesInRootValidated is EnsureSharedDirectoriesInRoot
// with a lock-generation validator around every directory creation boundary.
func EnsureSharedDirectoriesInRootValidated(root *os.Root, validate func() error) error {
	if root == nil {
		return errors.New("root is required")
	}
	for _, rel := range sharedDirectoryPaths() {
		if err := safefs.RejectRootSymlinkAncestors(root, rel); err != nil {
			return err
		}
		if err := validateRootMutation(validate); err != nil {
			return err
		}
		// #nosec G301 -- shared knowledge directories are user-visible, non-secret project files.
		if err := root.MkdirAll(rel, 0o777); err != nil {
			return err
		}
		if err := validateRootMutation(validate); err != nil {
			return err
		}
	}
	return nil
}

func ensureSharedDirectories(root string) (string, error) {
	if root == "" {
		return "", errors.New("root is required")
	}
	mutationRoot, err := safefs.ResolveRootForMutation(root)
	if err != nil {
		return "", err
	}
	for _, rel := range sharedDirectoryPaths() {
		target := filepath.Join(mutationRoot, rel)
		if err := safefs.RejectExistingSymlinkAncestors(target); err != nil {
			return "", err
		}
		// #nosec G301 -- shared knowledge directories are user-visible, non-secret project files.
		if err := os.MkdirAll(target, 0o777); err != nil {
			return "", err
		}
	}
	return mutationRoot, nil
}

func sharedDirectoryPaths() []string {
	return []string{
		filepath.Join(".agents", "knowledge"),
		filepath.Join(".agents", "rules"),
		filepath.Join(".agents", "skills"),
		filepath.Join(".agents", "plugins"),
		filepath.Join(".agents", "adapters"),
	}
}

func localDirectoryPaths() []string {
	return []string{
		filepath.Join(".agents.local", "knowledge"),
		filepath.Join(".agents.local", "rules"),
		filepath.Join(".agents.local", "skills"),
		filepath.Join(".agents.local", "plugins"),
		filepath.Join(".agents.local", "adapters"),
	}
}

func projectSkillStateDirectoryPaths() []string {
	return []string{
		filepath.Join("state", "skills"),
	}
}

func ensureGitignoreEntries(path string, requiredEntries []string) error {
	if err := safefs.RejectExistingSymlinkAncestors(path); err != nil {
		return err
	}
	parentPath := filepath.Dir(path)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New(".gitignore parent is not a physical directory"), err)
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return err
	}
	defer root.Close()
	openedParent, err := root.Stat(".")
	if err != nil || !os.SameFile(parentInfo, openedParent) {
		return errors.Join(errors.New(".gitignore parent changed while it was opened"), err)
	}
	name := filepath.Base(path)
	return ensureGitignoreEntriesInRoot(root, name, requiredEntries)
}

func ensureGitignoreEntriesInRoot(root *os.Root, name string, requiredEntries []string) error {
	return ensureGitignoreEntriesInRootValidated(root, name, requiredEntries, nil)
}

func ensureGitignoreEntriesInRootValidated(root *os.Root, name string, requiredEntries []string, validateMutation func() error) error {
	if root == nil {
		return errors.New(".gitignore root is required")
	}
	if filepath.Clean(name) != filepath.Base(name) {
		return errors.New(".gitignore path must be a direct child of its retained root")
	}
	for attempt := 0; attempt < 8; attempt++ {
		body, info, exists, err := readGitignoreGeneration(root, name)
		if err != nil {
			return err
		}
		merged, changed := mergeGitignoreEntries(body, requiredEntries)
		if !changed {
			return nil
		}
		validate := func(currentRoot *os.Root, currentName string) error {
			if err := validateRootMutation(validateMutation); err != nil {
				return err
			}
			if beforeGitignoreCompare != nil {
				beforeGitignoreCompare()
			}
			currentBody, currentInfo, currentExists, err := readGitignoreGeneration(currentRoot, currentName)
			if err != nil {
				return err
			}
			if currentExists != exists || !bytes.Equal(currentBody, body) || (exists && !os.SameFile(info, currentInfo)) {
				return errGitignoreGenerationChanged
			}
			return nil
		}
		validateAfter := func(*os.Root, string) error { return validateRootMutation(validateMutation) }
		if err := safefs.AtomicWriteRootFileUsingUmaskTransaction(root, name, merged, 0o666, 0o777, validate, validateAfter); err == nil {
			return nil
		} else if !errors.Is(err, errGitignoreGenerationChanged) {
			return err
		}
	}
	return errors.New(".gitignore kept changing while required entries were merged")
}

func readGitignoreGeneration(root *os.Root, name string) ([]byte, os.FileInfo, bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, false, errors.New(".gitignore is not a regular file")
	}
	body, opened, err := backup.ReadRootRegularFileBoundedInfo(root, name, 8<<20)
	if err != nil {
		return nil, nil, false, err
	}
	return body, opened, true, nil
}

func mergeGitignoreEntries(body []byte, requiredEntries []string) ([]byte, bool) {
	merged := append([]byte(nil), body...)
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	have := map[string]bool{}
	for _, line := range lines {
		have[strings.TrimSpace(line)] = true
	}
	changed := false
	for _, required := range requiredEntries {
		if have[required] {
			continue
		}
		if len(merged) > 0 && !bytes.HasSuffix(merged, []byte("\n")) {
			merged = append(merged, '\n')
		}
		merged = append(merged, []byte(required+"\n")...)
		have[required] = true
		changed = true
	}
	return merged, changed
}

func writeIfMissing(path string, body string) error {
	return safefs.AtomicWriteFileIfMissingUsingUmask(path, []byte(body), 0o666, 0o777)
}

func writeRootIfMissingValidated(root *os.Root, name string, body string, validateMutation func() error) (returnErr error) {
	if root == nil {
		return errors.New("root is required")
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("bootstrap path escapes retained root: %s", name)
	}
	if err := safefs.RejectRootSymlinkAncestors(root, clean); err != nil {
		return err
	}
	if err := validateRootMutation(validateMutation); err != nil {
		return err
	}
	file, err := root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if errors.Is(err, os.ErrExist) {
		info, inspectErr := root.Lstat(clean)
		if inspectErr != nil {
			return inspectErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("bootstrap path is not a regular file: %s", name)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	created, err := file.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.Join(fmt.Errorf("bootstrap file identity could not be established: %s", name), err)
	}
	if _, err := file.WriteString(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	current, err := root.Lstat(clean)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(created, current) {
		return errors.Join(fmt.Errorf("bootstrap file changed while it was created: %s", name), err)
	}
	return validateRootMutation(validateMutation)
}

func validateRootMutation(validate func() error) error {
	if validate == nil {
		return nil
	}
	return validate()
}

// IsProjectLocalPath reports whether a project-relative canonical path belongs
// to the local overlay, including its guide beside the overlay directory.
func IsProjectLocalPath(path string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	return path == "AGENTS.local.md" || path == ".agents.local" || strings.HasPrefix(path, ".agents.local/")
}
