package web

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The board's and the card's PDF, as Node.js WeKan's ExporterBoardPDF and
// ExporterCardPDF write them (models/server/ExporterCardPDF.js) with the
// base-font writer: the board's header - its title, or the swimlane's or the
// list's, its members, dates and description - then a heading a swimlane and
// a list, and every card in the card document's layout; or the one card.

// pdfDate is formatDateValue: "-" for no date.
func (o excelCardOptions) pdfDate(v any) string {
	t, ok := v.(time.Time)
	if !ok {
		return "-"
	}
	zone := o.Zone
	if zone == "" {
		zone = "UTC"
	}
	f := formatDateByUserPreference(t, o.DateFormat, true, zone)
	if o.Zone == "" {
		f += " UTC"
	}
	return f
}

// pdfUser is formatUser: the full name, the username, the initials or the id.
func pdfUser(u bson.M) string {
	if u == nil {
		return "Unknown"
	}
	p, _ := u["profile"].(bson.M)
	if p == nil {
		if d, ok := u["profile"].(bson.D); ok {
			p = bson.M(docMap(d))
		}
	}
	for _, v := range []string{strOf(p["fullname"]), strOf(u["username"]), strOf(p["initials"]), strOf(u["_id"])} {
		if v != "" {
			return v
		}
	}
	return ""
}

// pdfCustomValue is customFieldValue and formatCustomFieldValue.
func (o excelCardOptions) pdfCustomValue(def, value any) any {
	out := value
	items := cdList(cdGet(cdGet(def, "settings"), "dropdownItems"))
	nameOf := func(id any) (any, bool) {
		for _, it := range items {
			if cdTruthy(it) && cdGet(it, "_id") == id {
				return cdGet(it, "name"), true
			}
		}
		return nil, false
	}
	switch cdString(cdGet(def, "type")) {
	case "dropdown":
		if n, ok := nameOf(value); ok {
			out = n
		}
	case "dropdownMultiSelect":
		var names []any
		for _, id := range cdList(value) {
			if n, ok := nameOf(id); ok {
				names = append(names, n)
			} else {
				names = append(names, id)
			}
		}
		out = names
		if names == nil {
			out = []any{}
		}
	}
	switch x := out.(type) {
	case nil, undefinedValue:
		return "-"
	case string:
		if x == "" {
			return "-"
		}
		return x
	case time.Time:
		return o.pdfDate(x)
	case []any:
		if len(x) == 0 {
			return "-"
		}
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = cdString(e)
		}
		return strings.Join(parts, ", ")
	case bool:
		if x {
			return "x"
		}
		return "-"
	}
	return cdString(out)
}

// pdfTranslate is the PDF's __(key, fallback): the language's word, else the
// fallback - never the key.
func pdfTranslate(lang string) func(key, fallback string) string {
	return func(key, fallback string) string {
		if v := tr(lang, key); v != "" && v != key {
			return v
		}
		return fallback
	}
}

