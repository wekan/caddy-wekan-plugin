package web

import (
	"context"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A board's Excel export in the layout of Node.js WeKan's table exporter
// (models/server/ExporterExcel.js): the board's sheet with its title in A1,
// its description in row 3, its dates and members in row 5 and a card a row
// under the header in row 7, and an Activity sheet of its comments. Every
// header is in the exporting user's language - which WeKan's Excel import
// reads back in any language (import_sheets.go).

// excelTableDate is addTZhours: a date cell, or " " for none.
func excelTableDate(v any) any {
	switch x := v.(type) {
	case bson.DateTime:
		return jsDate(x.Time().UTC())
	case time.Time:
		return jsDate(x.UTC())
	case string:
		if x == "" {
			return " "
		}
		if t, ok := jsDateTime(x); ok {
			return jsDate(t)
		}
	case nil:
		return " "
	}
	return " "
}

// excelSheetTitle is ExporterExcel's worksheet title: a title over 31
// characters cut at a word before 27, and "...".
func excelSheetTitle(title string) string {
	if len([]rune(title)) <= 31 {
		return xlsxSheetName(title, "Board")
	}
	words := strings.Split(title, " ")
	tmp := ""
	for _, w := range words {
		if len([]rune(words[0])) > 27 {
			tmp = string([]rune(words[0])[:27]) + " "
			break
		}
		if len([]rune(tmp))+len([]rune(w)) < 27 {
			tmp += w + " "
		} else {
			break
		}
	}
	return xlsxSheetName(tmp+"...", "Board")
}

// excelTableWorkbook is ExporterExcel.build for one board.
func (s *Server) excelTableWorkbook(ctx context.Context, b *Board, lang string) ([]byte, error) {
	find := func(coll string, filter bson.M) []bson.M {
		docs := []bson.M{}
		if cur, err := s.db.Collection(coll).Find(ctx, filter); err == nil {
			_ = cur.All(ctx, &docs)
		}
		return docs
	}
	var board bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&board)
	lists, swimlanes := find("lists", bson.M{"boardId": b.ID}), find("swimlanes", bson.M{"boardId": b.ID})
	jlist, jswimlane := map[string]string{}, map[string]string{}
	for _, l := range lists {
		jlist[strOf(l["_id"])] = strOf(l["title"])
	}
	for _, l := range swimlanes {
		jswimlane[strOf(l["_id"])] = strOf(l["title"])
	}
	// The first label's name, and every other's with a comma before it.
	jlabel := map[string]string{}
	for i, l := range b.Labels {
		if i == 0 {
			jlabel[l.ID] = l.Name
		} else {
			jlabel[l.ID] = "," + l.Name
		}
	}
	cards := find("cards", bson.M{"boardId": b.ID, "linkedId": bson.M{"$in": bson.A{"", nil}}})
	cardTitle := map[string]string{}
	ids := bson.A{}
	userIDs, seen := bson.A{}, map[string]bool{}
	addUser := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			userIDs = append(userIDs, id)
		}
	}
	for _, m := range b.Members {
		addUser(m.UserID)
	}
	for _, l := range lists {
		addUser(strOf(l["userId"]))
	}
	for _, c := range cards {
		cardTitle[strOf(c["_id"])] = strOf(c["title"])
		ids = append(ids, strOf(c["_id"]))
		addUser(strOf(c["userId"]))
		for _, k := range []string{"members", "assignees", "requesters", "assigners"} {
			for _, id := range stringsOf(c[k]) {
				addUser(id)
			}
		}
	}
	var comments []bson.M
	if len(ids) > 0 {
		comments = find("card_comments", bson.M{"cardId": dbIn(ids)})
	}
	for _, c := range comments {
		addUser(strOf(c["userId"]))
	}
	var users []bson.M
	if len(userIDs) > 0 {
		users = find("users", bson.M{"_id": dbIn(userIDs)})
	}
	// Admin Panel -> Anonymize export users: the names, and what the cards
	// and comments say about them.
	anon := s.exportAnonymizer(ctx, b, users)
	if anon != nil {
		for _, u := range users {
			anon.user(u)
		}
		anon.cards(cards)
		anon.comments(comments)
	}
	jmeml := map[string]string{}
	names := []string{}
	for _, u := range users {
		jmeml[strOf(u["_id"])] = strOf(u["username"])
		names = append(names, strOf(u["username"]))
	}
	// JavaScript's string + undefined: a name nobody has is "undefined".
	name := func(id string) string {
		if n, ok := jmeml[id]; ok {
			return n
		}
		return "undefined"
	}
	nameOrNil := func(id string) any {
		if n, ok := jmeml[id]; ok {
			return n
		}
		return nil
	}
	t := func(key string) string { return tr(lang, key) }
	rows := [][]any{
		{strOf(board["title"])},
		{},
		{t("description"), strOf(board["description"])},
		{},
		{t("createdAt"), excelTableDate(board["createdAt"]), t("modifiedAt"), excelTableDate(board["modifiedAt"]), t("members"), strings.Join(names, ",")},
		{},
		{t("number"), t("title"), t("description"), t("parent-card"), t("owner"), t("createdAt"), t("last-modified-at"),
			t("card-received"), t("card-start"), t("card-due"), t("card-end"), t("list"), t("swimlane"), t("assignee"),
			t("members"), t("requested-by"), t("assigned-by"), t("labels"), t("overtime-hours"), t("spent-time-hours")},
	}
	for n, c := range cards {
		people := func(field string) string {
			out := ""
			for _, id := range stringsOf(c[field]) {
				out += name(id) + " "
			}
			return out
		}
		labels := ""
		for _, id := range stringsOf(c["labelIds"]) {
			if l, ok := jlabel[id]; ok {
				labels += l + " "
			} else {
				labels += "undefined "
			}
		}
		var parent any
		if p := strOf(c["parentId"]); p != "" {
			parent = cardTitle[p]
		}
		by := func(ids, text string) string {
			var parts []string
			for _, id := range stringsOf(c[ids]) {
				if n, ok := jmeml[id]; ok && n != "" {
					parts = append(parts, n)
				} else if id != "" {
					parts = append(parts, id)
				}
			}
			if v := strOf(c[text]); v != "" {
				parts = append(parts, v)
			}
			return strings.Join(parts, ", ")
		}
		overtime := "false"
		if c["isOvertime"] == true {
			overtime = "true"
		}
		var spent any
		switch v := plain(c["spentTime"]).(type) {
		case float64:
			spent = v
		case int32:
			spent = float64(v)
		case int64:
			spent = float64(v)
		}
		var title, description any
		if v, ok := c["title"].(string); ok {
			title = v
		}
		if v, ok := c["description"].(string); ok {
			description = v
		}
		rows = append(rows, []any{strconv.Itoa(n + 1), title, description, parent, nameOrNil(strOf(c["userId"])),
			excelTableDate(c["createdAt"]), excelTableDate(c["dateLastActivity"]), excelTableDate(c["receivedAt"]),
			excelTableDate(c["startAt"]), excelTableDate(c["dueAt"]), excelTableDate(c["endAt"]),
			jlist[strOf(c["listId"])], jswimlane[strOf(c["swimlaneId"])], people("assignees"), people("members"),
			by("requesters", "requestedBy"), by("assigners", "assignedBy"), labels, overtime, spent})
	}
	activity := [][]any{
		{strOf(board["title"])},
		{},
		{t("number"), t("activity"), t("card"), t("owner"), t("createdAt"), t("last-modified-at")},
	}
	count := 0
	for _, cm := range comments {
		title := cardTitle[strOf(cm["cardId"])]
		if anon != nil {
			title = anon.text(title)
		}
		if title == "" {
			continue
		}
		count++
		owner := any(strOf(cm["userId"]))
		if n, ok := jmeml[strOf(cm["userId"])]; ok && n != "" {
			owner = n
		}
		var text any
		if v, ok := cm["text"].(string); ok {
			text = v
		}
		activity = append(activity, []any{strconv.Itoa(count), text, title, owner,
			excelTableDate(cm["createdAt"]), excelTableDate(cm["modifiedAt"])})
	}
	return xlsxBook([]xlsxSpec{
		{Name: excelSheetTitle(strOf(board["title"])), Rows: rows, BoldRows: []int{5, 7}, Merges: []string{"A1:H1", "B3:H3", "F5:R5"}, DateTime: true},
		{Name: xlsxSheetName(t("activity"), "Activity"), Rows: activity, BoldRows: []int{3}, Merges: []string{"A1:F1"}, DateTime: true},
	})
}
