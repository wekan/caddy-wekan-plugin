package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var errTrelloDocument = errors.New("error-json-schema")

func trelloFieldImport(item bson.M, kind string) (any, bool) {
	value := asM(item["value"])
	switch kind {
	case "dropdown":
		id := str(item, "idValue")
		return id, id != ""
	case "checkbox":
		switch checked := value["checked"].(type) {
		case bool:
			return checked, true
		case string:
			if checked == "true" || checked == "false" {
				return checked == "true", true
			}
		}
	case "number":
		if number, err := strconv.ParseFloat(cdString(value["number"]), 64); err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
			return number, true
		}
	case "date":
		if date, ok := jsParseDate(str(value, "date")); ok {
			return date.UTC().Format("2006-01-02T15:04:05.000Z"), true
		}
	case "text":
		text, ok := value["text"].(string)
		return text, ok
	}
	return nil, false
}

// Translate a Trello document into the native importer's board and relation
// records. File data stays in the archive until the native importer needs it.
func trelloImport(raw []byte) (map[string]any, error) {
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil || strings.TrimSpace(str(input, "name")) == "" {
		return nil, errTrelloDocument
	}
	lists, listsOK := input["lists"].([]any)
	cards, cardsOK := input["cards"].([]any)
	if !listsOK || !cardsOK {
		return nil, errTrelloDocument
	}
	out := map[string]any{"title": str(input, "name"), "description": str(input, "desc")}
	add := func(key string, row map[string]any) {
		rows, _ := out[key].([]any)
		out[key] = append(rows, row)
	}
	for _, value := range lists {
		list, ok := value.(map[string]any)
		if !ok || str(list, "id") == "" {
			return nil, errTrelloDocument
		}
		add("lists", map[string]any{"_id": list["id"], "title": str(list, "name"), "sort": list["pos"], "archived": list["closed"] == true})
	}
	for _, label := range docsOf(input, "labels") {
		add("labels", map[string]any{"_id": str(label, "id"), "name": str(label, "name"), "color": str(label, "color")})
	}
	fieldKinds := map[string]string{}
	for _, field := range docsOf(input, "customFields") {
		id, name, kind := str(field, "id"), str(field, "name"), str(field, "type")
		if id == "" || strings.TrimSpace(name) == "" || fieldKinds[id] != "" {
			continue
		}
		settings := map[string]any{}
		switch kind {
		case "number", "checkbox", "date", "text":
		case "list":
			kind = "dropdown"
			options := []any{}
			for _, option := range cdList(plain(field["options"])) {
				if id, ok := cdGet(option, "id").(string); ok && id != "" {
					options = append(options, map[string]any{"_id": id, "name": cdOr(cdGet(cdGet(option, "value"), "text"))})
				}
			}
			settings["dropdownItems"] = options
		default:
			continue
		}
		fieldKinds[id] = kind
		front := asM(field["display"])["cardFront"] == true
		add("customFields", map[string]any{"_id": id, "name": name, "type": kind, "settings": settings,
			"showOnCard": front, "showLabelOnMiniCard": front, "automaticallyOnCard": true, "sort": field["pos"]})
	}
	for _, value := range cards {
		card, ok := value.(map[string]any)
		id := str(card, "id")
		if !ok || id == "" || str(card, "idList") == "" {
			return nil, errTrelloDocument
		}
		row := map[string]any{"_id": id, "listId": card["idList"], "title": str(card, "name"),
			"description": str(card, "desc"), "sort": card["pos"], "archived": card["closed"] == true,
			"dueComplete": card["dueComplete"] == true, "labelIds": card["idLabels"]}
		for from, to := range map[string]string{"due": "dueAt", "start": "startAt", "dateLastActivity": "dateLastActivity"} {
			if at, valid := jsParseDate(str(card, from)); valid {
				row[to] = at.UTC().Format("2006-01-02T15:04:05.000Z")
			}
		}
		custom := []any{}
		used := map[string]bool{}
		for _, item := range docsOf(card, "customFieldItems") {
			fieldID := str(item, "idCustomField")
			if used[fieldID] {
				continue
			}
			if value, valid := trelloFieldImport(item, fieldKinds[fieldID]); valid {
				used[fieldID] = true
				custom = append(custom, map[string]any{"_id": fieldID, "value": value})
			}
		}
		row["customFields"] = custom
		add("cards", row)
		for _, attachment := range docsOf(card, "attachments") {
			name := str(attachment, "fileName")
			if name == "" {
				name = str(attachment, "name")
			}
			if id := str(attachment, "id"); id != "" && name != "" {
				add("attachments", map[string]any{"_id": id, "cardId": str(card, "id"), "name": name, "type": str(attachment, "mimeType")})
			}
		}
	}
	for _, rawCheck := range cdList(input["checklists"]) {
		check, ok := rawCheck.(map[string]any)
		if !ok {
			continue
		}
		id, card := str(check, "id"), str(check, "idCard")
		if id == "" || card == "" {
			continue
		}
		add("checklists", map[string]any{"_id": id, "cardId": card, "boardId": "", "title": str(check, "name"), "sort": check["pos"]})
		for _, item := range docsOf(check, "checkItems") {
			add("checklistItems", map[string]any{"_id": str(item, "id"), "cardId": card, "checklistId": id, "boardId": "",
				"title": str(item, "name"), "sort": item["pos"], "isFinished": str(item, "state") == "complete"})
		}
	}
	for _, rawAction := range cdList(input["actions"]) {
		action, _ := rawAction.(map[string]any)
		if str(action, "type") != "commentCard" {
			continue
		}
		data, _ := action["data"].(map[string]any)
		card, _ := data["card"].(map[string]any)
		row := map[string]any{"cardId": str(card, "id"), "text": str(data, "text")}
		if at, valid := jsParseDate(str(action, "date")); valid {
			row["createdAt"] = at.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		add("comments", row)
	}
	return out, nil
}

type trelloArchiveBoard struct {
	doc   map[string]any
	files func(bson.M) io.ReadCloser
}

func trelloImportArchive(raw []byte) ([]trelloArchiveBoard, error) {
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || len(archive.File) > 10000 {
		return nil, errTrelloDocument
	}
	entries := map[string]*zip.File{}
	var names []string
	for _, file := range archive.File {
		name := strings.TrimSuffix(file.Name, "/")
		if name == "." || name == ".." || strings.HasPrefix(name, "../") || name == "" ||
			path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:\x00") {
			return nil, errTrelloDocument
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if !file.Mode().IsRegular() || entries[name] != nil {
			return nil, errTrelloDocument
		}
		entries[name] = file
		if strings.HasSuffix(strings.ToLower(name), ".json") && !strings.Contains("/"+path.Dir(name)+"/", "/attachments/") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type candidate struct {
		name string
		doc  map[string]any
	}
	var candidates []candidate
	perDirectory := map[string]int{}
	var usedBytes int64
	for _, name := range names {
		remaining := int64(maxImportForm) - usedBytes
		if remaining <= 0 || entries[name].UncompressedSize64 > uint64(remaining) {
			return nil, errTooLarge
		}
		jsonBytes, err := readBounded(entries[name], remaining)
		if err != nil {
			return nil, err
		}
		usedBytes += int64(len(jsonBytes))
		doc, err := trelloImport(jsonBytes)
		if err != nil {
			if path.Base(name) == "board.json" {
				return nil, err
			}
			continue
		}
		candidates = append(candidates, candidate{name, doc})
		perDirectory[path.Dir(name)]++
	}
	if len(candidates) == 0 {
		return nil, errTrelloDocument
	}

	scopesOf := func(board candidate) []string {
		dir := path.Dir(board.name)
		stem := strings.TrimSuffix(board.name, path.Ext(board.name))
		var scopes []string
		if path.Base(board.name) != "board.json" {
			scopes = append(scopes, path.Join(stem, "attachments"), stem)
		}
		if perDirectory[dir] == 1 && (dir != "." || len(candidates) == 1) {
			scopes = append(scopes, path.Join(dir, "attachments"))
			if dir != "." {
				scopes = append(scopes, dir)
			}
		}
		return scopes
	}
	scopeOwners := map[string]int{}
	for _, board := range candidates {
		for _, scope := range scopesOf(board) {
			scopeOwners[scope]++
		}
	}
	boards := make([]trelloArchiveBoard, 0, len(candidates))
	for _, board := range candidates {
		scopes := scopesOf(board)
		boards = append(boards, trelloArchiveBoard{board.doc, func(row bson.M) io.ReadCloser {
			var file *zip.File
			var member string
			for _, leaf := range []string{str(row, "_id") + "_" + str(row, "name"), str(row, "name")} {
				if leaf == "" || leaf == "." || leaf == ".." || strings.ContainsAny(leaf, "/\\:\x00") {
					continue
				}
				for _, scope := range scopes {
					if scopeOwners[scope] != 1 {
						continue
					}
					member = path.Join(scope, leaf)
					file = entries[member]
					if file != nil {
						break
					}
				}
				if file != nil {
					break
				}
			}
			if file == nil {
				return nil
			}
			delete(entries, member)
			limit := min(int64(MaxAttachmentBytes), int64(maxImportForm)-usedBytes)
			if limit <= 0 || file.UncompressedSize64 > uint64(limit) {
				return nil
			}
			content, err := readBounded(file, limit)
			if err != nil {
				usedBytes += limit
				return nil
			}
			usedBytes += int64(len(content))
			return io.NopCloser(bytes.NewReader(content))
		}})
	}
	return boards, nil
}

func (s *Server) importTrelloPage(w http.ResponseWriter, r *http.Request, u *User) {
	data := s.importData(r, u, "trello")
	fail := func(status int) {
		data["Error"] = errTrelloDocument.Error()
		s.render(w, r, "import", status, data)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportForm+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		fail(http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	var doc map[string]any
	var boards []trelloArchiveBoard
	var err error
	if file, header, openErr := r.FormFile("file"); openErr == nil {
		defer file.Close()
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(file, maxImportForm+1))
		if err != nil || len(raw) > maxImportForm {
			fail(http.StatusRequestEntityTooLarge)
			return
		}
		if strings.HasSuffix(strings.ToLower(header.Filename), ".zip") || bytes.HasPrefix(raw, []byte("PK\x03\x04")) {
			boards, err = trelloImportArchive(raw)
		} else {
			doc, err = trelloImport(raw)
		}
	} else {
		doc, err = trelloImport([]byte(r.FormValue("text")))
	}
	if err != nil {
		fail(http.StatusBadRequest)
		return
	}
	if len(boards) == 0 {
		boards = []trelloArchiveBoard{{doc: doc}}
	}
	created := []string{}
	for _, board := range boards {
		id, err := s.importBoardWith(r, u, board.doc, nil, importOptions{Files: board.files})
		if err != nil {
			for _, previous := range created {
				s.deleteBoardData(context.WithoutCancel(r.Context()), previous)
			}
			if errors.Is(err, errBoardCreation) {
				fail(http.StatusForbidden)
			} else {
				fail(http.StatusInternalServerError)
			}
			return
		}
		created = append(created, id)
	}
	http.Redirect(w, r, "/b/"+created[0]+"/"+slugify(str(boards[0].doc, "title")), http.StatusSeeOther)
}
