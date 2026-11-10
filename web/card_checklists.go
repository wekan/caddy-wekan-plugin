package web

import (
	"context"
	"math"
	"time"

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
	// AssigneeID is #1618's responsible member, AssigneeName how they show.
	AssigneeID, AssigneeName string
	// Due is #4755's dueAt.
	Due dueDate
}

type cardChecklist struct {
	ID, Title string
	Percent   int
	Items     []cardChecklistItem
	Due       dueDate
}

// dueDate is a dueAt as the date badge (dateBadgeBody) shows it: the
// <time datetime>, and the date in the member's own format.
type dueDate struct{ At *time.Time }

func dueDateOf(v any) dueDate {
	if t, ok := v.(time.Time); ok && !t.IsZero() {
		return dueDate{&t}
	}
	if d, ok := v.(bson.DateTime); ok {
		t := d.Time()
		return dueDate{&t}
	}
	return dueDate{}
}

// Set is whether there is a date.
func (d dueDate) Set() bool { return d.At != nil }

// ISO is the date for <time datetime>.
func (d dueDate) ISO() string {
	if d.At == nil {
		return ""
	}
	return d.At.UTC().Format(time.RFC3339)
}

// Show is the date in the member's format (profile.dateFormat), in the
// server's zone, with its time.
func (d dueDate) Show(format string) string {
	if d.At == nil {
		return ""
	}
	if format == "" {
		format = "YYYY-MM-DD"
	}
	return formatDateByUserPreference(*d.At, format, true, time.Local.String())
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
		c := cardChecklist{ID: docString(cl, "_id"), Title: docString(cl, "title"), Due: dueDateOf(cl["dueAt"])}
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
					c.Items = append(c.Items, cardChecklistItem{ID: docString(it, "_id"), ChecklistID: c.ID, Title: docString(it, "title"), Finished: f,
						AssigneeID: docString(it, "assigneeId"), Due: dueDateOf(it["dueAt"])})
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

// nameChecklistAssignees is the assignee's avatar title, from the board's
// active members (checklistItemAssigneePopup's displayName): the full name,
// else the username.
func nameChecklistAssignees(lists []cardChecklist, members []memberChoice) {
	names := map[string]string{}
	for _, m := range members {
		names[m.ID] = m.Username
		if m.Fullname != "" {
			names[m.ID] = m.Fullname
		}
	}
	for i := range lists {
		for j := range lists[i].Items {
			it := &lists[i].Items[j]
			if it.AssigneeID != "" {
				it.AssigneeName = names[it.AssigneeID]
				if it.AssigneeName == "" {
					it.AssigneeName = it.AssigneeID
				}
			}
		}
	}
}
