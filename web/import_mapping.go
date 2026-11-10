package web

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's map-members step for another tool's file (client/components/
// import/import.js mapPeopleOrImport, importMapMembers): the file's people,
// read by the same parser that imports it, each to be mapped to an existing
// user; who is left unmapped becomes a placeholder user. The file itself
// travels with the step's form, so nothing is kept on the server between.

var _ = []string{msg("import-map-members"), msg("import-members-map"), msg("import-members-map-note"),
	msg("import-without-mapping-members"), msg("done")}

// importInputFromRaw is the page's file as its source's parser reads it:
// the text of a text source, the JSON of any other.
func importInputFromRaw(source string, raw []byte) (any, error) {
	if _, text := importTextParsers[source]; text {
		return string(raw), nil
	}
	doc, err := decodeJS(raw)
	if err != nil {
		return nil, errors.New("error-json-malformed")
	}
	return doc, nil
}

type importMappingPerson struct {
	Index     int
	Key, Name string
}

func (s *Server) importMappingStep(w http.ResponseWriter, r *http.Request, u *User, source string, raw []byte, people []importPerson, split bool) {
	data := s.importData(r, u, source)
	var list []importMappingPerson
	for i, p := range people {
		list = append(list, importMappingPerson{Index: i, Key: p.key, Name: p.name})
	}
	data["MapMembers"] = map[string]any{"People": list, "Payload": base64.StdEncoding.EncodeToString(raw),
		"Action": "/import/" + source + "/members", "Split": split}
	s.render(w, r, "import", http.StatusOK, data)
}

// validImportMapping keeps the mapping's existing users only.
func (s *Server) validImportMapping(ctx context.Context, mapping map[string]string) map[string]string {
	out := map[string]string{}
	for key, id := range mapping {
		if key == "" || !validID(id) || key == "__proto__" || key == "constructor" || key == "prototype" {
			continue
		}
		if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Err() == nil {
			out[key] = id
		}
	}
	return out
}

// importMembersPage is the step's answer: the file again, each person's
// chosen username, and the import with them.
func (s *Server) importMembersPage(w http.ResponseWriter, r *http.Request, u *User) {
	source := r.PathValue("source")
	if _, general := importParsers[source]; !general {
		if _, text := importTextParsers[source]; !text {
			http.NotFound(w, r)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportForm*2+1<<20)
	data := s.importData(r, u, source)
	fail := func(code string) {
		data["Error"] = code
		s.render(w, r, "import", http.StatusBadRequest, data)
	}
	raw, err := base64.StdEncoding.DecodeString(r.FormValue("payload"))
	if err != nil || len(raw) > maxImportForm {
		fail("error-json-malformed")
		return
	}
	input, err := importInputFromRaw(source, raw)
	if err != nil {
		fail(err.Error())
		return
	}
	people := importPeopleOptions{Mode: "map", Mapping: map[string]string{}, Split: r.FormValue("splitBy") == "swimlane"}
	if r.FormValue("skipMapping") != "" {
		people.Mode = "placeholder"
	} else {
		for i := 0; i < 10000; i++ {
			key, ok := r.Form["key-"+strconv.Itoa(i)]
			if !ok {
				break
			}
			name := strings.TrimSpace(r.FormValue("map-" + strconv.Itoa(i)))
			if name == "" || len(key) == 0 {
				continue
			}
			var target User
			if s.db.Collection("users").FindOne(r.Context(), bson.M{"username": eq(name)}).Decode(&target) == nil && !target.LoginDisabled {
				people.Mapping[key[0]] = target.ID
			}
		}
	}
	s.importFromPage(w, r, u, source, input, people, fail)
}
