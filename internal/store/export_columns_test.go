package store

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// exportColumnExclusions is the explicit allowlist of stored columns the GDPR
// Art. 15 export deliberately does NOT carry, keyed table -> column -> reason.
// Every other stored column is exported (#805). An entry here must be matched by
// a named exclusion, with the same reason, in the comment above the Export* row
// types in store.go — a DSAR that silently omits a stored column is a
// data-subject access answer presented as complete when it is not.
var exportColumnExclusions = map[string]map[string]string{
	"hierarchy_membership": {
		"written_by": "a fingerprint of the operator's write credential, not data about the subject (#886)",
	},
	"push_outcome_audit": {
		"commit_developer": "the identity of another data subject, kept only so that author's erasure can blank commit_sha (#849)",
	},
}

// developerColumnExclusions is the explicit allowlist of tables that carry a
// `developer` column yet are deliberately NOT in developerPIITables, keyed
// table -> reason. It is EMPTY: measured, the tables with a developer column are
// exactly the ones in developerPIITables. A table added with a developer column
// and listed in neither place escapes both erase and export.
var developerColumnExclusions = map[string]string{}

// exportedTables is every table the DSAR export discloses: the erasure/export
// allowlist plus developer_alias, which developerPIITables' comment names as
// handled separately but equally covered by export, plus watcher_checkpoint,
// which is found by a session-id join rather than a developer column (#919).
func exportedTables() []string {
	return append(append([]string{}, developerPIITables...), "developer_alias", "watcher_checkpoint")
}

// jsonFieldNames returns the JSON names of t's exported fields.
func jsonFieldNames(t *testing.T, typ reflect.Type) map[string]bool {
	t.Helper()
	out := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s.%s has no JSON name: every export field must be disclosed under a column name",
				typ.Name(), typ.Field(i).Name)
		}
		out[name] = true
	}
	return out
}

// exportRowTypes maps each DeveloperExport JSON key (which is a table name) to
// the element type of its slice, so the table -> row-type binding is read from
// the artifact itself rather than from a second hand-maintained registry.
func exportRowTypes(t *testing.T) map[string]reflect.Type {
	t.Helper()
	typ := reflect.TypeOf(DeveloperExport{})
	out := map[string]reflect.Type{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Slice || f.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out[name] = f.Type.Elem()
	}
	return out
}

// exportSelectColumns parses ExportDeveloper's source in store.go and returns,
// per table, the column names its SELECT reads. A COALESCE(col, default)
// expression counts as reading col. The source is bounded to ExportDeveloper's
// body the same way TestExportPlanFixtureMatchesTheRealExportSQL bounds it.
func exportSelectColumns(t *testing.T) map[string][]string {
	t.Helper()
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (d *DB) ExportDeveloper(")
	if start < 0 {
		t.Fatal("ExportDeveloper not found in store.go: the SELECT column check cannot run")
	}
	end := strings.Index(body[start:], "\n// queryRows runs one read")
	if end < 0 {
		t.Fatal("end of ExportDeveloper's body not found in store.go: an unbounded parse would read other methods' SELECTs")
	}
	fnSrc := body[start : start+end]

	selectRe := regexp.MustCompile(`(?s)SELECT (.*?)\s+FROM (\w+) WHERE`)
	coalesceRe := regexp.MustCompile(`^COALESCE\((\w+),`)
	identRe := regexp.MustCompile(`^\w+$`)
	out := map[string][]string{}
	for _, m := range selectRe.FindAllStringSubmatch(fnSrc, -1) {
		table := m[2]
		if _, dup := out[table]; dup {
			t.Fatalf("ExportDeveloper has two SELECTs FROM %s: the column check cannot tell which one the export uses", table)
		}
		// Top-level comma split: commas inside COALESCE(...) are not separators.
		var items []string
		depth, from := 0, 0
		list := m[1]
		for i, r := range list {
			switch r {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					items = append(items, list[from:i])
					from = i + 1
				}
			}
		}
		items = append(items, list[from:])
		for _, it := range items {
			it = strings.TrimSpace(it)
			if c := coalesceRe.FindStringSubmatch(it); c != nil {
				out[table] = append(out[table], c[1])
				continue
			}
			if !identRe.MatchString(it) {
				t.Fatalf("%s: cannot parse SELECT item %q as a column name or COALESCE(column, …)", table, it)
			}
			out[table] = append(out[table], it)
		}
	}
	return out
}

