package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Identities is the set of distinct developer identifiers and org names a
// database holds. Returned by InspectIdentities so a caller can classify a
// database without depending on store internals.
type Identities struct {
	Developers []string
	Orgs       []string
}

// InspectIdentities opens the SQLite database at path READ-ONLY and returns the
// distinct developer identifiers and org names it contains across every table
// that carries them. It runs NO migration and issues NO write: the connection is
// opened mode=ro (an OS-level read-only open), so inspecting a real capture
// database can never delete, migrate, or otherwise mutate its contents — the
// #475 safety guarantee.
//
// mode=ro (deliberately NOT immutable=1) still honors a live -wal so a demo
// database Ctrl-C'd with an uncheckpointed WAL is read correctly; immutable=1
// would ignore the WAL and could misread a real database as empty, so it is
// unsafe here. Reading a WAL database rebuilds the ephemeral -shm shared-memory
// index exactly as any reader does — that is not a mutation of the database and
// SQLite recreates it on every open.
//
// It exists so `tierd demo` can refuse to recreate a database that already holds
// real (non-demo) data (#475). A file that is not a readable SQLite database — a
// non-SQLite file, a corrupt one, or a schema missing an expected table —
// returns a non-nil error, which the caller must treat as "cannot confirm this
// is a throwaway demo database" and refuse to touch (fail-closed). An empty tier
// database returns empty slices and a nil error.
//
// Bare-path DSN (not a file: URI) matches Open's convention, including its
// unsupported '?'-in-path caveat.
func InspectIdentities(ctx context.Context, path string) (Identities, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return Identities{}, fmt.Errorf("open read-only: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Union across every table that stores a developer identity so ANY real row —
	// a captured cost event, an attributed outcome, an org-hierarchy seat, a
	// finance invoice, or an alias mapping — is seen and makes the database
	// non-demo. A missing table (an unexpected/foreign schema) surfaces as a query
	// error and is refused upstream, which is the safe direction.
	developers, err := distinctStrings(ctx, db, `
		SELECT developer FROM token_events
		UNION SELECT developer FROM outcomes
		UNION SELECT developer FROM org_hierarchy
		UNION SELECT developer FROM period_membership
		UNION SELECT developer FROM actual_spend
		UNION SELECT alias FROM developer_alias
		UNION SELECT canonical FROM developer_alias`)
	if err != nil {
		return Identities{}, fmt.Errorf("read developers: %w", err)
	}
	// org is nullable on org_hierarchy (a developer may be seeded without an org),
	// so skip NULLs to keep the scan into a plain string valid; org on
	// period_membership and org_actual_spend is NOT NULL.
	orgs, err := distinctStrings(ctx, db, `
		SELECT org FROM org_hierarchy WHERE org IS NOT NULL
		UNION SELECT org FROM period_membership
		UNION SELECT org FROM org_actual_spend`)
	if err != nil {
		return Identities{}, fmt.Errorf("read orgs: %w", err)
	}
	return Identities{Developers: developers, Orgs: orgs}, nil
}

// PopulatedTablesOutside opens the SQLite database at path READ-ONLY and returns
// the names of user tables that hold at least one row and are NOT in allow.
//
// It is the fail-closed half of the #475 demo-database guard. InspectIdentities
// only sees tables that carry a developer/org column, so a real deployment whose
// only rows live in a table without those columns — e.g. webhook_payloads, the
// GitHub audit trail written before any capture or outcome — would look empty to
// an identity scan and be wrongly classified as a throwaway demo. Enumerating
// EVERY user table from sqlite_master and reporting any populated one outside the
// demo seeder's own table set closes that gap for present AND future tables: a
// table added to the schema later is protected by default until it is explicitly
// allow-listed, rather than silently invisible.
//
// allow is the set of tables the demo seeder itself populates; tier_migrations
// (schema bookkeeping) and sqlite_* internal tables are always ignored. Same
// mode=ro / no-mutation guarantee as InspectIdentities. A non-SQLite or corrupt
// file returns a non-nil error, which the caller treats as "cannot confirm" and
// refuses (fail-closed).
func PopulatedTablesOutside(ctx context.Context, path string, allow []string) ([]string, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open read-only: %w", err)
	}
	defer func() { _ = db.Close() }()

	allowed := map[string]bool{"tier_migrations": true}
	for _, t := range allow {
		allowed[t] = true
	}

	tables, err := distinctStrings(ctx, db, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("enumerate tables: %w", err)
	}

	var populated []string
	for _, t := range tables {
		if allowed[t] {
			continue
		}
		// A quoted identifier is required — table names come from sqlite_master,
		// not user input, but the schema could add a table whose name needs it.
		var has int
		row := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %q LIMIT 1)`, t))
		if err := row.Scan(&has); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		if has == 1 {
			populated = append(populated, t)
		}
	}
	return populated, nil
}

// distinctStrings runs a single-column query and collects the non-NULL rows.
func distinctStrings(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
