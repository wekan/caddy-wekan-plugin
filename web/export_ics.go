package web

import (
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The board's dated cards as an iCalendar feed, as Node.js WeKan
// (models/export.js calendar.ics, models/lib/icalExport.js).
func (s *Server) exportICSRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/calendar.ics", s.exportable(s.apiExportICS))
}

// icsEscape is RFC 5545 TEXT: backslash, semicolon, comma and line breaks escaped.
func icsEscape(v string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\r", `\n`, "\n", `\n`)
	return r.Replace(v)
}

func icsDate(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// icsFold folds a line at 75 characters, each continuation starting with a space.
func icsFold(line string) string {
	r := []rune(line)
	if len(r) <= 75 {
		return line
	}
	var b strings.Builder
	b.WriteString(string(r[:75]))
	for rest := r[75:]; len(rest) > 0; {
		n := min(74, len(rest))
		b.WriteString("\r\n " + string(rest[:n]))
		rest = rest[n:]
	}
	return b.String()
}

func timeOf(v any) (time.Time, bool) {
	switch x := v.(type) {
	case bson.DateTime:
		return x.Time(), true
	case time.Time:
		return x, true
	}
	return time.Time{}, false
}

// cardsToICS is WeKan's cardsToIcs: a card's start..end span, its due date and
// its received date are an event each.
func cardsToICS(cards []bson.M, calendarName string, cardURL func(bson.M) string, now time.Time) string {
	stamp := icsDate(now)
	lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", icsFold("PRODID:-//WeKan//iCal Export//EN"), "CALSCALE:GREGORIAN",
		"METHOD:PUBLISH", icsFold("X-WR-CALNAME:" + icsEscape(calendarName))}
	event := func(uid string, start, end time.Time, summary, description, url string) {
		lines = append(lines, "BEGIN:VEVENT", icsFold("UID:"+uid), icsFold("DTSTAMP:"+stamp), icsFold("DTSTART:"+icsDate(start)),
			icsFold("DTEND:"+icsDate(end)), icsFold("SUMMARY:"+icsEscape(summary)))
		if description != "" {
			lines = append(lines, icsFold("DESCRIPTION:"+icsEscape(description)))
		}
		if url != "" {
			lines = append(lines, icsFold("URL:"+url))
		}
		lines = append(lines, "END:VEVENT")
	}
	for _, c := range cards {
		id, _ := c["_id"].(string)
		title, _ := c["title"].(string)
		if title == "" {
			title = "(untitled card)"
		}
		desc, _ := c["description"].(string)
		url := cardURL(c)
		start, hasStart := timeOf(c["startAt"])
		end, hasEnd := timeOf(c["endAt"])
		if hasStart || hasEnd {
			if !hasStart {
				start = end
			}
			if !hasEnd {
				end = start
			}
			event(id+"-span@wekan", start, end, title, desc, url)
		}
		if due, ok := timeOf(c["dueAt"]); ok {
			event(id+"-due@wekan", due, due, title+" (due)", desc, url)
		}
		if got, ok := timeOf(c["receivedAt"]); ok {
			event(id+"-received@wekan", got, got, title+" (received)", desc, url)
		}
	}
	lines = append(lines, "END:VCALENDAR")
	return strings.Join(lines, "\r\n") + "\r\n"
}

