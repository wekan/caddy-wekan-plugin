package web

// WeKan's copyBoard method (server/publications/boards.js), beside the REST
// copy POST /api/boards/{board}/copy: a board admin copies their board.
//
// CopyIdentityBleed (models/lib/boardCopyProperties.js): the caller may
// change only the copy's title, sort, type and which parts are copied -
// above all never its _id, which used to make WeKan read and copy ANOTHER,
// private, board. Any other property is an attempt: refused before anything
// is read, and recorded (authz.board-copy-overrides). A wrong type on a
// supported property is ordinary bad input, refused unrecorded. copyOptions
// is WeKan's selection (models/lib/boardCopyOptions.js): only its own keys,
// each true or false, a part off when the part it lives in is off.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// boardCopyFields are BOARD_COPY_FIELDS: each part and the part it is in.
var boardCopyFields = []struct{ key, parent string }{
	{"swimlanes", ""}, {"lists", "swimlanes"}, {"labels", ""}, {"customFields", ""}, {"rules", ""},
	{"integrations", ""}, {"cards", "lists"}, {"checklists", "cards"}, {"comments", "cards"},
	{"attachments", "cards"}, {"scrum", "customFields"},
}

// boardCopySections: the export document's sections of each part.
var boardCopySections = map[string][]string{
	"swimlanes": {"swimlanes"}, "lists": {"lists"}, "labels": {"labels"}, "customFields": {"customFields"},
	"rules": {"rules", "triggers", "actions"}, "cards": {"cards"}, "checklists": {"checklists", "checklistItems"},
	"comments": {"comments"}, "attachments": {"attachments"},
}

var errCopyProperties = errors.New("invalid-copy-properties")

// boardCopyProps are a copy's supported changes, checked.
type boardCopyProps struct {
	Title, Type  string
	Sort         *float64
	WithoutCards bool
	Options      map[string]bool // nil: everything
}

// parseBoardCopyProperties is boardCopyProperties: attempt is true when the
// refusal is for a property outside the contract.
func parseBoardCopyProperties(raw json.RawMessage) (p boardCopyProps, attempt bool, err error) {
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil || in == nil {
		return p, false, errCopyProperties
	}
	for k, v := range in {
		// JSON has no undefined: a null fails every check, as in WeKan.
		null := string(v) == "null"
		switch k {
		case "title":
			if null || json.Unmarshal(v, &p.Title) != nil {
				err = errCopyProperties
			}
		case "sort":
			var f float64
			if null || json.Unmarshal(v, &f) != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				err = errCopyProperties
			} else {
				p.Sort = &f
			}
		case "type":
			if null || json.Unmarshal(v, &p.Type) != nil || (p.Type != "board" && p.Type != "template-board") {
				err = errCopyProperties
			}
		case "withoutCards":
			if null || json.Unmarshal(v, &p.WithoutCards) != nil {
				err = errCopyProperties
			}
		case "copyOptions":
			// checked by normalizeBoardCopyOptions
		default:
			attempt, err = true, errCopyProperties
		}
	}
	return p, attempt, err
}

// normalizeBoardCopyOptions: every part, the given ones as given, a part off
// when its parent is.
func normalizeBoardCopyOptions(raw json.RawMessage) (map[string]bool, bool) {
	out := map[string]bool{}
	for _, f := range boardCopyFields {
		out[f.key] = true
	}
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil || in == nil {
		return nil, false
	}
	for k, v := range in {
		on, isBool := v.(bool)
		if _, known := out[k]; !known || !isBool {
			return nil, false
		}
		out[k] = on
	}
	for _, f := range boardCopyFields { // in order: a parent is settled first
		if f.parent != "" && !out[f.parent] {
			out[f.key] = false
		}
	}
	return out, true
}

// methodCopyBoard is WeKan's copyBoard(boardId, properties).
func (s *Server) methodCopyBoard(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	if len(args) < 2 {
		return nil, &methodError{"Match.Error", "Expected object"}
	}
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	if !b.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	p, attempt, err := parseBoardCopyProperties(args[1])
	if err != nil {
		if attempt {
			s.recordSecurity(r, securityEvent{Key: "authz.board-copy-overrides", Action: "blocked", Source: "method:copyBoard", UserID: u.ID,
				Detail: "Unsupported board-copy properties refused"})
		}
		return nil, &methodError{"invalid-copy-properties", "Invalid board copy properties"}
	}
	var in map[string]json.RawMessage
	_ = json.Unmarshal(args[1], &in)
	if raw, ok := in["copyOptions"]; ok {
		opts, ok := normalizeBoardCopyOptions(raw)
		if !ok {
			return nil, &methodError{"invalid-copy-options", "Invalid copy options"}
		}
		p.Options = opts
	}
	id, err := s.copyBoardAs(r, u, &b, p)
	if errors.Is(err, errBoardCreation) {
		return nil, &methodError{"not-authorized", err.Error()}
	}
	return id, err
}

// copyBoardAs copies b for u - its lists, cards, checklists, comments,
// attachments, webhooks (#5592) and rules (#5627), or the parts p selects -
// with p's title, sort and type, and returns the new board's id.
func (s *Server) copyBoardAs(r *http.Request, u *User, b *Board, p boardCopyProps) (string, error) {
	ctx := r.Context()
	raw, err := s.boardDocs(ctx, b, exportDocOptions{})
	if err != nil {
		return "", err
	}
	plain, err := plainDocs(raw)
	if err != nil {
		return "", err
	}
	doc, _ := plain.(map[string]any)
	want := p.Options
	if p.WithoutCards {
		if want == nil {
			want, _ = normalizeBoardCopyOptions(json.RawMessage(`{}`))
		}
		want["cards"], want["checklists"], want["comments"], want["attachments"] = false, false, false, false
	}
	for part, on := range want {
		if !on {
			for _, sec := range boardCopySections[part] {
				delete(doc, sec)
			}
		}
	}
	doc["title"] = strings.TrimSpace(p.Title)
	if doc["title"] == "" {
		doc["title"] = s.uniqueTitle(ctx, b.Title)
	}
	same := map[string]string{} // the board's people stay themselves
	for _, m := range b.Members {
		if m.IsActive {
			same[m.UserID] = m.UserID
		}
	}
	id, err := s.ImportBoard(r, u, doc, same)
	if err != nil {
		return "", err
	}
	set := bson.M{}
	// The same people, as WeKan's board.copy() keeps them: the members array
	// exactly as stored - every role flag, a false one too - not as the
	// import rebuilds it.
	var src struct {
		Members bson.A `bson:"members"`
	}
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&src) == nil && src.Members != nil {
		set["members"] = src.Members
	}
	if p.Sort != nil {
		set["sort"] = *p.Sort
	}
	if p.Type != "" {
		set["type"] = p.Type
	}
	if len(set) > 0 {
		_, _ = s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$set": set})
	}
	if want == nil || want["integrations"] {
		s.copyIntegrations(ctx, b.ID, id)
	}
	return id, nil
}

// copyIntegrations: the board's webhooks, on the copy.
func (s *Server) copyIntegrations(ctx context.Context, from, to string) {
	cur, err := s.db.Collection("integrations").Find(ctx, bson.M{"boardId": eq(from)})
	if err != nil {
		return
	}
	var hooks []bson.M
	_ = cur.All(ctx, &hooks)
	for _, h := range hooks {
		h["_id"], h["boardId"] = randomID(), to
		_, _ = s.db.Collection("integrations").InsertOne(ctx, h)
	}
}
