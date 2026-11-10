package web

import (
	"context"
	"math"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The card details' checklists, as WeKan's checklists.jade draws them: each
// checklist in its sort order with its title, checklistProgress's percent and
// its items in theirs - read here; writing them is the card details' own
// TODO item. Its actions pop-over (checklistActionsPopup) opens the
// checklist's Export, scoped to it as exportChecklistPopup is.

type cardChecklistItem struct {
	ID, ChecklistID, Title string
	Finished               bool
}

type cardChecklist struct {
	ID, Title string
	Percent   int
	Items     []cardChecklistItem
}

// cardChecklists is card.checklists() and each checklist's items(), sorted.
func (s *Server) cardChecklists(ctx context.Context, cardID string) []cardChecklist {
	cur, err := s.db.Collection("checklists").Find(ctx, bson.M{"cardId": eq(cardID)}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		return nil
	}
	var lists []bson.M
	if cur.All(ctx, &lists) != nil {
		return nil
	}
	out := make([]cardChecklist, 0, len(lists))
	for _, cl := range lists {
		c := cardChecklist{ID: docString(cl, "_id"), Title: docString(cl, "title")}
		ic, err := s.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": eq(c.ID), "cardId": eq(cardID)}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		if err == nil {
			var items []bson.M
			if ic.All(ctx, &items) == nil {
				done := 0
				for _, it := range items {
					f, _ := it["isFinished"].(bool)
					if f {
						done++
					}
					c.Items = append(c.Items, cardChecklistItem{ID: docString(it, "_id"), ChecklistID: c.ID, Title: docString(it, "title"), Finished: f})
				}
				c.Percent = checklistPercent(done, len(items))
			}
		}
		out = append(out, c)
	}
	return out
}

// checklistPercent is finishedPercent: Math.round(finished / count * 100).
func checklistPercent(done, count int) int {
	if count == 0 {
		return 0
	}
	return int(math.Floor(float64(done)/float64(count)*100 + 0.5))
}