func (s *Server) apiExportICS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("board")
	var b Board
	if !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&b) != nil {
		http.Error(w, "Board not found", http.StatusNotFound)
		return
	}
	if b.Permission != "public" {
		u, ok := s.exportUser(r)
		switch {
		case !ok:
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		case u == nil:
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if m := b.member(u.ID); m == nil || m.AssignedOnly() {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
	cur, err := s.db.Collection("cards").Find(r.Context(), bson.M{"boardId": b.ID, "archived": false,
		"linkedId": bson.M{"$in": bson.A{"", nil}}}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		http.Error(w, "Export failed", http.StatusInternalServerError)
		return
	}
	cards := []bson.M{}
	_ = cur.All(r.Context(), &cards)
	// Admin Panel -> Anonymize export users: the @mentions the feed carries.
	s.exportAnonymizer(r.Context(), &b, nil).cards(cards)
	base := siteURL(r)
	ics := cardsToICS(cards, "WeKan - "+b.Title, func(c bson.M) string {
		cid, _ := c["_id"].(string)
		return base + "/b/" + b.ID + "/" + b.Slug + "/" + cid
	}, time.Now())
	name := b.Slug
	if name == "" {
		name = b.ID
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="`+strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < ' ' {
			return '-'
		}
		return r
	}, name)+`.ics"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(ics))
}

// The import of an iCalendar file as cards, as Node.js WeKan
// (server/methods/icsImport.js, server/lib/icsImport.js).
func (s *Server) importICSRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/boards/{board}/swimlanes/{swimlane}/lists/{list}/ics", s.importable(s.api(s.apiImportICS)))
}

type icsEvent struct {
	Summary, Description, UID string
	Start, End                *time.Time
}

// icsUnfold joins RFC 5545's folded lines.
func icsUnfold(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(out) > 0 {
			out[len(out)-1] += line[1:]
		} else {
			out = append(out, line)
		}
	}
	return out
}

// icsContentLine is a line's name (upper case) and value; a colon inside a
// quoted parameter value is not the separator.
func icsContentLine(line string) (string, string, bool) {
	quoted := false
	for i, ch := range line {
		if ch == '"' {
			quoted = !quoted
		} else if ch == ':' && !quoted {
			name := strings.ToUpper(strings.SplitN(line[:i], ";", 2)[0])
			return name, line[i+1:], true
		}
	}
	return "", "", false
}

func icsUnescape(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			switch next := text[i+1]; next {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			case ',', ';', '\\':
				b.WriteByte(next)
				i++
				continue
			}
		}
		b.WriteByte(text[i])
	}
	return b.String()
}

// icsParseDate takes WeKan's two forms: a date-time (UTC, as WeKan reads it)
// and a date.
func icsParseDate(v string) *time.Time {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t
		}
	}
	return nil
}

func parseICS(text string) []icsEvent {
	var events []icsEvent
	var cur *icsEvent
	for _, line := range icsUnfold(text) {
		switch strings.ToUpper(line) {
		case "":
			continue
		case "BEGIN:VEVENT":
			cur = &icsEvent{}
			continue
		case "END:VEVENT":
			if cur != nil {
				events = append(events, *cur)
			}
			cur = nil
			continue
		}
		if cur == nil {
			continue
		}
		name, value, ok := icsContentLine(line)
		if !ok {
			continue
		}
		switch name {
		case "SUMMARY":
			cur.Summary = icsUnescape(value)
		case "DESCRIPTION":
			cur.Description = icsUnescape(value)
		case "DTSTART":
			cur.Start = icsParseDate(value)
		case "DTEND":
			cur.End = icsParseDate(value)
		case "UID":
			cur.UID = value
		}
	}
	return events
}

// MaxICSEvents is the most events one import makes into cards.
const MaxICSEvents = 5000

func (s *Server) apiImportICS(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !b.Writable(u, "write", nil) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not-authorized"})
		return
	}
	var req struct {
		ICS     string `json:"ics"`
		ICSText string `json:"icsText"`
	}
	_ = decodeJSON(r, &req)
	text := req.ICS
	if text == "" {
		text = req.ICSText
	}
	if strings.TrimSpace(text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing-ics", "message": "Provide the .ics file contents in the \"ics\" field."})
		return
	}
	ctx := r.Context()
	listID, swimlaneID := r.PathValue("list"), r.PathValue("swimlane")
	if !validID(listID) || s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID), "boardId": b.ID}).Err() != nil {
		apiError(w, http.StatusNotFound, "List not found on this board.")
		return
	}
	if !validID(swimlaneID) || s.db.Collection("swimlanes").FindOne(ctx, bson.M{"_id": eq(swimlaneID), "boardId": b.ID}).Err() != nil {
		apiError(w, http.StatusNotFound, "Swimlane not found on this board.")
		return
	}
	events := parseICS(text)
	if len(events) > MaxICSEvents {
		apiError(w, http.StatusBadRequest, "at most 5000 events in one import")
		return
	}
	ids := []string{}
	for _, e := range events {
		title := strings.TrimSpace(e.Summary)
		if title == "" {
			title = "(untitled event)" // a card always has a title here
		}
		if len(title) > 1000 {
			title = title[:1000]
		}
		c, err := s.AddCard(ctx, b, u, listID, swimlaneID, title)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		set := bson.M{"description": e.Description}
		if e.Start != nil {
			set["startAt"] = *e.Start
		}
		if due := e.End; due != nil {
			set["dueAt"] = *due
		} else if e.Start != nil {
			set["dueAt"] = *e.Start
		}
		_, _ = s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": set})
		ids = append(ids, c.ID)
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"created": len(ids), "cardIds": ids})
}
