package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestInspectIssueIDCountsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tier.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"UTF-8", "UTF-8", "PROJ-9"} {
		if _, err := db.InsertOutcome(context.Background(), Outcome{Developer: "alice", IssueID: id, Weight: 1, Quality: 1, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int{"UTF-8": 2, "PROJ-9": 1}
	counts, err := db.IssueIDCounts(context.Background())
	if err != nil || !reflect.DeepEqual(counts, want) {
		t.Fatal(counts, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	counts, err = InspectIssueIDCounts(context.Background(), path)
	if err != nil || !reflect.DeepEqual(counts, want) {
		t.Fatal(counts, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("inspection changed database", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := InspectIssueIDCounts(context.Background(), missing); err == nil {
		t.Fatal("missing DB accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("inspection created DB", err)
	}
}
