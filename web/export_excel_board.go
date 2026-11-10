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

// A board's Excel export with Card details ticked - the Export pop-over's
// default - as Node.js WeKan's models/server/ExporterExcelBoard.js writes
// it: the board's title, its members, dates and description, then every
// swimlane and list under a gray heading and every card in the card
// export's own layout (export_card_document.go, export_card_excel.go), a
// card a page.

// xThemeAccents is THEME_ACCENTS (models/lib/themeAccents.js).
var xThemeAccents = map[string]string{
	"belize": "#2573a7", "nephritis": "#217d49", "pomegranate": "#c0392b", "pumpkin": "#a75919",
	"wisteria": "#8e44ad", "moderatepink": "#bd3a79", "strongcyan": "#00798d", "limegreen": "#277c48",
	"midnight": "#2c3e50", "dark": "#2c3e51", "relax": "#217d4a", "corteza": "#4a738a",
	"appleglasspastel": "#2563eb", "clearblue": "#00798d", "cleargreen": "#277c48", "clearorange": "#a75919",
	"clearpink": "#bd3a79", "clearpurple": "#8e44ad", "clearred": "#c0392b", "natural": "#596557",
	"modern": "#2372a4", "moderndark": "#2a2a2a", "exodark": "#222", "cleandark": "#2E2E39",
	"cleanlight": "rgba(190, 190, 190, 1)",
}

// excelDateFormats are the formats formatDateByUserPreference knows.
var excelDateFormats = []string{"YYYY-MM-DD", "DD-MM-YYYY", "MM-DD-YYYY",
	"YYYY-MM-DD-date-only", "DD-MM-YYYY-date-only", "MM-DD-YYYY-date-only"}

// formatDateByUserPreference is imports/lib/dateUtils.js's, in zone (an
// IANA name; an unknown one is the server's, UTC).
func formatDateByUserPreference(t time.Time, format string, withTime bool, zone string) string {
	if strings.HasSuffix(format, "-date-only") {
		format, withTime = strings.TrimSuffix(format, "-date-only"), false
	}
	t = t.UTC()
	if zone != "" {
		if loc, err := time.LoadLocation(zone); err == nil {
			t = t.In(loc)
		}
	}
	y, m, d := t.Format("2006"), t.Format("01"), t.Format("02")
	var s string
	switch format {
	case "DD-MM-YYYY":
		s = d + "-" + m + "-" + y
	case "MM-DD-YYYY":
		s = m + "-" + d + "-" + y
	default:
		s = y + "-" + m + "-" + d
	}
	if withTime {
		s += " " + t.Format("15:04")
	}
	return s
}

// excelCardOptions is what ExporterExcelBoard and ExporterExcelCard read
// from the request: the language, the selection (nil is all), the date
// format and the reader's zone.
type excelCardOptions struct {
	Lang, DateFormat, Zone string
	Fields                 []string
	// Scope is a swimlane's or a list's export (#1173).
	Scope exportScope
}

func (o excelCardOptions) has(key string) bool {
	if o.Fields == nil {
		return true
	}
	for _, f := range o.Fields {
		if f == key {
			return true
		}
	}
	return false
}

