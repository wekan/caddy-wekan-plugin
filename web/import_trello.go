package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var errTrelloDocument = errors.New("error-json-schema")

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

func trelloImportArchive(raw []byte) (map[string]any, func(bson.M) io.ReadCloser, error) {
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || len(archive.File) > 10000 {
		return nil, nil, errTrelloDocument
	}
	entries := map[string]*zip.File{}
	for _, file := range archive.File {
		if file.Name != "board.json" && !strings.HasPrefix(file.Name, "attachments/") {
			continue
		}
		leaf := strings.TrimPrefix(file.Name, "attachments/")
		if leaf == "" && file.FileInfo().IsDir() {
			continue
		}
		if !file.Mode().IsRegular() || leaf == "." || leaf == ".." ||
			strings.ContainsAny(leaf, "/\\:\x00") || entries[file.Name] != nil {
			return nil, nil, errTrelloDocument
		}
		entries[file.Name] = file
	}
	entry := entries["board.json"]
	if entry == nil {
		return nil, nil, errTrelloDocument
	}
	jsonBytes, err := readBounded(entry, maxImportForm)
	if err != nil {
		return nil, nil, err
	}
	doc, err := trelloImport(jsonBytes)
	if err != nil {
		return nil, nil, err
	}
	var usedBytes int64
	readAttachment := func(row bson.M) io.ReadCloser {
		name := str(row, "name")
		path := "attachments/" + str(row, "_id") + "_" + name
		file := entries[path]
		if file == nil {
			path = "attachments/" + name
			file = entries[path]
		}
		if file == nil {
			return nil
		}
		delete(entries, path)
		limit := min(int64(MaxAttachmentBytes), int64(maxImportForm)-usedBytes)
		if limit <= 0 || file.UncompressedSize64 > uint64(limit) {
			return nil
		}
		content, err := readBounded(file, limit)
		if err != nil {
			// The bound is charged even when a stream disagrees with its header.
			usedBytes += limit
			return nil
		}
		usedBytes += int64(len(content))
		return io.NopCloser(bytes.NewReader(content))
	}
	return doc, readAttachment, nil
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
	var attachments func(bson.M) io.ReadCloser
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
			doc, attachments, err = trelloImportArchive(raw)
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
	id, err := s.importBoardWith(r, u, doc, nil, importOptions{Files: attachments})
	if errors.Is(err, errBoardCreation) {
		fail(http.StatusForbidden)
		return
	}
	if err != nil {
		fail(http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/b/"+id+"/"+slugify(str(doc, "title")), http.StatusSeeOther)
}