// pdfExport is the board's PDF, or with only the card's.
func (s *Server) pdfExport(ctx context.Context, b *Board, o excelCardOptions, only *Card) []byte {
	find := func(coll string, filter bson.M, sort bson.D) []any {
		opts := options.Find()
		if sort != nil {
			opts.SetSort(sort)
		}
		var docs []bson.M
		if cur, err := s.db.Collection(coll).Find(ctx, filter, opts); err == nil {
			_ = cur.All(ctx, &docs)
		}
		out := make([]any, len(docs))
		for i, d := range docs {
			out[i] = plain(d)
		}
		return out
	}
	fields := o.Fields
	if fields == nil {
		fields = exportFieldKeys
	}
	has := func(k string) bool {
		for _, f := range fields {
			if f == k {
				return true
			}
		}
		return false
	}
	bySort := bson.D{{Key: "sort", Value: 1}}
	var raw bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&raw)
	board := plain(raw)
	listSel, laneSel := bson.M{"boardId": b.ID, "archived": false}, bson.M{"boardId": b.ID, "archived": false}
	cardSel := bson.M{"boardId": b.ID, "archived": false}
	if only != nil {
		listSel, laneSel = bson.M{"boardId": b.ID}, bson.M{"boardId": b.ID}
		cardSel = bson.M{"_id": eq(only.ID), "boardId": b.ID, "listId": eq(only.ListID)}
	} else {
		if id := o.Scope.SwimlaneID; id != "" {
			cardSel["swimlaneId"] = eq(id)
			laneSel = bson.M{"boardId": b.ID, "archived": false, "_id": eq(id)}
		}
		if id := o.Scope.ListID; id != "" {
			cardSel["listId"] = eq(id)
			listSel = bson.M{"boardId": b.ID, "archived": false, "_id": eq(id)}
		}
	}
	lists, lanes, cards := find("lists", listSel, bySort), find("swimlanes", laneSel, bySort), find("cards", cardSel, bySort)
	ids := bson.A{}
	for _, c := range cards {
		ids = append(ids, cdString(cdGet(c, "_id")))
	}
	var checklists, items, subtasks, comments, attachments []any
	if len(ids) > 0 {
		// A card's PDF reads everything of it; the document leaves out what
		// the selection does not ask for.
		if only != nil || has("checklists") {
			checklists = find("checklists", bson.M{"cardId": dbIn(ids)}, bySort)
			cl := bson.A{}
			for _, c := range checklists {
				cl = append(cl, cdString(cdGet(c, "_id")))
			}
			if len(cl) > 0 {
				items = find("checklistItems", bson.M{"checklistId": dbIn(cl)}, bySort)
			}
		}
		if only != nil || has("subtasks") {
			subtasks = find("cards", bson.M{"boardId": b.ID, "$or": bson.A{bson.M{"parentId": dbIn(ids)}, bson.M{"parentIds": dbIn(ids)}}}, bySort)
		}
		if only != nil || has("comments") {
			comments = find("card_comments", bson.M{"cardId": dbIn(ids)}, bson.D{{Key: "createdAt", Value: 1}})
		}
		if only != nil || has("attachments") {
			attachments = find("attachments", bson.M{"meta.cardId": dbIn(ids), "deletedAt": nil}, bson.D{{Key: "uploadedAt", Value: 1}})
		}
	}
	defs := map[string]any{}
	if only != nil || has("custom-fields") {
		fieldIDs := bson.A{}
		for _, c := range cards {
			for _, f := range cdList(cdGet(c, "customFields")) {
				if id := cdOr(cdGet(f, "_id")); id != "" {
					fieldIDs = append(fieldIDs, id)
				}
			}
		}
		if len(fieldIDs) > 0 {
			for _, d := range find("customFields", bson.M{"_id": dbIn(fieldIDs)}, nil) {
				defs[cdString(cdGet(d, "_id"))] = d
			}
		}
	}
	userIDs, seen := bson.A{}, map[string]bool{}
	addUser := func(v any) {
		if id := cdOr(v); id != "" && !seen[id] {
			seen[id] = true
			userIDs = append(userIDs, id)
		}
	}
	if only == nil {
		for _, m := range b.Members {
			addUser(m.UserID)
		}
	}
	for _, c := range cards {
		addUser(cdGet(c, "userId"))
		for _, k := range []string{"members", "assignees", "requesters", "assigners"} {
			for _, id := range cdList(cdGet(c, k)) {
				addUser(id)
			}
		}
		for _, k := range []string{"positive", "negative"} {
			for _, id := range cdList(cdGet(cdGet(c, "vote"), k)) {
				addUser(id)
			}
		}
	}
	for _, c := range comments {
		addUser(cdGet(c, "userId"))
	}
	for _, a := range attachments {
		up := cdGet(a, "userId")
		if !cdTruthy(up) {
			up = cdGet(cdGet(a, "meta"), "userId")
		}
		addUser(up)
	}
	var users []bson.M
	if len(userIDs) > 0 {
		if cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": dbIn(userIDs)}); err == nil {
			_ = cur.All(ctx, &users)
		}
	}
	if anon := s.exportAnonymizer(ctx, b, users); anon != nil {
		for _, u := range users {
			anon.user(u)
		}
		var docs []bson.M
		for _, c := range append(append([]any{}, cards...), subtasks...) {
			docs = append(docs, bson.M(c.(map[string]any)))
		}
		anon.cards(docs)
		var cms []bson.M
		for _, c := range comments {
			cms = append(cms, bson.M(c.(map[string]any)))
		}
		anon.comments(cms)
	}
	byID := map[string]bson.M{}
	for _, u := range users {
		byID[strOf(u["_id"])] = u
	}
	userName := func(id string) string { return pdfUser(byID[id]) }
	t := pdfTranslate(o.Lang)
	field := func(key, fb string, v any) string {
		text := cdString(v)
		if v == nil || isUndef(v) || text == "" {
			text = "-"
		}
		return t(key, fb) + ": " + text
	}
	group := func(rows []any, key func(any) string) map[string][]any {
		m := map[string][]any{}
		for _, x := range rows {
			if id := key(x); id != "" {
				m[id] = append(m[id], x)
			}
		}
		return m
	}
	checklistsOf := group(checklists, func(x any) string { return cdOr(cdGet(x, "cardId")) })
	commentsOf := group(comments, func(x any) string { return cdOr(cdGet(x, "cardId")) })
	attachmentsOf := group(attachments, func(x any) string { return cdOr(cdGet(cdGet(x, "meta"), "cardId")) })
	subtasksOf := map[string][]any{}
	for _, st := range subtasks {
		dup := map[string]bool{}
		for _, id := range append([]any{cdGet(st, "parentId")}, cdList(cdGet(st, "parentIds"))...) {
			if pid := cdOr(id); validID(pid) && !dup[pid] {
				dup[pid] = true
				subtasksOf[pid] = append(subtasksOf[pid], st)
			}
		}
	}
	listByID, laneByID := map[string]any{}, map[string]any{}
	for _, l := range lists {
		listByID[cdString(cdGet(l, "_id"))] = l
	}
	for _, l := range lanes {
		laneByID[cdString(cdGet(l, "_id"))] = l
	}
	heads := []string{"#", t("name", "Name"), t("size", "Size"), t("type", "Type"),
		t("export-card-attachment-uploaded-at", "Uploaded At"), t("export-card-attachment-uploaded-by", "Uploaded By")}
	cardLines := func(c, list, lane any) []pdfLine {
		id := cdString(cdGet(c, "_id"))
		var images []cdImage
		for _, a := range attachmentsOf[id] {
			typ := strings.ToLower(cdOr(cdGet(a, "type")))
			if typ != "image/jpeg" && typ != "image/jpg" && typ != "image/png" {
				continue
			}
			name := cdOr(cdGet(a, "name"))
			if name == "" {
				name = cdOr(cdGet(cdGet(a, "meta"), "name"))
			}
			if name == "" {
				name = cdOr(cdGet(a, "_id"))
			}
			var att Attachment
			var data []byte
			if s.db.Collection("attachments").FindOne(ctx, bson.M{"_id": eq(cdString(cdGet(a, "_id")))}).Decode(&att) == nil {
				if p, err := s.attachmentFile(&att); err == nil {
					data, _ = os.ReadFile(p)
				}
			}
			images = append(images, cdImage{Name: name, Size: formatExportFileSize(cdGet(a, "size")), Ext: typ, Data: data})
		}
		doc := buildExportCardDocument(cardExportInput{Board: board, List: list, Card: c, Swimlane: lane, Checklists: checklistsOf[id],
			ChecklistItems: items, Subtasks: subtasksOf[id], Comments: commentsOf[id], Attachments: attachmentsOf[id], Images: images,
			CustomFieldsByID: defs},
			cardExportOptions{Fields: fields, UserName: userName, FormatDate: o.pdfDate, CustomValue: o.pdfCustomValue, Translate: t})
		return documentToLines(doc, heads)
	}
	str := func(v string) pdfLine { return pdfLine{IsString: true, Text: v} }
	var lines []pdfLine
	if only != nil {
		for _, c := range cards {
			lines = append(lines, cardLines(c, listByID[cdString(cdGet(c, "listId"))], laneByID[cdString(cdGet(c, "swimlaneId"))])...)
		}
		return buildPdfBuffer(lines)
	}
	// _boardHeaderLines.
	firstTitle := func(rows []any) string {
		if len(rows) > 0 {
			return cdOr(cdGet(rows[0], "title"))
		}
		return ""
	}
	heading := cdOr(cdGet(board, "title"))
	if heading == "" {
		heading = "Board"
	}
	switch {
	case o.Scope.ListID != "":
		heading = field("list", "List", firstTitle(lists))
	case o.Scope.SwimlaneID != "":
		heading = field("swimlane", "Swimlane", firstTitle(lanes))
	}
	lines = append(lines, pdfPlain(heading, true), str(""))
	if o.Scope.ListID == "" && o.Scope.SwimlaneID == "" && has("board-header") {
		var names []string
		for _, m := range b.Members {
			if n := userName(m.UserID); n != "" {
				names = append(names, n)
			}
		}
		lines = append(lines, str(field("members", "Members", strings.Join(names, ", "))),
			str(field("createdAt", "Created at", o.pdfDate(cdGet(board, "createdAt")))),
			str(field("modifiedAt", "Modified at", o.pdfDate(cdGet(board, "modifiedAt")))))
		if d := cdOr(cdGet(board, "description")); d != "" {
			lines = append(lines, str(""), pdfPlain(t("description", "Description")+":", true))
			lines = append(lines, wrapRichTextBlock(d, "")...)
		}
		lines = append(lines, str(""))
	}
	type laneGroup struct {
		lane  any
		title string
	}
	var groups []laneGroup
	for _, l := range lanes {
		if cdString(cdGet(l, "type")) != "template-swimlane" {
			title := cdOr(cdGet(l, "title"))
			if title == "" {
				title = t("swimlane", "Swimlane")
			}
			groups = append(groups, laneGroup{l, title})
		}
	}
	if len(groups) == 0 {
		groups = []laneGroup{{nil, t("swimlane", "Swimlane")}}
	}
	if o.Scope.ListID != "" {
		groups = []laneGroup{{nil, ""}}
	}
	for _, g := range groups {
		if g.title != "" && o.Scope.SwimlaneID == "" {
			lines = append(lines, pdfPlain(field("swimlane", "Swimlane", g.title), true), str(""))
		}
		for _, l := range lists {
			var listCards []any
			for _, c := range cards {
				if cdString(cdGet(c, "listId")) == cdString(cdGet(l, "_id")) && (g.lane == nil || cdString(cdGet(c, "swimlaneId")) == cdString(cdGet(g.lane, "_id"))) {
					listCards = append(listCards, c)
				}
			}
			if o.Scope.ListID == "" {
				title := cdOr(cdGet(l, "title"))
				if title == "" {
					title = t("list", "List")
				}
				lines = append(lines, pdfPlain(field("list", "List", title+" ("+strconv.Itoa(len(listCards))+")"), true), str(""))
			}
			for _, c := range listCards {
				list := listByID[cdString(cdGet(c, "listId"))]
				if list == nil {
					list = l
				}
				lane := laneByID[cdString(cdGet(c, "swimlaneId"))]
				if lane == nil {
					lane = g.lane
				}
				lines = append(lines, cardLines(c, list, lane)...)
				lines = append(lines, str(""), str(""))
			}
		}
	}
	return buildPdfBuffer(lines)
}