// fmtDate is fmtDate: the date in the reader's zone, or in UTC and saying so.
func (o excelCardOptions) fmtDate(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case string:
		if x == "" {
			return ""
		}
		p, ok := jsDateTime(x)
		if !ok {
			return ""
		}
		t = p
	default:
		return ""
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

// customFieldValueText is customFieldValueText: a dropdown's item name, a
// date in the reader's format, a list joined, yes or no.
func (o excelCardOptions) customFieldValueText(def, value any) any {
	if value == nil || isUndef(value) || value == "" {
		return ""
	}
	typ := cdString(cdGet(def, "type"))
	items := cdList(cdGet(cdGet(def, "settings"), "dropdownItems"))
	nameOf := func(id any) (string, bool) {
		for _, it := range items {
			if cdTruthy(it) && cdGet(it, "_id") == id {
				return cdOr(cdGet(it, "name")), true
			}
		}
		return "", false
	}
	if typ == "dropdown" {
		if n, ok := nameOf(value); ok {
			return n
		}
	}
	if typ == "dropdownMultiSelect" {
		var out []string
		for _, id := range cdList(value) {
			if n, ok := nameOf(id); ok {
				out = append(out, n)
			} else {
				out = append(out, cdString(id))
			}
		}
		return strings.Join(out, ", ")
	}
	switch x := value.(type) {
	case time.Time:
		return o.fmtDate(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = cdString(e)
		}
		return strings.Join(parts, ", ")
	case bool:
		if x {
			return tr(o.Lang, "yes")
		}
		return tr(o.Lang, "no")
	}
	return cdString(value)
}

// excelSheetNameOf is ExporterExcelBoard's sanitizeSheetName.
func excelSheetNameOf(v string) string {
	c := jsTrim(strings.Map(func(r rune) rune {
		if strings.ContainsRune(`:\/?*[]`, r) {
			return ' '
		}
		return r
	}, v))
	if c == "" {
		c = "Board"
	}
	if len(utf16Encode(c)) > 31 {
		return jsSlice16(c, 28) + "..."
	}
	return c
}

func utf16Encode(s string) []rune { return u16(s) }

// excelEmbeddable is EMBEDDABLE_IMAGE_MIME.
var excelEmbeddable = map[string]string{"image/jpeg": "jpeg", "image/jpg": "jpeg", "image/png": "png", "image/gif": "gif", "image/bmp": "bmp"}

// excelBoardWorkbook is ExporterExcelBoard's workbook for a board.
//
// With only, it is ExporterExcelCard's workbook of that one card instead:
// the card in its layout from row 1, on a sheet named for it, with no board
// header and no headings.
func (s *Server) excelBoardWorkbook(ctx context.Context, b *Board, o excelCardOptions, only *Card) ([]byte, error) {
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
	bySort := bson.D{{Key: "sort", Value: 1}}
	var raw bson.M
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&raw) != nil {
		return nil, os.ErrNotExist
	}
	board := plain(raw)
	listFilter, cardFilter := bson.M{"boardId": b.ID, "archived": false}, bson.M{"boardId": b.ID, "archived": false, "linkedId": bson.M{"$in": bson.A{"", nil}}}
	laneFilter := listFilter
	if only != nil {
		// A card's export: that card, archived or not, and its list and
		// swimlane whatever they are (ReactiveCache.getList).
		listFilter = bson.M{"boardId": b.ID}
		laneFilter = listFilter
		cardFilter = bson.M{"_id": eq(only.ID), "boardId": b.ID, "listId": eq(only.ListID)}
	} else {
		// A swimlane's or a list's: its cards, and only it of its kind.
		if id := o.Scope.SwimlaneID; id != "" {
			cardFilter["swimlaneId"] = eq(id)
			laneFilter = bson.M{"boardId": b.ID, "archived": false, "_id": eq(id)}
		}
		if id := o.Scope.ListID; id != "" {
			cardFilter["listId"] = eq(id)
			listFilter = bson.M{"boardId": b.ID, "archived": false, "_id": eq(id)}
		}
	}
	lists := find("lists", listFilter, bySort)
	swimlanes := find("swimlanes", laneFilter, bySort)
	cards := find("cards", cardFilter, bySort)
	ids := bson.A{}
	for _, c := range cards {
		ids = append(ids, cdString(cdGet(c, "_id")))
	}
	var checklists, items, subtasks, comments, attachments []any
	if len(ids) > 0 {
		if o.has("checklists") {
			checklists = find("checklists", bson.M{"cardId": dbIn(ids)}, bySort)
			clIDs := bson.A{}
			for _, c := range checklists {
				clIDs = append(clIDs, cdString(cdGet(c, "_id")))
			}
			if len(clIDs) > 0 {
				items = find("checklistItems", bson.M{"checklistId": dbIn(clIDs)}, bySort)
			}
		}
		if o.has("subtasks") {
			subtasks = find("cards", bson.M{"boardId": b.ID, "$or": bson.A{bson.M{"parentId": dbIn(ids)}, bson.M{"parentIds": dbIn(ids)}}}, bySort)
		}
		if o.has("comments") {
			comments = find("card_comments", bson.M{"cardId": dbIn(ids)}, bson.D{{Key: "createdAt", Value: 1}})
		}
		if o.has("attachments") {
			attachments = find("attachments", bson.M{"meta.cardId": dbIn(ids), "deletedAt": nil}, bson.D{{Key: "uploadedAt", Value: -1}})
		}
	}
	defs := map[string]any{}
	if o.has("custom-fields") {
		fieldIDs := bson.A{}
		seen := map[string]bool{}
		for _, c := range cards {
			for _, f := range cdList(cdGet(c, "customFields")) {
				if id := cdOr(cdGet(f, "_id")); id != "" && !seen[id] {
					seen[id] = true
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
	// The people, by username, Admin Panel's anonymizing applied first.
	userIDs, seenUser := bson.A{}, map[string]bool{}
	addUser := func(v any) {
		if id := cdOr(v); id != "" && !seenUser[id] {
			seenUser[id] = true
			userIDs = append(userIDs, id)
		}
	}
	for _, m := range b.Members {
		addUser(m.UserID)
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
		for _, list := range [][]any{cards, subtasks} {
			var docs []bson.M
			for _, c := range list {
				docs = append(docs, bson.M(c.(map[string]any)))
			}
			anon.cards(docs)
		}
		var cms []bson.M
		for _, c := range comments {
			cms = append(cms, bson.M(c.(map[string]any)))
		}
		anon.comments(cms)
	}
	userMap := map[string]string{}
	for _, u := range users {
		userMap[strOf(u["_id"])] = strOf(u["username"])
	}
	userName := func(id string) string {
		if n := userMap[id]; n != "" {
			return n
		}
		return id
	}
	t := func(key string) string { return tr(o.Lang, key) }
	font := t("excel-font")

	// What the export is: the board, or the one swimlane or list.
	scopeTitle, scopeHeading := cdOr(cdGet(board, "title")), cdOr(cdGet(board, "title"))
	firstTitle := func(rows []any, key string) string {
		if len(rows) > 0 {
			if v := cdOr(cdGet(rows[0], "title")); v != "" {
				return v
			}
		}
		return tr(o.Lang, key)
	}
	switch {
	case o.Scope.ListID != "":
		scopeTitle = scopeTitle + " - " + firstTitle(lists, "list")
		scopeHeading = tr(o.Lang, "list") + ": " + firstTitle(lists, "list")
	case o.Scope.SwimlaneID != "":
		scopeTitle = scopeTitle + " - " + firstTitle(swimlanes, "swimlane")
		scopeHeading = tr(o.Lang, "swimlane") + ": " + firstTitle(swimlanes, "swimlane")
	}
	ws := newXSheet(excelSheetNameOf(scopeTitle))
	if only != nil && len(cards) > 0 {
		ws.Name = excelCardSheetName(cdOr(cdGet(cards[0], "title")))
	}
	ws.Widths = []float64{18, 30, 18, 30, 18, 30}
	row := 1
	var breaks []int
	r := func() string { return strconv.Itoa(row) }
	mergedRow := func(value any, f jsObject, fill jsObject, height float64) {
		ws.merge("A" + r() + ":F" + r())
		c := ws.cell("A" + r())
		c.Value = value
		ff := xObj("name", font, "size", 10)
		for _, kv := range f {
			ff.set(kv.Key, kv.Value)
		}
		c.Font = ff
		c.Alignment = xObj("vertical", "middle", "horizontal", "left", "wrapText", true)
		if fill != nil {
			c.Fill = fill
		}
		if height == 0 {
			height = 20
		}
		ws.Heights[row] = height
		row++
	}
	labelValue := func(label string, value any, rawDate any) {
		lc := ws.cell("A" + r())
		lc.Value = label + ":"
		lc.Font = xObj("name", font, "size", 10, "bold", true)
		lc.Alignment = xObj("vertical", "middle", "horizontal", "right")
		lc.Border = xBorder("thin")
		ws.merge("B" + r() + ":F" + r())
		vc := ws.cell("B" + r())
		if d, ok := rawDate.(time.Time); ok {
			vc.Value = jsDate(d)
			vc.NumFmt = "yyyy-mm-dd hh:mm"
		} else {
			vc.Value = value
		}
		vc.Font = xObj("name", font, "size", 10)
		vc.Alignment = xObj("vertical", "middle", "horizontal", "left", "wrapText", true)
		vc.Border = xBorder("thin")
		ws.Heights[row] = 20
		row++
	}
	if only == nil {
		mergedRow(scopeHeading, xObj("name", font, "size", 16, "bold", true), nil, 40)
	}
	if only == nil && o.Scope.ListID == "" && o.Scope.SwimlaneID == "" && o.has("board-header") {
		var names []string
		for _, m := range b.Members {
			if n := userName(m.UserID); n != "" {
				names = append(names, n)
			}
		}
		labelValue(t("members"), strings.Join(names, ", "), nil)
		labelValue(t("createdAt"), o.fmtDate(cdGet(board, "createdAt")), cdGet(board, "createdAt"))
		labelValue(t("modifiedAt"), o.fmtDate(cdGet(board, "modifiedAt")), cdGet(board, "modifiedAt"))
		if d := cdGet(board, "description"); cdTruthy(d) {
			labelValue(t("description"), d, nil)
		}
		row++
	}
	if o.has("card-details") || only != nil {
		listByID, laneByID := map[string]any{}, map[string]any{}
		for _, l := range lists {
			listByID[cdString(cdGet(l, "_id"))] = l
		}
		for _, l := range swimlanes {
			laneByID[cdString(cdGet(l, "_id"))] = l
		}
		cardFields := o.Fields
		if cardFields == nil {
			cardFields = []string{"labels", "people", "board-info", "dates", "description", "custom-fields", "checklists",
				"subtasks", "comments", "attachments", "voting", "poker", "stickers", "locations", "dependencies", "sort"}
		}
		subtasksOf := map[string][]any{}
		for _, st := range subtasks {
			seen := map[string]bool{}
			for _, id := range append([]any{cdGet(st, "parentId")}, cdList(cdGet(st, "parentIds"))...) {
				if pid := cdOr(id); validID(pid) && !seen[pid] {
					seen[pid] = true
					subtasksOf[pid] = append(subtasksOf[pid], st)
				}
			}
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
		accent := strings.ToUpper(strings.ReplaceAll(xThemeAccents[cdOr(cdGet(board, "color"))], "#", ""))
		heads := []string{"#", t("name"), t("size"), t("type"), t("export-card-attachment-uploaded-at"), t("export-card-attachment-uploaded-by")}
		drawCard := func(c, l0, lane0 any) {
			if row > 2 {
				breaks = append(breaks, row-1)
			}
			id := cdString(cdGet(c, "_id"))
			var images []cdImage
			for _, a := range attachmentsOf[id] {
				ext := excelEmbeddable[strings.ToLower(cdOr(cdGet(a, "type")))]
				if ext == "" || cdGet(a, "size") == int64(0) || cdGet(a, "size") == int32(0) || cdGet(a, "size") == 0.0 {
					continue
				}
				var att Attachment
				if s.db.Collection("attachments").FindOne(ctx, bson.M{"_id": eq(cdString(cdGet(a, "_id")))}).Decode(&att) != nil {
					continue
				}
				path, err := s.attachmentFile(&att)
				if err != nil {
					continue
				}
				data, err := os.ReadFile(path)
				if err != nil || len(data) == 0 {
					continue
				}
				name := cdOr(cdGet(a, "name"))
				if name == "" {
					name = cdOr(cdGet(cdGet(a, "meta"), "name"))
				}
				if name == "" {
					name = id
				}
				images = append(images, cdImage{Name: name, Size: formatExportFileSize(cdGet(a, "size")), Ext: ext, Data: data})
			}
			list := listByID[cdString(cdGet(c, "listId"))]
			if list == nil {
				list = l0
			}
			lane := laneByID[cdString(cdGet(c, "swimlaneId"))]
			if lane == nil {
				lane = lane0
			}
			doc := buildExportCardDocument(cardExportInput{Board: board, List: list, Card: c, Swimlane: lane,
				Checklists: checklistsOf[id], ChecklistItems: items, Subtasks: subtasksOf[id], Comments: commentsOf[id],
				Attachments: attachmentsOf[id], Images: images, CustomFieldsByID: defs},
				cardExportOptions{Fields: cardFields, UserName: userName, FormatDate: o.fmtDate,
					CustomValue: o.customFieldValueText,
					Translate: func(key, fallback string) string {
						if v := tr(o.Lang, key); v != "" {
							return v
						}
						return fallback
					}})
			next, br := renderCardDocumentExcel(ws, row, doc, cardExcelOptions{FontName: font, ProgressColor: accent, AttachmentHeadings: heads})
			row = next + 1
			breaks = append(breaks, br...)
		}
		if only != nil {
			for _, c := range cards {
				drawCard(c, nil, nil)
			}
			ws.Breaks = breaks
			return xlsxStyledBook([]*xSheet{ws})
		}
		type laneGroup struct {
			lane  any
			title string
		}
		var groups []laneGroup
		for _, l := range swimlanes {
			if cdString(cdGet(l, "type")) != "template-swimlane" {
				title := cdOr(cdGet(l, "title"))
				if title == "" {
					title = t("swimlane")
				}
				groups = append(groups, laneGroup{l, title})
			}
		}
		if len(groups) == 0 {
			groups = []laneGroup{{nil, t("swimlane")}}
		}
		// A list's export starts at the list: its cards, under no heading.
		if o.Scope.ListID != "" {
			groups = []laneGroup{{nil, ""}}
		}
		for _, g := range groups {
			if g.title != "" && o.Scope.SwimlaneID == "" {
				mergedRow(t("swimlane")+": "+g.title, xObj("name", font, "size", 12, "bold", true), xGray(), 22)
			}
			for _, l := range lists {
				var listCards []any
				for _, c := range cards {
					if cdString(cdGet(c, "listId")) == cdString(cdGet(l, "_id")) &&
						(g.lane == nil || cdString(cdGet(c, "swimlaneId")) == cdString(cdGet(g.lane, "_id"))) {
						listCards = append(listCards, c)
					}
				}
				title := cdOr(cdGet(l, "title"))
				if title == "" {
					title = t("list")
				}
				if o.Scope.ListID == "" {
					mergedRow(t("list")+": "+title+" ("+strconv.Itoa(len(listCards))+")", xObj("name", font, "size", 11, "bold", true), xGray(), 20)
				}
				for _, c := range listCards {
					drawCard(c, l, g.lane)
				}
			}
		}
	}
	ws.Breaks = breaks
	return xlsxStyledBook([]*xSheet{ws})
}

// excelFieldKeys are the keys the board's Excel export translates, named so
// sync-i18n copies them.
var excelFieldKeys = func(lang string) []string {
	return []string{tr(lang, "excel-font"), tr(lang, "name"), tr(lang, "size"), tr(lang, "type"),
		tr(lang, "export-card-attachment-uploaded-at"), tr(lang, "export-card-attachment-uploaded-by"), tr(lang, "modifiedAt"),
		tr(lang, "swimlane"), tr(lang, "list"), tr(lang, "members"), tr(lang, "createdAt"), tr(lang, "description")}
}

// boardHasAdminOnlyFields is whether a custom field of the board is for its
// admins only (adminOnlyCustomFields.js).
func (s *Server) boardHasAdminOnlyFields(ctx context.Context, b *Board) bool {
	return s.db.Collection("customFields").FindOne(ctx, bson.M{"boardIds": b.ID, "adminOnly": true}).Err() == nil
}

// excelCardSheetName is ExporterExcelCard's sanitizeSheetName.
func excelCardSheetName(v string) string {
	if v == "" {
		v = "Card"
	}
	v = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\\/*?:[]`, r) {
			return '-'
		}
		return r
	}, v)
	return jsSlice16(v, 31)
}
