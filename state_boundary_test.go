//go:build darwin || linux

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPersistenceBoundary(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "state" && path != filepath.Join("internal", "state")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		inside := strings.HasPrefix(path, filepath.Join("internal", "state")+string(filepath.Separator))
		sqlNames := map[string]bool{}
		for _, imp := range file.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if name == "database/sql" || name == "modernc.org/sqlite" {
				if !inside {
					t.Errorf("%s imports persistence internals: %s", path, name)
				}
				if name == "database/sql" {
					alias := "sql"
					if imp.Name != nil {
						alias = imp.Name.Name
					}
					sqlNames[alias] = true
				}
			}
		}
		checkType := func(node ast.Node) {
			ast.Inspect(node, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && sqlNames[id.Name] {
						t.Errorf("%s exposes database/sql in its public contract", path)
					}
				}
				return true
			})
		}
		if inside {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.IsExported() {
					checkType(fn.Type)
				}
				if gen, ok := decl.(*ast.GenDecl); ok {
					for _, spec := range gen.Specs {
						if typ, ok := spec.(*ast.TypeSpec); ok && typ.Name.IsExported() {
							if record, ok := typ.Type.(*ast.StructType); ok {
								for _, field := range record.Fields.List {
									for _, name := range field.Names {
										if name.IsExported() {
											checkType(field.Type)
										}
									}
								}
							} else {
								checkType(typ.Type)
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
}
