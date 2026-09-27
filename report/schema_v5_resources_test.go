package report

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestSchemaV5ResourceSummaryCoversEveryField keeps report/schema_v5.json in
// step with the ResourceSummary struct, both ways (probatorium#411 review):
// $defs.resourceSummary does not set additionalProperties:false, so a field
// the Go struct gains and the JSON schema lacks still validates, and nothing
// else notices the drift. Every json-tagged field must be a schema property
// of the matching JSON type ("integer" for an int64, "number" for a float64)
// and nullable (every summary metric is a pointer, omitted or null when
// absent); every schema property must name a field.
func TestSchemaV5ResourceSummaryCoversEveryField(t *testing.T) {
	raw, err := os.ReadFile("schema_v5.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Type any `json:"type"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	props := doc.Defs["resourceSummary"].Properties
	if len(props) == 0 {
		t.Fatal("schema_v5.json has no $defs.resourceSummary.properties: the guard would judge nothing")
	}
	rt := reflect.TypeFor[ResourceSummary]()
	fields := map[string]bool{}
	for i := range rt.NumField() {
		f := rt.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = true
		p, ok := props[name]
		if !ok {
			t.Errorf("ResourceSummary.%s (json %q) is missing from schema_v5.json $defs.resourceSummary", f.Name, name)
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		want := ""
		switch ft.Kind() {
		case reflect.Int, reflect.Int32, reflect.Int64:
			want = "integer"
		case reflect.Float32, reflect.Float64:
			want = "number"
		default:
			t.Errorf("ResourceSummary.%s: unexpected Go type %s; teach this guard its JSON type", f.Name, f.Type)
			continue
		}
		var types []string
		switch v := p.Type.(type) {
		case string:
			types = []string{v}
		case []any:
			for _, e := range v {
				if s, ok := e.(string); ok {
					types = append(types, s)
				}
			}
		}
		if !slices.Contains(types, want) || !slices.Contains(types, "null") {
			t.Errorf("schema_v5.json resourceSummary.%s type %v, want [%q, \"null\"] for Go %s", name, p.Type, want, f.Type)
		}
	}
	if len(fields) == 0 {
		t.Fatal("ResourceSummary has no json-tagged fields: the guard would judge nothing")
	}
	for name := range props {
		if !fields[name] {
			t.Errorf("schema_v5.json resourceSummary.%s names no ResourceSummary field", name)
		}
	}
}
