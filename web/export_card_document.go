package web

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// A card as a document, as Node.js WeKan's exports draw it
// (models/lib/cardDocument.js and cardExportDocument.js): a title, meta rows
// of label/value pairs three to a row, then a section per part of the card
// that has something in it - stickers, locations, dependencies, description,
// custom fields, checklists, subtasks, comments, attachments, voting, poker -
// in blocks that name no colour, column or font. A renderer draws them
// (export_card_excel.go). The records are plain() values: maps, arrays,
// time.Time dates. TestCardDocumentIsWeKans checks it against WeKan.

// cdBlock is a block of the document; only the fields of its type are set.
type cdBlock struct {
	Type         string      `json:"type"`
	Runs         []mdRun     `json:"runs,omitempty"`
	Pairs        [][]any     `json:"pairs,omitempty"`
	LabelTitle   string      `json:"labelTitle,omitempty"`
	LabelDetails []any       `json:"labelDetails,omitempty"`
	Key          string      `json:"key,omitempty"`
	Title        string      `json:"title,omitempty"`
	Blocks       []mdBlock   `json:"blocks,omitempty"`
	Items        []cdItem    `json:"items,omitempty"`
	Header       []string    `json:"header,omitempty"`
	Rows         [][][]mdRun `json:"rows,omitempty"`
	Images       []cdImage   `json:"images,omitempty"`
	Progress     *cdProgress `json:"progress,omitempty"`
}

// MarshalJSON writes a block's own keys only, as WeKan's blocks have them:
// a title's runs even when there are none.
func (b cdBlock) MarshalJSON() ([]byte, error) {
	o := jsObject{}
	o.set("type", b.Type)
	switch b.Type {
	case "title", "note":
		o.set("runs", orEmpty(b.Runs))
		if b.Progress != nil {
			o.set("progress", b.Progress)
		}
	case "meta":
		o.set("pairs", b.Pairs)
		if b.LabelTitle != "" {
			o.set("labelTitle", b.LabelTitle)
			o.set("labelDetails", b.LabelDetails)
		}
	case "section":
		o.set("key", b.Key)
		o.set("title", b.Title)
	case "text":
		o.set("blocks", b.Blocks)
	case "list":
		items := b.Items
		if items == nil {
			items = []cdItem{}
		}
		o.set("items", items)
	case "rows":
		o.set("header", b.Header)
		o.set("rows", b.Rows)
	case "images":
		o.set("images", b.Images)
	}
	return o.MarshalJSON()
}

func orEmpty(r []mdRun) []mdRun {
	if r == nil {
		return []mdRun{}
	}
	return r
}

type cdItem struct {
	Runs       []mdRun       `json:"runs"`
	Done       *bool         `json:"done,omitempty"`
	Marker     string        `json:"marker"`
	Level      int           `json:"level"`
	Attachment *cdAttachment `json:"attachment,omitempty"`
}

type cdProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

type cdAttachment struct {
	Name     string `json:"name"`
	Size     string `json:"size"`
	Type     string `json:"type"`
	Uploaded string `json:"uploaded"`
	Uploader string `json:"uploader"`
}

// cdImage is an embedded picture: its name, size text, extension and bytes.
type cdImage struct {
	Name string `json:"name"`
	Size string `json:"size"`
	Ext  string `json:"ext"`
	Data []byte `json:"-"`
}

// cardDocFallbacks is FALLBACKS: the English word of each label.
var cardDocFallbacks = map[string]string{
	"board": "Board", "list": "List", "swimlane": "Swimlane", "card-number": "Card number",
	"creator": "Created by", "members": "Members", "assignees": "Assignees",
	"requested-by": "Requested by", "assigned-by": "Assigned by",
	"createdAt": "Created at", "card-received": "Received", "card-start": "Start",
	"card-due": "Due", "card-end": "End", "last-activity": "Last activity",
	"card-spent": "Spent time", "overtime": "Overtime", "labels": "Labels",
	"description": "Description", "custom-fields": "Custom fields",
	"checklists": "Checklists", "export-card-subtasks": "Subtasks",
	"comments": "Comments", "attachments": "Attachments", "voting": "Voting",
	"poker-question": "Poker", "date": "Date", "comment": "Comment",
	"stickers": "Stickers", "location": "Location", "card-dependencies": "Dependencies",
	"sort": "Sort", "location-name": "Location name", "location-address": "Address",
	"location-latitude": "Latitude", "location-longitude": "Longitude",
}

