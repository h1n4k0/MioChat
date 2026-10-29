package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Catalog totals alone are not a complete TUI inventory: aliases and nested
// command routes must remain mapped even when they are absent from the menu.
func TestGUIParityInventoryIncludesEveryTUICommandCase(t *testing.T) {
	data, err := os.ReadFile("gpui-client/TUI_PARITY.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		CatalogCount int `json:"catalog_count"`
		Rows         []struct{ Command, Group, Status string }
		Summary      struct {
			Catalog    int `json:"catalog_candidates"`
			Additional int `json:"additional_command_candidates"`
		} `json:"coverage_summary"`
	}
	if err = json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	mapped := map[string]bool{}
	catalog := map[string]bool{}
	for _, entry := range commandCatalog {
		catalog[entry.Name] = true
	}
	ready, extra := 0, 0
	for _, row := range inventory.Rows {
		if mapped[row.Command] {
			t.Fatal("duplicate mapping", row.Command)
		}
		mapped[row.Command] = true
		if catalog[row.Command] {
			if row.Group == "ADDITIONAL" {
				t.Fatal("catalog mislabeled", row.Command)
			}
			if !strings.HasPrefix(row.Status, "缺失") {
				ready++
			}
		} else {
			if row.Group != "ADDITIONAL" {
				t.Fatal("unknown catalog command", row.Command)
			}
			if !strings.HasPrefix(row.Status, "缺失") {
				extra++
			}
		}
	}
	if inventory.CatalogCount != len(catalog) || ready != inventory.Summary.Catalog || extra != inventory.Summary.Additional {
		t.Fatal("coverage totals differ from actual mapping", len(catalog), ready, extra, inventory.Summary)
	}
	for name := range catalog {
		if !mapped[name] {
			t.Fatal("unmapped catalog entry", name)
		}
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "gui_") {
			continue
		}
		file, e := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if e != nil {
			t.Fatal(e)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expression := range clause.List {
				literal, ok := expression.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, e := strconv.Unquote(literal.Value)
				if e == nil && strings.HasPrefix(value, "/") && !mapped[value] {
					t.Errorf("unmapped TUI command %s in %s", value, name)
				}
			}
			return true
		})
	}
}
