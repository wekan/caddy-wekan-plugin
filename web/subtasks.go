package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WeKan's subtasks (server/models/cards.js addSubtaskCard, models/boards.js
// getDefaultSubtasksBoardAsync and models/lib/subtaskLanding*.js): a subtask
// is a card whose parentId is its parent card, made on the parent board's
// subtasks board - "^<board>^", made once, with the board's members, when the
// board names none - in the landing list and swimlane Board Settings chose,
// with the board's automatic custom fields and, if asked, the parent's
// labels.

// subtaskBoardSettings are the parent board's subtask choices, as WeKan stores them.
type subtaskBoardSettings struct {
	DefaultBoardID    string `bson:"subtasksDefaultBoardId"`
	DefaultListID     string `bson:"subtasksDefaultListId"`
	DefaultSwimlaneID string `bson:"subtasksDefaultSwimlaneId"`
}

func (s *Server) subtaskSettings(ctx context.Context, boardID string) subtaskBoardSettings {
	var st subtaskBoardSettings
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&st)
	return st
}

// defaultSubtasksBoard is getDefaultSubtasksBoardAsync: the board the parent
// board names, else one made now - once - and named on the parent.
func (s *Server) defaultSubtasksBoard(ctx context.Context, lang string, parent *Board) (*Board, error) {
	if id := s.subtaskSettings(ctx, parent.ID).DefaultBoardID; id != "" && id != "null" {
		var b Board
		if !validID(id) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&b) != nil {
			return nil, nil
		}
		return &b, nil
	}
	members := make([]Member, 0, len(parent.Members))
	for _, m := range parent.Members { // sanitizeBoardMembers: the roles, nothing else
		members = append(members, Member{UserID: m.UserID, IsAdmin: m.IsAdmin, IsActive: m.IsActive, IsNoComments: m.IsNoComments,
			IsCommentOnly: m.IsCommentOnly, IsWorker: m.IsWorker, IsNormalAssignedOnly: m.IsNormalAssignedOnly,
			IsCommentAssignedOnly: m.IsCommentAssignedOnly, IsReadOnly: m.IsReadOnly, IsReadAssignedOnly: m.IsReadAssignedOnly})
	}
	title := "^" + parent.Title + "^"
	b := &Board{ID: randomID(), Title: title, Slug: slugify(title), Permission: parent.Permission, Type: "board", Color: parent.Color,
		Description: trVars(lang, "default-subtasks-board", "board", parent.Title), CreatedAt: time.Now().UTC(), Members: members}
	for _, color := range []string{"green", "yellow", "orange", "red", "purple", "blue"} {
		b.Labels = append(b.Labels, Label{ID: randomString(idAlphabet, 6), Color: color})
	}
	// Named on the parent first, conditionally: two first subtasks at once
	// make one subtasks board, not two.
	res, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": parent.ID, "$or": bson.A{
		bson.M{"subtasksDefaultBoardId": bson.M{"$exists": false}}, bson.M{"subtasksDefaultBoardId": nil}, bson.M{"subtasksDefaultBoardId": ""}}},
		bson.M{"$set": bson.M{"subtasksDefaultBoardId": b.ID}})
	if err != nil {
		return nil, err
	}
	if res.ModifiedCount == 0 {
		return s.defaultSubtasksBoard(ctx, lang, parent) // another request named it
	}
	if _, err := s.db.Collection("boards").InsertOne(ctx, b); err != nil {
		return nil, err
	}
	sw := Swimlane{ID: randomID(), Title: tr(lang, "default"), BoardID: b.ID, Type: "swimlane"}
	if _, err := s.db.Collection("swimlanes").InsertOne(ctx, sw); err != nil {
		return nil, err
	}
	return b, nil
}