// --- plain() values as JavaScript reads them

func cdGet(v any, k string) any {
	if m, ok := v.(map[string]any); ok {
		if x, ok := m[k]; ok {
			return x
		}
	}
	return undefinedValue{}
}

func cdTruthy(v any) bool {
	switch x := v.(type) {
	case nil, undefinedValue:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int32:
		return x != 0
	case int64:
		return x != 0
	case int:
		return x != 0
	}
	return true
}

// cdString is JavaScript's String(v).
func cdString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case undefinedValue:
		return "undefined"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return jsNumber(x)
	case int32:
		return strconv.Itoa(int(x))
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case time.Time:
		return jsDateString(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil && !isUndef(e) {
				parts[i] = cdString(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// cdOr is (v || ”) as text.
func cdOr(v any) string {
	if !cdTruthy(v) {
		return ""
	}
	return cdString(v)
}

func cdList(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func cdIsNumber(v any) bool {
	switch v.(type) {
	case float64, int32, int64, int:
		return true
	}
	return false
}

func cdPlainRuns(text string) []mdRun {
	if text == "" {
		return []mdRun{}
	}
	return []mdRun{{Text: text}}
}

func cdWanted(fields []string, key string) bool {
	if len(fields) == 0 {
		return true
	}
	for _, f := range fields {
		if f == key {
			return true
		}
	}
	return false
}

// cardDocData is the data argument of buildCardDocument.
type cardDocData struct {
	BoardTitle, ListTitle, SwimlaneTitle                                string
	CardNumber                                                          any
	Labels                                                              []string
	LabelDetails                                                        []any
	CreatedBy                                                           string
	Members, Assignees, Requesters, Assigners                           []string
	RequestedBy, AssignedBy                                             string
	CreatedAt, ModifiedAt, ReceivedAt, StartAt, DueAt, EndAt, SpentTime string
	Overtime                                                            string
	CustomFields                                                        [][2]any
	Checklists                                                          []cdChecklist
	Subtasks                                                            []any
	Comments                                                            []cdComment
	Attachments                                                         []cdAttachment
	Images                                                              []cdImage
	Voting, Poker                                                       [][]any
}

type cdChecklist struct {
	Title string
	Items []any
}

type cdComment struct{ Date, Author, Text string }

// cardLocations is cardLocations: the card's locations, or its one old
// location field.
func cardLocations(card any) []any {
	if l := cdList(cdGet(card, "locations")); len(l) > 0 {
		return l
	}
	if cdTruthy(cdGet(card, "locationName")) || cdTruthy(cdGet(card, "locationAddress")) ||
		cdIsNumber(cdGet(card, "locationLatitude")) || cdIsNumber(cdGet(card, "locationLongitude")) {
		return []any{map[string]any{"name": cdOr(cdGet(card, "locationName")), "address": cdOr(cdGet(card, "locationAddress")),
			"latitude": cdGet(card, "locationLatitude"), "longitude": cdGet(card, "locationLongitude")}}
	}
	return nil
}

// cardHasSectionData is hasSectionData.
func cardHasSectionData(key string, card any, d *cardDocData) bool {
	switch key {
	case "description":
		return jsTrim(cdOr(cdGet(card, "description"))) != ""
	case "custom-fields":
		for _, f := range d.CustomFields {
			if v := f[1]; v != nil && !isUndef(v) && jsTrim(cdString(v)) != "" {
				return true
			}
		}
		return false
	case "checklists":
		for _, c := range d.Checklists {
			if jsTrim(c.Title) != "" || len(c.Items) > 0 {
				return true
			}
		}
		return false
	case "subtasks":
		return len(d.Subtasks) > 0
	case "comments":
		for _, c := range d.Comments {
			if jsTrim(c.Text) != "" {
				return true
			}
		}
		return false
	case "attachments":
		return len(d.Attachments) > 0 || len(d.Images) > 0
	case "voting":
		return len(d.Voting) > 0
	case "poker":
		return len(d.Poker) > 0
	case "stickers":
		return len(cdList(cdGet(card, "stickers"))) > 0
	case "locations":
		return len(cardLocations(card)) > 0
	case "dependencies":
		return len(cdList(cdGet(card, "cardDependencies"))) > 0
	}
	return true
}

// buildCardDocument is buildCardDocument.
func buildCardDocument(card any, d *cardDocData, fields []string, translate func(key, fallback string) string) []cdBlock {
	t := func(key string) string {
		fb := cardDocFallbacks[key]
		if fb == "" {
			fb = key
		}
		if out := translate(key, fb); out != "" {
			return out
		}
		return fb
	}
	blocks := []cdBlock{{Type: "title", Runs: cdPlainRuns(cdOr(cdGet(card, "title")))}}
	var pairs [][]any
	add := func(label string, value any, raw any) {
		if value == nil || isUndef(value) || value == "" {
			return
		}
		pair := []any{t(label), cdString(value)}
		if tm, ok := raw.(time.Time); ok {
			pair = append(pair, jsDate(tm))
		}
		pairs = append(pairs, pair)
	}
	joinNonEmpty := func(parts []string) string {
		var out []string
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, ", ")
	}
	if cdWanted(fields, "board-info") {
		add("board", d.BoardTitle, nil)
		add("list", d.ListTitle, nil)
		add("swimlane", d.SwimlaneTitle, nil)
		add("card-number", d.CardNumber, nil)
	}
	if cdWanted(fields, "people") {
		add("creator", d.CreatedBy, nil)
		add("members", strings.Join(d.Members, ", "), nil)
		add("assignees", strings.Join(d.Assignees, ", "), nil)
		add("requested-by", joinNonEmpty(append(append([]string{}, d.Requesters...), d.RequestedBy)), nil)
		add("assigned-by", joinNonEmpty(append(append([]string{}, d.Assigners...), d.AssignedBy)), nil)
	}
	if cdWanted(fields, "dates") {
		add("createdAt", d.CreatedAt, cdGet(card, "createdAt"))
		add("card-received", d.ReceivedAt, cdGet(card, "receivedAt"))
		add("card-start", d.StartAt, cdGet(card, "startAt"))
		add("card-due", d.DueAt, cdGet(card, "dueAt"))
		add("card-end", d.EndAt, cdGet(card, "endAt"))
		add("last-activity", d.ModifiedAt, cdGet(card, "modifiedAt"))
		add("card-spent", d.SpentTime, nil)
		add("overtime", d.Overtime, nil)
	}
	if cdWanted(fields, "labels") {
		add("labels", strings.Join(d.Labels, ", "), nil)
	}
	if cdWanted(fields, "sort") {
		add("sort", cdGet(card, "sort"), nil)
	}
	for i := 0; i < len(pairs); i += 3 {
		slice := pairs[i:min(i+3, len(pairs))]
		b := cdBlock{Type: "meta", Pairs: slice}
		for _, p := range slice {
			if p[0] == t("labels") && len(d.LabelDetails) > 0 {
				b.LabelTitle, b.LabelDetails = t("labels"), d.LabelDetails
			}
		}
		blocks = append(blocks, b)
	}
	section := func(key, title string) { blocks = append(blocks, cdBlock{Type: "section", Key: key, Title: t(title)}) }
	has := func(key string) bool { return cdWanted(fields, key) && cardHasSectionData(key, card, d) }

	if has("stickers") {
		section("stickers", "stickers")
		var items []cdItem
		for _, s := range cdList(cdGet(card, "stickers")) {
			items = append(items, cdItem{Runs: cdPlainRuns(joinDash(cdOr(cdGet(s, "name")), cdOr(cdGet(s, "icon")), cdOr(cdGet(s, "color")), cdOr(cdGet(s, "highlight")))), Marker: "-"})
		}
		blocks = append(blocks, cdBlock{Type: "list", Items: items})
	}
	if has("locations") {
		section("locations", "location")
		for i, l := range cardLocations(card) {
			blocks = append(blocks, cdBlock{Type: "note", Runs: cdPlainRuns(t("location") + " " + strconv.Itoa(i+1))})
			var ps [][]any
			for _, kv := range [][2]string{{"location-name", "name"}, {"location-address", "address"}, {"location-latitude", "latitude"}, {"location-longitude", "longitude"}} {
				if v := cdGet(l, kv[1]); v != nil && !isUndef(v) && v != "" {
					ps = append(ps, []any{t(kv[0]), v})
				}
			}
			for at := 0; at < len(ps); at += 3 {
				blocks = append(blocks, cdBlock{Type: "meta", Pairs: ps[at:min(at+3, len(ps))]})
			}
		}
	}
	if has("dependencies") {
		section("dependencies", "card-dependencies")
		var items []cdItem
		for _, dep := range cdList(cdGet(card, "cardDependencies")) {
			value := ""
			if s, ok := dep.(string); ok {
				value = s
			} else {
				value = joinDash(cdOr(cdGet(dep, "cardId")), cdOr(cdGet(dep, "type")), cdOr(cdGet(dep, "icon")), cdOr(cdGet(dep, "color")))
			}
			items = append(items, cdItem{Runs: cdPlainRuns(value), Marker: "-"})
		}
		blocks = append(blocks, cdBlock{Type: "list", Items: items})
	}
	if has("description") {
		section("description", "description")
		blocks = append(blocks, cdBlock{Type: "text", Blocks: markdownBlocks(cdOr(cdGet(card, "description")))})
	}
	if has("custom-fields") {
		section("custom-fields", "custom-fields")
		var ps [][]any
		for _, f := range d.CustomFields {
			if v := f[1]; v != nil && !isUndef(v) && jsTrim(cdString(v)) != "" {
				ps = append(ps, []any{f[0], v})
			}
		}
		for i := 0; i < len(ps); i += 3 {
			blocks = append(blocks, cdBlock{Type: "meta", Pairs: ps[i:min(i+3, len(ps))]})
		}
	}
	itemRuns := func(source string) []mdRun {
		runs := []mdRun{}
		for _, b := range markdownBlocks(source) {
			runs = append(runs, b.Runs...)
		}
		return runs
	}
	if has("checklists") {
		section("checklists", "checklists")
		for _, c := range d.Checklists {
			done := 0
			for _, it := range c.Items {
				if cdTruthy(it) && cdTruthy(cdGet(it, "isFinished")) {
					done++
				}
			}
			blocks = append(blocks, cdBlock{Type: "note", Runs: []mdRun{{Text: c.Title, Bold: true}}, Progress: &cdProgress{done, len(c.Items)}})
			items := []cdItem{}
			for _, it := range c.Items {
				fin := cdTruthy(cdGet(it, "isFinished"))
				marker := "[ ]"
				if fin {
					marker = "[x]"
				}
				items = append(items, cdItem{Runs: itemRuns(cdOr(cdGet(it, "title"))), Done: &fin, Marker: marker})
			}
			blocks = append(blocks, cdBlock{Type: "list", Items: items})
		}
	}
	if has("subtasks") {
		section("subtasks", "export-card-subtasks")
		items := []cdItem{}
		for _, s := range d.Subtasks {
			arch := cdTruthy(cdGet(s, "archived"))
			marker := "[ ]"
			if arch {
				marker = "[x]"
			}
			items = append(items, cdItem{Runs: cdPlainRuns(cdOr(cdGet(s, "title"))), Done: &arch, Marker: marker})
		}
		blocks = append(blocks, cdBlock{Type: "list", Items: items})
	}
	if has("comments") {
		section("comments", "comments")
		rows := [][][]mdRun{}
		for _, c := range d.Comments {
			text := []mdRun{}
			if c.Author != "" {
				text = append(text, mdRun{Text: c.Author + ": ", Bold: true})
			}
			text = append(text, itemRuns(c.Text)...)
			rows = append(rows, [][]mdRun{cdPlainRuns(c.Date), text})
		}
		blocks = append(blocks, cdBlock{Type: "rows", Header: []string{t("date"), t("comment")}, Rows: rows})
	}
	if has("attachments") {
		section("attachments", "attachments")
		items := []cdItem{}
		for i := range d.Attachments {
			a := d.Attachments[i]
			text := a.Name
			if a.Size != "" {
				text = a.Name + " (" + a.Size + ")"
			}
			no := false
			items = append(items, cdItem{Runs: cdPlainRuns(text), Done: &no, Marker: "-", Attachment: &a})
		}
		blocks = append(blocks, cdBlock{Type: "list", Items: items})
		var images []cdImage
		for _, img := range d.Images {
			if len(img.Data) > 0 {
				images = append(images, img)
			}
		}
		if len(images) > 0 {
			blocks = append(blocks, cdBlock{Type: "images", Images: images})
		}
	}
	if has("voting") {
		section("voting", "voting")
		blocks = append(blocks, cdBlock{Type: "meta", Pairs: d.Voting})
	}
	if has("poker") {
		section("poker", "poker-question")
		blocks = append(blocks, cdBlock{Type: "meta", Pairs: d.Poker})
	}
	return blocks
}

func joinDash(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " - ")
}

// formatExportFileSize is formatExportFileSize.
func formatExportFileSize(v any) string {
	var size float64
	switch x := v.(type) {
	case float64:
		size = x
	case int32:
		size = float64(x)
	case int64:
		size = float64(x)
	case int:
		size = float64(x)
	case string:
		f, err := strconv.ParseFloat(jsTrim(x), 64)
		if err != nil {
			return "0 B"
		}
		size = f
	default:
		return "0 B"
	}
	if math.IsNaN(size) || math.IsInf(size, 0) || size <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	index := min(int(math.Floor(math.Log2(size)/10)), len(units)-1)
	value := size / math.Pow(1024, float64(index))
	if index == 0 {
		return jsNumber(value) + " " + units[index]
	}
	return jsToFixed1(value) + " " + units[index]
}

// jsToFixed1 is Number.prototype.toFixed(1).
func jsToFixed1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// cardExportInput is buildExportCardDocument's data.
type cardExportInput struct {
	Board, List, Card, Swimlane any
	Checklists, ChecklistItems  []any
	Subtasks, Comments          []any
	Attachments                 []any
	Images                      []cdImage
	CustomFieldsByID            map[string]any
}

// cardExportOptions are its options.
type cardExportOptions struct {
	Fields      []string
	UserName    func(id string) string
	FormatDate  func(v any) string
	Translate   func(key, fallback string) string
	CustomValue func(definition, value any) any
}

// buildExportCardDocument is buildExportCardDocument: the records an
// exporter loaded, as the card's document.
func buildExportCardDocument(in cardExportInput, o cardExportOptions) []cdBlock {
	card := in.Card
	name := func(id any) string { return o.UserName(cdOr(id)) }
	names := func(ids any) []string {
		var out []string
		for _, id := range cdList(ids) {
			if n := name(id); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	labelsByID := map[string]any{}
	for _, l := range cdList(cdGet(in.Board, "labels")) {
		if id := cdOr(cdGet(l, "_id")); id != "" {
			labelsByID[id] = l
		}
	}
	var labels []string
	var details []any
	for _, id := range cdList(cdGet(card, "labelIds")) {
		l, ok := labelsByID[cdString(id)]
		if !ok {
			continue
		}
		details = append(details, l)
		n := cdOr(cdGet(l, "name"))
		if n == "" {
			n = cdOr(cdGet(l, "color"))
		}
		if n == "" {
			n = cdOr(cdGet(l, "_id"))
		}
		labels = append(labels, n)
	}
	date := func(v any) string { return o.FormatDate(v) }
	modified := cdGet(card, "dateLastActivity")
	if !cdTruthy(modified) {
		modified = cdGet(card, "modifiedAt")
	}
	spent := cdGet(card, "spentTime")
	d := &cardDocData{
		BoardTitle: cdOr(cdGet(in.Board, "title")), ListTitle: cdOr(cdGet(in.List, "title")), SwimlaneTitle: cdOr(cdGet(in.Swimlane, "title")),
		CardNumber: cdGet(card, "cardNumber"), Labels: labels, LabelDetails: details,
		CreatedBy: name(cdGet(card, "userId")), Members: names(cdGet(card, "members")), Assignees: names(cdGet(card, "assignees")),
		RequestedBy: cdOr(cdGet(card, "requestedBy")), AssignedBy: cdOr(cdGet(card, "assignedBy")),
		Requesters: names(cdGet(card, "requesters")), Assigners: names(cdGet(card, "assigners")),
		CreatedAt: date(cdGet(card, "createdAt")), ModifiedAt: date(modified), ReceivedAt: date(cdGet(card, "receivedAt")),
		StartAt: date(cdGet(card, "startAt")), DueAt: date(cdGet(card, "dueAt")), EndAt: date(cdGet(card, "endAt")),
	}
	if spent != nil && !isUndef(spent) {
		d.SpentTime = cdString(spent)
	}
	if cdTruthy(spent) {
		if cdTruthy(cdGet(card, "isOvertime")) {
			d.Overtime = o.Translate("yes", "Yes")
		} else {
			d.Overtime = o.Translate("no", "No")
		}
	}
	for _, f := range cdList(cdGet(card, "customFields")) {
		id := cdOr(cdGet(f, "_id"))
		def, ok := in.CustomFieldsByID[id]
		if !cdTruthy(f) || id == "" || !ok || !cdTruthy(def) {
			continue
		}
		d.CustomFields = append(d.CustomFields, [2]any{cdGet(def, "name"), o.CustomValue(def, cdGet(f, "value"))})
	}
	for _, c := range in.Checklists {
		var items []any
		for _, it := range in.ChecklistItems {
			if cdString(cdGet(it, "checklistId")) == cdString(cdGet(c, "_id")) {
				items = append(items, it)
			}
		}
		d.Checklists = append(d.Checklists, cdChecklist{Title: cdOr(cdGet(c, "title")), Items: items})
	}
	d.Subtasks = in.Subtasks
	for _, c := range in.Comments {
		d.Comments = append(d.Comments, cdComment{Date: date(cdGet(c, "createdAt")), Author: name(cdGet(c, "userId")), Text: cdOr(cdGet(c, "text"))})
	}
	for _, a := range in.Attachments {
		n := cdOr(cdGet(a, "name"))
		if n == "" {
			n = cdOr(cdGet(cdGet(a, "meta"), "name"))
		}
		if n == "" {
			n = cdOr(cdGet(a, "_id"))
		}
		up := cdGet(a, "uploadedAt")
		if !cdTruthy(up) {
			up = cdGet(a, "uploadedAtOstrio")
		}
		if !cdTruthy(up) {
			up = cdGet(a, "createdAt")
		}
		by := cdGet(a, "userId")
		if !cdTruthy(by) {
			by = cdGet(cdGet(a, "meta"), "userId")
		}
		d.Attachments = append(d.Attachments, cdAttachment{Name: n, Size: formatExportFileSize(cdGet(a, "size")),
			Type: cdOr(cdGet(a, "type")), Uploaded: date(up), Uploader: name(by)})
	}
	d.Images = in.Images
	vote, poker := cdGet(card, "vote"), cdGet(card, "poker")
	if cdTruthy(cdGet(vote, "question")) {
		d.Voting = [][]any{{o.Translate("vote-question", "Vote question"), cdGet(vote, "question")},
			{o.Translate("vote-for-it", "For"), strconv.Itoa(len(cdList(cdGet(vote, "positive"))))},
			{o.Translate("vote-against", "Against"), strconv.Itoa(len(cdList(cdGet(vote, "negative"))))}}
	}
	if est := cdGet(poker, "estimation"); cdTruthy(cdGet(poker, "question")) || !isUndef(est) {
		q := ""
		if cdTruthy(cdGet(poker, "question")) {
			q = o.Translate("yes", "Yes")
		}
		e := ""
		if !isUndef(est) {
			e = cdString(est)
		}
		d.Poker = [][]any{{o.Translate("poker-question", "Poker"), q}, {o.Translate("poker-estimation", "Estimation"), e}}
	}
	return buildCardDocument(card, d, o.Fields, o.Translate)
}

// cardDocTranslate is the document's translate in lang: the language's own
// word, else WeKan's fallback. The keys are named here so sync-i18n copies
// them.
func cardDocTranslate(lang string) func(key, fallback string) string {
	translations.load()
	table := translations.table(lang)
	return func(key, fallback string) string {
		if v := table[key]; v != "" {
			return v
		}
		return fallback
	}
}

// cardDocKeys are every key the document asks for.
var cardDocKeys = func(lang string) []string {
	return []string{tr(lang, "board"), tr(lang, "list"), tr(lang, "swimlane"), tr(lang, "card-number"), tr(lang, "creator"),
		tr(lang, "members"), tr(lang, "assignees"), tr(lang, "requested-by"), tr(lang, "assigned-by"), tr(lang, "createdAt"),
		tr(lang, "card-received"), tr(lang, "card-start"), tr(lang, "card-due"), tr(lang, "card-end"), tr(lang, "last-activity"),
		tr(lang, "card-spent"), tr(lang, "overtime"), tr(lang, "labels"), tr(lang, "description"), tr(lang, "custom-fields"),
		tr(lang, "checklists"), tr(lang, "export-card-subtasks"), tr(lang, "comments"), tr(lang, "attachments"), tr(lang, "voting"),
		tr(lang, "poker-question"), tr(lang, "date"), tr(lang, "comment"), tr(lang, "stickers"), tr(lang, "location"),
		tr(lang, "card-dependencies"), tr(lang, "sort"), tr(lang, "location-name"), tr(lang, "location-address"),
		tr(lang, "location-latitude"), tr(lang, "location-longitude"), tr(lang, "yes"), tr(lang, "no"), tr(lang, "vote-question"),
		tr(lang, "vote-for-it"), tr(lang, "vote-against")}
}
