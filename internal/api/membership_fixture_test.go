package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/tiermetric/tier/internal/store"
)

// testStorePaths maps each test *store.DB to its file, so a fixture can reach the
// raw schema when it must model a state no API can produce.
var testStorePaths sync.Map // *store.DB -> string

func registerTestStore(db *store.DB, path string) { testStorePaths.Store(db, path) }

// baselineHierarchy places a developer the way an install that upgraded to
// dated membership holds a PRE-EXISTING assignment (#886): org_hierarchy and
// period_membership exactly as UpsertHierarchy writes them, plus ONE open
// hierarchy_membership row valid from store.MembershipBaselineFrom — what
// backfillHierarchyMembership stamps. Fixtures that seed events in the past and
// test the fold, the floor or the residual use it; UpsertHierarchy alone would
// (correctly) leave every event that predates the call in "other".
//
// No API produces this row: every write through the store stamps the server
// clock. The fixture writes it over a raw connection, after deleting the
// at-now row UpsertHierarchy just appended (the schema admits DELETE, for
// erasure), so it is only a fixture's shortcut to the upgraded state.
func baselineHierarchy(db *store.DB, ctx context.Context, developer, team, division, org string) error {
	if err := db.UpsertHierarchy(ctx, developer, team, division, org, "test:fixture"); err != nil {
		return err
	}
	p, ok := testStorePaths.Load(db)
	if !ok {
		return fmt.Errorf("baselineHierarchy: store not registered (open it through newTestHandler* or call registerTestStore)")
	}
	raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, `DELETE FROM hierarchy_membership WHERE developer = ?`, developer); err != nil {
		return err
	}
	_, err = raw.ExecContext(ctx, `
		INSERT INTO hierarchy_membership (developer, team, division, valid_from, valid_to, written_by)
		VALUES (?, ?, ?, ?, NULL, ?)`,
		developer, team, division, store.MembershipBaselineFrom, store.MembershipWriterBaseline)
	return err
}

// baselineAlias maps alias to canonical the way an install that upgraded
// through #914 holds a PRE-EXISTING alias: the alias and the canonical each
// carry the person's placement from store.MembershipBaselineFrom, as the #914
// migration (copyAliasMembershipHistory) leaves a legacy alias whose person had
// one baselined placement. Call it after the person's baselineHierarchy. The
// alias write itself appends rows at the server clock — so an alias made
// through the API never places events that predate it — and this fixture
// replaces those rows over a raw connection, exactly as baselineHierarchy does.
func baselineAlias(db *store.DB, ctx context.Context, alias, canonical string) error {
	if err := db.UpsertDeveloperAlias(ctx, alias, canonical, "test:fixture"); err != nil {
		return err
	}
	p, ok := testStorePaths.Load(db)
	if !ok {
		return fmt.Errorf("baselineAlias: store not registered (open it through newTestHandler* or call registerTestStore)")
	}
	raw, err := sql.Open("sqlite", rawTestDSN(p.(string)))
	if err != nil {
		return err
	}
	defer func() { _ = raw.Close() }()
	for _, id := range []string{alias, canonical} {
		var team, division string
		err := raw.QueryRowContext(ctx, `SELECT team, division FROM hierarchy_membership
			WHERE developer = ? AND valid_to IS NULL`, id).Scan(&team, &division)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := raw.ExecContext(ctx, `DELETE FROM hierarchy_membership WHERE developer = ?`, id); err != nil {
			return err
		}
		if _, err := raw.ExecContext(ctx, `
			INSERT INTO hierarchy_membership (developer, team, division, valid_from, valid_to, written_by)
			VALUES (?, ?, ?, ?, NULL, ?)`,
			id, team, division, store.MembershipBaselineFrom, store.MembershipWriterAliasHistory); err != nil {
			return err
		}
	}
	return nil
}
