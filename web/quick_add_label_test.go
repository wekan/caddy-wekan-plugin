package web

import (
	"context"
	"testing"
)

func TestQuickAddLabelReusesConcurrentLabel(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	user := w.seedUser(t, "ann", "pw")
	board, err := w.s.CreateBoard(ctx, user, "Bracket labels")
	if err != nil {
		t.Fatal(err)
	}
	stale := *board
	title, label, err := w.s.quickAddLabel(ctx, board, user, "[Fedora] First")
	if err != nil || title != "First" || label == "" {
		t.Fatalf("first save: %q, %q, %v", title, label, err)
	}
	title, reused, err := w.s.quickAddLabel(ctx, &stale, user, "[fedora] Second")
	if err != nil || title != "Second" || reused != label {
		t.Fatalf("second save: %q, %q, %v", title, reused, err)
	}
}
