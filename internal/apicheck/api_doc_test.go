// SPDX-License-Identifier: Apache-2.0

package apicheck_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestProductionGoPackagesAreDocumented(t *testing.T) {
	root := moduleRoot(t)

	packageDocs := make(map[string]bool)
	productionPackages := make(map[string]bool)
	var failures []string
	err := walkProductionGoFiles(root, func(path string) error {
		dir := filepath.Dir(path)
		productionPackages[dir] = true
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if !lacksUsefulDocumentation(file.Name.Name, file.Doc) {
			packageDocs[dir] = true
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if externallyNamedFunction(declaration) && lacksUsefulDocumentation(declaration.Name.Name, declaration.Doc) {
					failures = append(failures, rel+":"+declaration.Name.Name+" lacks useful documentation")
				}
			case *ast.GenDecl:
				if declaration.Tok != token.TYPE && declaration.Tok != token.CONST && declaration.Tok != token.VAR {
					continue
				}
				for _, spec := range declaration.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						if spec.Name.IsExported() && lacksUsefulDocumentation(spec.Name.Name, spec.Doc, declaration.Doc) {
							failures = append(failures, rel+":"+spec.Name.Name+" lacks useful documentation")
						}
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							if name.IsExported() && lacksUsefulDocumentation(name.Name, spec.Doc, declaration.Doc) {
								failures = append(failures, rel+":"+name.Name+" lacks useful documentation")
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for dir := range productionPackages {
		if packageDocs[dir] {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		failures = append(failures, rel+": package lacks documentation")
	}
	sort.Strings(failures)
	if len(failures) > 0 {
		t.Fatalf("production Go documentation contract failed:\n%s", strings.Join(failures, "\n"))
	}
}

func TestDocumentationContractDoesNotBorrowSiblingComments(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", `package fixture
const (
	Alpha = 1
	// Beta documents the second value.
	Beta = 2
)`, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	declaration := file.Decls[0].(*ast.GenDecl)
	alpha := declaration.Specs[0].(*ast.ValueSpec)
	beta := declaration.Specs[1].(*ast.ValueSpec)
	if !lacksUsefulDocumentation("Alpha", alpha.Doc, declaration.Doc) {
		t.Fatal("undocumented Alpha borrowed Beta's later comment")
	}
	if lacksUsefulDocumentation("Beta", beta.Doc, declaration.Doc) {
		t.Fatal("documented Beta was rejected")
	}
}

func TestDocumentationContractRejectsKnownPlaceholders(t *testing.T) {
	for _, comment := range []string{
		"// TODO: document Alpha.",
		"// See the design doc.",
		"// Alpha is a type.",
		"// Alpha is part of the fixture package API.",
		"// #nosec G101 -- not documentation.",
	} {
		doc := &ast.CommentGroup{List: []*ast.Comment{{Text: comment}}}
		if !lacksUsefulDocumentation("Alpha", doc) {
			t.Errorf("placeholder accepted: %s", comment)
		}
	}
}

func lacksUsefulDocumentation(name string, docs ...*ast.CommentGroup) bool {
	for _, doc := range docs {
		if !missingOrPlaceholder(name, doc) {
			return false
		}
	}
	return true
}

func missingOrPlaceholder(name string, doc *ast.CommentGroup) bool {
	if doc == nil || strings.TrimSpace(doc.Text()) == "" {
		return true
	}
	text := strings.ToLower(strings.TrimSpace(doc.Text()))
	stem := strings.TrimSuffix(text, ".")
	if strings.HasPrefix(stem, "todo") || strings.HasPrefix(stem, "fixme") ||
		strings.HasPrefix(stem, "#nosec") || strings.HasPrefix(stem, "nolint") ||
		stem == "see the design doc" || stem == "see documentation" || stem == "documentation" {
		return true
	}
	if strings.Contains(stem, " is part of the ") && strings.Contains(stem, " package api") {
		return true
	}
	symbol := strings.ToLower(name)
	for _, filler := range []string{" is a type", " is a function", " is a variable", " is a constant"} {
		if stem == symbol+filler {
			return true
		}
	}
	return false
}

func externallyNamedFunction(declaration *ast.FuncDecl) bool {
	if !declaration.Name.IsExported() {
		return false
	}
	if declaration.Recv == nil {
		return true
	}
	if len(declaration.Recv.List) == 0 {
		return false
	}
	receiver := declaration.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	identifier, ok := receiver.(*ast.Ident)
	return ok && identifier.IsExported()
}

func walkProductionGoFiles(root string, visit func(string) error) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".agents", ".tmp", ".ua", "docs", "graphify-out", "reviews", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		return visit(path)
	})
}