// storedColumns reads PRAGMA table_info for table on a freshly migrated DB.
func storedColumns(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(),
		`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan pragma_table_info(%s): %v", table, err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pragma_table_info(%s): %v", table, err)
	}
	return cols
}

// TestExportDeveloper_CarriesEveryStoredColumn pins that the DSAR export's row
// types disclose every stored column of every exported table (#805): a column
// added to a PII table without an export field (or an allowlist entry) fails
// here, and so does an export field that names no stored column, since the
// export's JSON names ARE the column names.
func TestExportDeveloper_CarriesEveryStoredColumn(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	rowTypes := exportRowTypes(t)
	selectCols := exportSelectColumns(t)
	checked := 0
	for _, table := range exportedTables() {
		typ, ok := rowTypes[table]
		if !ok {
			t.Errorf("table %q holds personal data but DeveloperExport has no %q slice: the DSAR does not disclose it at all", table, table)
			continue
		}
		cols := storedColumns(t, db, table)
		if len(cols) == 0 {
			t.Errorf("pragma_table_info(%q) returned no columns: the table does not exist, so this guard compared nothing", table)
			continue
		}
		fields := jsonFieldNames(t, typ)
		stored := make(map[string]bool, len(cols))
		var missing []string
		for _, c := range cols {
			stored[c] = true
			if fields[c] {
				continue
			}
			if _, excluded := exportColumnExclusions[table][c]; excluded {
				continue
			}
			missing = append(missing, c)
		}
		if len(missing) > 0 {
			t.Errorf("%s: stored column(s) %v are missing from %s — add them to the export struct AND its SELECT in ExportDeveloper, or name the exclusion and its reason in exportColumnExclusions and in store.go's Export* comment",
				table, missing, typ.Name())
		}
		var phantom []string
		for f := range fields {
			if !stored[f] {
				phantom = append(phantom, f)
			}
		}
		sort.Strings(phantom)
		if len(phantom) > 0 {
			t.Errorf("%s: %s JSON field(s) %v name no stored column — the export's JSON names must equal the column names", table, typ.Name(), phantom)
		}
		// The struct can declare a field the SELECT never reads; it then exports
		// a zero value as if it were stored. Pin the SELECT itself.
		sel, ok := selectCols[table]
		if !ok {
			t.Errorf("%s: no SELECT … FROM %s WHERE found in ExportDeveloper", table, table)
		}
		selected := make(map[string]bool, len(sel))
		for _, c := range sel {
			selected[c] = true
		}
		var unselected []string
		for _, c := range cols {
			if _, excluded := exportColumnExclusions[table][c]; !excluded && !selected[c] {
				unselected = append(unselected, c)
			}
		}
		if ok && len(unselected) > 0 {
			t.Errorf("%s: stored column(s) %v are declared on %s but ExportDeveloper's SELECT does not read them, so the export carries a zero value instead of the stored one — add them to the SELECT and its Scan",
				table, unselected, typ.Name())
		}
		for c, reason := range exportColumnExclusions[table] {
			if !stored[c] {
				t.Errorf("exportColumnExclusions[%q][%q] names a column that is not stored: remove the stale entry", table, c)
			}
			if fields[c] {
				t.Errorf("exportColumnExclusions[%q][%q] is exported anyway: remove the stale entry", table, c)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("exportColumnExclusions[%q][%q] has no reason", table, c)
			}
		}
		checked += len(cols)
	}
	if checked == 0 {
		t.Fatal("no stored columns were compared: this guard checked nothing")
	}
	if len(selectCols) != len(exportedTables()) {
		t.Errorf("parsed %d SELECTs from ExportDeveloper, want %d (one per exported table): %v", len(selectCols), len(exportedTables()), selectCols)
	}

	// Every table carrying a `developer` column must be in developerPIITables
	// (so it is erased AND exported) or a declared exclusion.
	rows, err := db.db.QueryContext(context.Background(),
		`SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) p
		 WHERE m.type = 'table' AND p.name = 'developer' ORDER BY m.name`)
	if err != nil {
		t.Fatalf("list tables with a developer column: %v", err)
	}
	defer func() { _ = rows.Close() }()
	inPII := map[string]bool{}
	for _, tb := range developerPIITables {
		inPII[tb] = true
	}
	withDeveloper := 0
	for rows.Next() {
		var tb string
		if err := rows.Scan(&tb); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		withDeveloper++
		if _, excluded := developerColumnExclusions[tb]; !inPII[tb] && !excluded {
			t.Errorf("table %q has a developer column but is neither in developerPIITables nor in developerColumnExclusions: its rows escape both erasure and the DSAR export", tb)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	if withDeveloper == 0 {
		t.Fatal("found no table with a developer column: the PII-table completeness check compared nothing")
	}
}
