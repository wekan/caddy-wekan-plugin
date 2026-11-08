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
	m.HandleFunc("GET /api/boards/{board}/calendar.ics", s.apiExportICS)
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
