package web

import (
	"encoding/json"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// methodStructuralMoveOptions is WeKan's structuralMoveOptions(target)
// (server/moveBoardObjects.js), the mixed selection's Move pop-over: the
// boards the caller may write, and on the target board, step by step as the
// target names them, its swimlanes, the lists of the swimlane (its own and
// those in every swimlane), the cards of the list in that swimlane - for an
// assigned-only member only theirs - the checklists of the card and the items
// of the checklist; each by sort, id and title only.
func (s *Server) methodStructuralMoveOptions(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	match := &methodError{"Match.Error", "Match failed"}
	var target map[string]any
	if len(args) < 1 || json.Unmarshal(args[0], &target) != nil {
		return nil, match
	}
	str := func(k string) (string, bool) {
		v, has := target[k]
		if !has {
			return "", true
		}
		s, ok := v.(string)
		return s, ok
	}
	for k := range target {
		if !map[string]bool{"boardId": true, "swimlaneId": true, "listId": true, "cardId": true, "checklistId": true, "itemId": true, "position": true}[k] {
			return nil, match
		}
	}
	ids := map[string]string{}
	for _, k := range []string{"boardId", "swimlaneId", "listId", "cardId", "checklistId", "itemId", "position"} {
		v, ok := str(k)
		if !ok {
			return nil, match
		}
		ids[k] = v
	}
	if ids["boardId"] == "" {
		return nil, match
	}
	ctx := r.Context()
	type option struct {
		ID    string `bson:"_id" json:"_id"`
		Title string `bson:"title" json:"title"`
	}
	projected := func(coll string, q bson.M) []option {
		out := []option{}
		if cur, err := s.db.Collection(coll).Find(ctx, q, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}).
			SetProjection(bson.M{"title": 1})); err == nil {
			_ = cur.All(ctx, &out)
		}
		return out
	}
	var mine []Board
	if cur, err := s.db.Collection("boards").Find(ctx, bson.M{"members.userId": u.ID, "archived": false},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "title", Value: 1}})); err == nil {
		_ = cur.All(ctx, &mine)
	}
	writable := []option{}
	for _, b := range mine {
		if b.Writable(u, "write", nil) {
			writable = append(writable, option{b.ID, b.Title})
		}
	}
	b, err := s.boardFor(ctx, ids["boardId"], u)
	if err != nil || !b.MemberOf(u.ID) { // not a write: the choices it lists; moveBoardObjects checks the role
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	cardScope := bson.M{}
	if m := b.member(u.ID); m != nil && m.AssignedOnly() {
		cardScope["assignees"] = u.ID
	}
	swimlanes := projected("swimlanes", bson.M{"boardId": b.ID, "archived": false, "deletedAt": nil})
	result := map[string]any{"boardId": writable, "swimlaneId": swimlanes}
	if sw := ids["swimlaneId"]; validID(sw) {
		// The swimlane's own lists and those shown in every swimlane (no
		// swimlaneId, or an empty one).
		result["listId"] = projected("lists", bson.M{"boardId": b.ID, "archived": false, "deletedAt": nil, "$or": bson.A{
			bson.M{"swimlaneId": eq(sw)}, bson.M{"swimlaneId": bson.M{"$in": bson.A{nil, ""}}}}})
		if l := ids["listId"]; validID(l) {
			q := bson.M{"boardId": b.ID, "listId": eq(l), "swimlaneId": eq(sw), "archived": false, "deletedAt": nil}
			for k, v := range cardScope {
				q[k] = v
			}
			result["cardId"] = projected("cards", q)
		}
	}
	cardOK := func(id string) bool {
		q := bson.M{"_id": eq(id), "boardId": b.ID}
		for k, v := range cardScope {
			q[k] = v
		}
		n, _ := s.db.Collection("cards").CountDocuments(ctx, q)
		return n > 0
	}
	if c := ids["cardId"]; validID(c) && cardOK(c) {
		result["checklistId"] = projected("checklists", bson.M{"cardId": eq(c)})
	}
	if cl := ids["checklistId"]; validID(cl) {
		var checklist struct {
			CardID string `bson:"cardId"`
		}
		if s.db.Collection("checklists").FindOne(ctx, bson.M{"_id": eq(cl), "boardId": b.ID}).Decode(&checklist) == nil && cardOK(checklist.CardID) {
			result["itemId"] = projected("checklistItems", bson.M{"checklistId": eq(cl)})
		}
	}
	return result, nil
}
