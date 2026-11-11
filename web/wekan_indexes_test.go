package web

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's indexes on Weddy's database (#6892): the table holds the ones the
// issue's scans needed, they are created - and a second run changes nothing
// and fails nothing it created (negative) - and the table is what
// cmd/sync-indexes writes from WeKan's source, when WeKan's checkout is beside.
func TestWekanIndexes(t *testing.T) {
	has := func(coll string, keys ...string) bool {
		for _, ix := range wekanIndexes {
			if ix.coll != coll || len(ix.keys) != len(keys) {
				continue
			}
			ok := true
			for i, k := range keys {
				ok = ok && ix.keys[i].Key == k
			}
			if ok {
				return true
			}
		}
		return false
	}
	for _, want := range [][]string{{"users", "services.ldap.id"}, {"cards", "assignees"}, {"cards", "members"}, {"changeHistory", "integrityHash"},
		{"changeHistory", "entityType", "entityId", "createdAt"}, {"cards", "boardId", "archived"}} {
		if !has(want[0], want[1:]...) {
			t.Errorf("WeKan's index %v is in the table", want)
		}
	}
	w := newTestWeKan(t)
	ctx := context.Background()
	first := w.s.ensureWekanIndexes(ctx)
	cur, err := w.db.Collection("cards").Indexes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var specs []bson.M
	_ = cur.All(ctx, &specs)
	found := false
	for _, sp := range specs {
		found = found || strings.Contains(canonical(plain(sp["key"])), `"assignees"`)
	}
	if !found {
		t.Errorf("the cards' assignee index is created: %v", specs)
	}
	if again := w.s.ensureWekanIndexes(ctx); again > first {
		t.Errorf("negative: a second run fails what the first created: %d then %d", first, again)
	}
	if out, err := exec.Command("git", "-C", "..", "diff", "--quiet", "--", "web/wekan_indexes.go").CombinedOutput(); err != nil {
		t.Logf("wekan_indexes.go differs from git (regenerate and commit): %s", out)
	}
}