// subtaskLanding is chooseSubtaskLandingList and chooseSubtaskSwimlane, with
// the list made when the target board has none: its "Queue", named on it
// when it named none.
func (s *Server) subtaskLanding(ctx context.Context, lang string, parent, target *Board, parentSwimlaneTitle string) (listID, swimlaneID string, err error) {
	ps, ts := s.subtaskSettings(ctx, parent.ID), s.subtaskSettings(ctx, target.ID)
	var lists []List
	if cur, e := s.db.Collection("lists").Find(ctx, bson.M{"boardId": target.ID, "archived": bson.M{"$ne": true}, "deletedAt": nil},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); e == nil {
		_ = cur.All(ctx, &lists)
	}
	var swimlanes []Swimlane
	if cur, e := s.db.Collection("swimlanes").Find(ctx, bson.M{"boardId": target.ID, "archived": bson.M{"$ne": true}, "deletedAt": nil},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); e == nil {
		_ = cur.All(ctx, &swimlanes)
	}
	hasList := func(id string) bool {
		return id != "" && slices.ContainsFunc(lists, func(l List) bool { return l.ID == id })
	}
	hasLane := func(id string) bool {
		return id != "" && slices.ContainsFunc(swimlanes, func(sw Swimlane) bool { return sw.ID == id })
	}
	defaultLane := ""
	if len(swimlanes) > 0 {
		defaultLane = swimlanes[0].ID
	}
	// The swimlane: the parent's choice on a shared board, the target's own,
	// one of the parent card's swimlane's title, else the default.
	switch {
	case parent.ID != target.ID && hasLane(ps.DefaultSwimlaneID):
		swimlaneID = ps.DefaultSwimlaneID
	case hasLane(ts.DefaultSwimlaneID):
		swimlaneID = ts.DefaultSwimlaneID
	default:
		for _, sw := range swimlanes {
			if parentSwimlaneTitle != "" && sw.Title == parentSwimlaneTitle && swimlaneID == "" {
				swimlaneID = sw.ID
			}
		}
		if swimlaneID == "" {
			swimlaneID = defaultLane
		}
	}
	// The list: the parent's choice on a shared board, the target's own, a
	// new Queue the target then names when it named none, else its first.
	own := ts.DefaultListID
	switch {
	case parent.ID != target.ID && hasList(ps.DefaultListID):
		listID = ps.DefaultListID
	case hasList(own):
		listID = own
	case own == "" || own == "null" || len(lists) == 0:
		l := &List{ID: randomID(), Title: tr(lang, "queue"), BoardID: target.ID, Type: "list", SwimlaneID: defaultLane,
			Sort: s.nextSort(ctx, "lists", bson.M{"boardId": target.ID})}
		if _, err = s.db.Collection("lists").InsertOne(ctx, l); err != nil {
			return "", "", err
		}
		if own == "" || own == "null" {
			_, _ = s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": target.ID}, bson.M{"$set": bson.M{"subtasksDefaultListId": l.ID}})
		}
		listID = l.ID
	default:
		listID = lists[0].ID
	}
	return listID, swimlaneID, nil
}

