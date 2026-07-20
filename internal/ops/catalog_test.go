package ops

import (
	"reflect"
	"sort"
	"testing"
)

// Behavior: every registered op appears in the catalog with a description,
// and every exported arg field is documented. A new op that forgets its
// documentation fails this test (the catalog is the schema's data source).
func TestCatalogCoversRegistry(t *testing.T) {
	cat := Catalog()
	byName := map[string]OpInfo{}
	for _, info := range cat {
		byName[info.Name] = info
	}
	if len(cat) != len(registry) {
		t.Fatalf("catalog has %d entries, registry has %d", len(cat), len(registry))
	}
	for name := range registry {
		info, ok := byName[name]
		if !ok {
			t.Errorf("op %q missing from Catalog()", name)
			continue
		}
		if info.Doc == "" {
			t.Errorf("op %q has an empty Doc", name)
		}
		if info.Args == nil {
			t.Errorf("op %q has a nil Args instance", name)
			continue
		}
		if got, want := reflect.TypeOf(info.Args), reflect.TypeOf(registry[name]()); got != want {
			t.Errorf("op %q Args type %v, registry constructs %v", name, got, want)
		}
		st := reflect.TypeOf(info.Args).Elem()
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}
			if info.FieldDocs[f.Name] == "" {
				t.Errorf("op %q field %q has no description in FieldDocs", name, f.Name)
			}
		}
		for docField := range info.FieldDocs {
			if _, ok := st.FieldByName(docField); !ok {
				t.Errorf("op %q documents unknown field %q", name, docField)
			}
		}
	}
}

// Behavior: the catalog is sorted by op name, so downstream artifacts
// (JSON Schema, docs) are byte-stable.
func TestCatalogSortedByName(t *testing.T) {
	cat := Catalog()
	names := make([]string, len(cat))
	for i, info := range cat {
		names[i] = info.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("catalog not sorted: %v", names)
	}
}

// Behavior: Args is a fresh zero value and Defaults is the same type with
// documented defaults applied — callers may mutate either without
// corrupting the catalog.
func TestCatalogInstancesAreFresh(t *testing.T) {
	first := Catalog()
	for _, info := range first {
		if hc, ok := info.Args.(*Healthcheck); ok {
			hc.Retries = 999
		}
	}
	for _, info := range Catalog() {
		if hc, ok := info.Args.(*Healthcheck); ok && hc.Retries != 0 {
			t.Fatalf("Catalog() Args instance is shared: %+v", hc)
		}
		if hc, ok := info.Defaults.(*Healthcheck); ok {
			if hc.Expect != 200 || hc.Retries != 5 {
				t.Fatalf("Defaults not applied: %+v", hc)
			}
		}
	}
}
