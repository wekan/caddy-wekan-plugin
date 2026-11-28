package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
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
	knownPeople := map[string]bool{}
	addPerson := func(id, username, name string) {
		if id == "" || knownPeople[id] {
			return
		}
		knownPeople[id] = true
		add("members", map[string]any{"userId": id, "username": username, "fullName": name})
	}
	for _, person := range docsOf(input, "members") {
		addPerson(str(person, "id"), str(person, "username"), str(person, "fullName"))
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
		assignees := []any{}
		for _, member := range cdList(card["idMembers"]) {
			if id, ok := member.(string); ok && id != "" {
				assignees = append(assignees, id)
				addPerson(id, "", id)
			}
		}
		row["assignees"] = assignees
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
		person := asM(action["memberCreator"])
		author := str(action, "idMemberCreator")
		if author == "" {
			author = str(person, "username")
		}
		if author == "" {
			author = str(person, "fullName")
		}
		addPerson(author, str(person, "username"), str(person, "fullName"))
		row := map[string]any{"cardId": str(card, "id"), "text": str(data, "text"), "userId": author}
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

func (s *Server) trelloPeople(ctx context.Context, doc map[string]any, user *User, mode string, selected map[string]string) (map[string]string, []string) {
	mapping := map[string]string{}
	var missing []importPerson
	for _, person := range docsOf(doc, "members") {
		id, username, name := str(person, "userId"), str(person, "username"), str(person, "fullName")
		if mode == "me" {
			mapping[id] = user.ID
			continue
		}
		targetName := username
		if name, ok := selected[id]; ok {
			targetName = name
		}
		if mode == "map" && targetName != "" {
			var target User
			if s.db.Collection("users").FindOne(ctx, bson.M{"username": eq(targetName), "loginDisabled": bson.M{"$ne": true}}).Decode(&target) == nil {
				mapping[id] = target.ID
				continue
			}
		}
		if name == "" {
			name = username
		}
		missing = append(missing, importPerson{key: id, name: name})
	}
	return mapping, s.importPlaceholders(ctx, missing, mapping, "trello")
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
	if r.URL.Query().Get("preview") == "1" {
		people := []any{}
		seen := map[string]bool{}
		for _, board := range boards {
			for _, person := range docsOf(board.doc, "members") {
				key := str(person, "userId")
				if !seen[key] {
					seen[key] = true
					people = append(people, map[string]string{"id": key, "username": str(person, "username"), "name": str(person, "fullName")})
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"people": people})
		return
	}
	selected := map[string]string{}
	if raw := r.FormValue("membersMapping"); raw != "" {
		if len(raw) > maxImportForm || json.Unmarshal([]byte(raw), &selected) != nil || selected == nil {
			fail(http.StatusBadRequest)
			return
		}
	}
	mode := importMembersMode(r.FormValue("membersMode"))
	if r.FormValue("membersMode") == "" {
		mode = "map"
	}
	created, status := s.saveTrelloBoards(r, u, boards, mode, selected)
	if status != http.StatusOK {
		fail(status)
		return
	}
	http.Redirect(w, r, "/b/"+created[0]+"/"+slugify(str(boards[0].doc, "title")), http.StatusSeeOther)
}

func (s *Server) saveTrelloBoards(r *http.Request, u *User, boards []trelloArchiveBoard, mode string, selected map[string]string) ([]string, int) {
	validated := map[string]bool{}
	for _, username := range selected {
		if validated[username] {
			continue
		}
		if username == "" || s.db.Collection("users").FindOne(r.Context(),
			bson.M{"username": eq(username), "loginDisabled": bson.M{"$ne": true}}).Err() != nil {
			return nil, http.StatusBadRequest
		}
		validated[username] = true
	}
	if s.mayCreateBoard(r.Context(), u, "board", "importBoard") != nil {
		return nil, http.StatusForbidden
	}
	created := []string{}
	var placeholders bson.A
	for _, board := range boards {
		mapping, made := s.trelloPeople(r.Context(), board.doc, u, mode, selected)
		for _, id := range made {
			placeholders = append(placeholders, id)
		}
		id, err := s.importBoardWith(r, u, board.doc, mapping, importOptions{Files: board.files, InactiveMembers: true})
		if err != nil {
			for _, previous := range created {
				s.deleteBoardData(context.WithoutCancel(r.Context()), previous)
			}
			if len(placeholders) > 0 {
				_, _ = s.db.Collection("users").DeleteMany(context.WithoutCancel(r.Context()), bson.M{"_id": dbIn(placeholders)})
			}
			if errors.Is(err, errBoardCreation) {
				return nil, http.StatusForbidden
			}
			return nil, http.StatusInternalServerError
		}
		created = append(created, id)
	}
	return created, http.StatusOK
}

func (s *Server) importTrelloHTTP(w http.ResponseWriter, r *http.Request, u *User) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportForm)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		apiError(w, http.StatusBadRequest, errTrelloDocument.Error())
		return
	}
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var boards []trelloArchiveBoard
	var payload struct {
		Board          json.RawMessage
		MembersMapping map[string]string
		MembersMode    string
	}
	switch contentType {
	case "application/json":
		if err = json.Unmarshal(raw, &payload); err == nil {
			var doc map[string]any
			if doc, err = trelloImport(payload.Board); err == nil {
				boards = []trelloArchiveBoard{{doc: doc}}
			}
		}
	case "application/zip", "application/x-zip-compressed":
		boards, err = trelloImportArchive(raw)
	default:
		err = errTrelloDocument
	}
	if err != nil || len(boards) == 0 {
		apiError(w, http.StatusBadRequest, errTrelloDocument.Error())
		return
	}
	mode := importMembersMode(payload.MembersMode)
	if payload.MembersMode == "" {
		mode = "map"
	}
	ids, status := s.saveTrelloBoards(r, u, boards, mode, payload.MembersMapping)
	if status != http.StatusOK {
		apiError(w, status, errTrelloDocument.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"boardIds": ids})
}