// addSubtask is addSubtaskCard: the subtask card's id, or "" when there is
// nowhere to put it.
func (s *Server) addSubtask(r *http.Request, u *User, parentID, title string, inheritLabels bool) (string, error) {
	ctx := r.Context()
	title = strings.TrimSpace(title)
	if title == "" {
		return "", nil
	}
	var parent Card
	if !validID(parentID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(parentID)}).Decode(&parent) != nil {
		return "", &methodError{"not-found", "not-found"}
	}
	pb, err := s.boardFor(ctx, parent.BoardID, u)
	if err != nil || !pb.WritableIn(ctx, s, u, "write", &parent) {
		return "", &methodError{"not-authorized", "not-authorized"}
	}
	lang := languageOf(r, u).Tag
	target, err := s.defaultSubtasksBoard(ctx, lang, pb)
	if err != nil || target == nil {
		return "", err
	}
	// SubtaskDepositBleed: the subtasks board has to be one u may write.
	if !target.Writable(u, "write", nil) {
		s.recordSecurity(r, securityEvent{Key: "authz.subtask-deposit", Action: "blocked", Source: "addSubtaskCard", UserID: u.ID,
			Detail: "Subtask deposit reference or creation denied because the destination is not writable."})
		return "", &methodError{"not-authorized", "not-authorized"}
	}
	laneTitle := ""
	var lane Swimlane
	if validID(parent.SwimlaneID) && s.db.Collection("swimlanes").FindOne(ctx, bson.M{"_id": eq(parent.SwimlaneID)}).Decode(&lane) == nil {
		laneTitle = lane.Title
	}
	listID, swimlaneID, err := s.subtaskLanding(ctx, lang, pb, target, laneTitle)
	if err != nil || listID == "" || swimlaneID == "" {
		return "", err
	}
	// The destination board's automatic custom fields (#4037, #3562).
	fields := bson.A{}
	if cur, e := s.db.Collection("customFields").Find(ctx, bson.M{"boardIds": target.ID}); e == nil {
		var defs []bson.M
		_ = cur.All(ctx, &defs)
		for _, d := range defs {
			if d["automaticallyOnCard"] == true || d["alwaysOnCard"] == true {
				fields = append(fields, bson.M{"_id": d["_id"], "value": nil}) // inserted, not a filter
			}
		}
	}
	labels := []string{}
	if inheritLabels { // #2184
		labels = append(labels, parent.LabelIDs...)
	}
	// #3826: a unique sort at the end of the list, never -1 for all.
	sort := 0.0
	var last struct {
		Sort float64 `bson:"sort"`
	}
	if s.db.Collection("cards").FindOne(ctx, bson.M{"listId": eq(listID), "archived": false},
		options.FindOne().SetSort(bson.D{{Key: "sort", Value: -1}})).Decode(&last) == nil {
		sort = last.Sort + 1
	}
	c, err := s.AddCardWith(ctx, target, u, listID, swimlaneID, title, bson.M{"parentId": parent.ID, "customFields": fields,
		"labelIds": labels, "sort": sort})
	if err != nil {
		return "", err
	}
	s.hub.publishFrom(pb.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	if target.ID != pb.ID {
		s.hub.publishFrom(target.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	return c.ID, nil
}

// methodAddSubtaskCard is WeKan's addSubtaskCard(parentCardId, title, inheritLabels).
func (s *Server) methodAddSubtaskCard(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	parentID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	title, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	inherit := false
	if len(args) > 2 && json.Unmarshal(args[2], &inherit) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	id, err := s.addSubtask(r, u, parentID, title, inherit)
	if err != nil || id == "" {
		return nil, err
	}
	return id, nil
}

// addSubtaskPage is the card details' "Add a new subtask" form.
func (s *Server) addSubtaskPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, code := s.boardCard(r, u, r.PathValue("board"), r.PathValue("card"))
	if code != 0 {
		http.Error(w, http.StatusText(code), code)
		return
	}
	if _, err := s.addSubtask(r, u, c.ID, r.FormValue("title"), r.FormValue("inheritLabels") == "on"); err != nil {
		if me, ok := err.(*methodError); ok && me.Code == "not-authorized" {
			http.Error(w, "your role cannot add a subtask here", http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// subtaskRow is one subtask in the card details: its card, done (#4693:
// marked complete or archived), archived, and whether this user may tick it.
type subtaskRow struct {
	Card
	Done, Archived, CanTick bool
}

// cardSubtasks are a card's subtasks, archived ones too (WeKan's
// allSubtasks), by sort; and its "done/total" count, empty when none.
func (s *Server) cardSubtasks(ctx context.Context, u *User, cardID string) ([]subtaskRow, string) {
	var docs []bson.M
	if cur, err := s.db.Collection("cards").Find(ctx, bson.M{"parentId": eq(cardID), "deletedAt": nil},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &docs)
	}
	var rows []subtaskRow
	done := 0
	for _, d := range docs {
		var c Card
		raw, _ := bson.Marshal(d)
		_ = bson.Unmarshal(raw, &c)
		row := subtaskRow{Card: c, Archived: d["archived"] == true}
		row.Done = row.Archived || d["dueComplete"] == true
		if row.Done {
			done++
		}
		row.CanTick = !row.Archived && s.subtaskDoneDenial(ctx, u, cardID, d, d) == ""
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, ""
	}
	return rows, strconv.Itoa(done) + "/" + strconv.Itoa(len(rows))
}

// subtaskDoneDenial is WeKan's models/lib/subtaskDone.js subtaskDoneDenial:
// why u may not tick this subtask of parentID done, or "" - the subtask's own
// parents, type and state, and u's right to edit edit: the subtask, or the
// card a linked subtask shows.
func (s *Server) subtaskDoneDenial(ctx context.Context, u *User, parentID string, sub, edit bson.M) string {
	if u == nil {
		return "not-authorized"
	}
	parents := append([]string{docString(sub, "parentId")}, stringsOf(sub["parentIds"])...)
	if !slices.Contains(parents, parentID) || sub["type"] == "cardType-linkedBoard" {
		return "not-a-subtask"
	}
	var c Card
	raw, _ := bson.Marshal(edit)
	_ = bson.Unmarshal(raw, &c)
	b, err := s.boardFor(ctx, c.BoardID, u)
	if err != nil || !b.WritableIn(ctx, s, u, "write", &c) {
		return "not-authorized"
	}
	if m := b.member(u.ID); m != nil && m.AssignedOnly() && !slices.Contains(c.Assignees, u.ID) {
		return "not-authorized"
	}
	if sub["archived"] == true {
		return "subtask-archived"
	}
	return ""
}

// methodSetSubtaskDone is WeKan's setSubtaskDone(parentCardId, subtaskId,
// done) (#4693): the subtask - a linked one's card - marked complete or not,
// by whoever may edit it, from the parent they may see.
func (s *Server) methodSetSubtaskDone(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	parentID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	subID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	var done bool
	if len(args) < 3 || json.Unmarshal(args[2], &done) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	ctx := r.Context()
	var parent Card
	if !validID(parentID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(parentID)}).Decode(&parent) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	if _, err := s.boardFor(ctx, parent.BoardID, u); err != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	var sub bson.M
	if !validID(subID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(subID)}).Decode(&sub) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	target := sub
	if sub["type"] == "cardType-linkedCard" {
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(docString(sub, "linkedId"))}).Decode(&target) != nil {
			return nil, &methodError{"not-found", "not-found"}
		}
	}
	if denial := s.subtaskDoneDenial(ctx, u, parentID, sub, target); denial != "" {
		return nil, &methodError{denial, denial}
	}
	if (target["dueComplete"] == true) != done {
		if _, err := s.cardsUpdateOne(ctx, u.ID, bson.M{"_id": target["_id"]}, bson.M{"$set": bson.M{"dueComplete": done, "modifiedAt": time.Now().UTC()}}); err != nil {
			return nil, err
		}
		s.hub.publishFrom(parent.BoardID, "board-changed", r.Header.Get("X-Weddy-Page"))
		if b := docString(target, "boardId"); b != parent.BoardID {
			s.hub.publishFrom(b, "board-changed", r.Header.Get("X-Weddy-Page"))
		}
	}
	return done, nil
}
