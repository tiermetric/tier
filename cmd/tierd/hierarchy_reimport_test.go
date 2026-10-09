package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestHierarchyImportJSONTagsMatchAPI(t *testing.T) {
	// api.hierarchyBulkItem is unexported. Parse its source tags and compare
	// their JSON names with reflection on the CLI type; no production helper.
	f, err := parser.ParseFile(token.NewFileSet(), "../../internal/api/hierarchy.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var apiTags map[string]bool
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "hierarchyBulkItem" {
			return true
		}
		apiTags = map[string]bool{}
		for _, field := range ts.Type.(*ast.StructType).Fields.List {
			if field.Tag == nil {
				t.Fatal("API field missing JSON tag")
			}
			tag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				t.Fatal(err)
			}
			apiTags[strings.Split(reflect.StructTag(tag).Get("json"), ",")[0]] = true
		}
		return false
	})
	cliTags := map[string]bool{}
	item := hierarchyImportItem{Developer: "alice", Team: "eng", Division: "platform", Org: "acme", Rejoin: true}
	typ := reflect.TypeOf(item)
	for i := 0; i < typ.NumField(); i++ {
		cliTags[strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	if !reflect.DeepEqual(cliTags, apiTags) {
		t.Fatalf("CLI JSON tags=%v API JSON tags=%v", cliTags, apiTags)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"rejoin":true`) {
		t.Fatalf("rejoin missing from marshaled CLI item: %s", encoded)
	}
}

func TestHierarchyRejoinCSV(t *testing.T) {
	for _, value := range []string{"", "TrUe", "FALSE", "yes", "1"} {
		t.Run(value, func(t *testing.T) {
			rows, err := readHierarchyCSV(strings.NewReader("developer,team,division,org,REJOIN\nalice,eng,,acme," + value + "\n"))
			if value == "yes" || value == "1" {
				if err == nil || !strings.Contains(err.Error(), "rejoin must be true or false") {
					t.Fatalf("invalid rejoin: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(rows[0].item)
			if err != nil {
				t.Fatal(err)
			}
			var item map[string]any
			if err := json.Unmarshal(b, &item); err != nil {
				t.Fatal(err)
			}
			if got, _ := item["rejoin"].(bool); got != strings.EqualFold(value, "true") {
				t.Fatalf("rejoin=%v", item)
			}
		})
	}
}

func TestHierarchyReimportReport(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"accepted":2,"kept_departed":[{"developer":"alice","org":"acme"}],"reseated_unknown":[{"developer":"bob","org":"acme"}]}`)
	}))
	defer s.Close()
	p := writeHierarchyCSV(t, "developer,team,division,org\nalice,eng,,acme\nbob,eng,,acme\n")
	code, out, errs := runHierarchyForTest("import", "--server", s.URL, p)
	if code != 0 {
		t.Fatalf("code=%d: %s", code, errs)
	}
	for _, want := range []string{`kept-departed "alice"`, `rejoin=true`, `re-seated "bob"`, `"acme"`, `end them again`} {
		if !strings.Contains(out+errs, want) {
			t.Errorf("missing %q in %s%s", want, out, errs)
		}
	}
}

func TestHierarchyReimportReportBound(t *testing.T) {
	members := make([]map[string]string, 101)
	for i := range members {
		members[i] = map[string]string{"developer": fmt.Sprintf("person-%03d", i), "org": "acme"}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": 1, "kept_departed": members, "reseated_unknown": members, "kept_departed_count": 123, "reseated_unknown_count": 124})
	}))
	defer s.Close()
	p := writeHierarchyCSV(t, "developer,team,division,org\nalice,eng,,acme\n")
	code, out, errs := runHierarchyForTest("import", "--server", s.URL, p)
	if code != 0 || strings.Contains(out, "person-100") || strings.Count(out, `kept-departed "`) != 100 || strings.Count(out, `re-seated "`) != 100 || !strings.Contains(out, "123") || !strings.Contains(out, "124") {
		t.Fatalf("code=%d output=%s errors=%s", code, out, errs)
	}
}
