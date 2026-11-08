package web

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Admin Panel -> Problems over REST, as Node.js WeKan (server/models/eventLog.js):
// for a site admin only, read-only apart from acknowledging a stream.
func (s *Server) apiAdminRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/admin/problems", s.api(s.apiProblems))
	m.HandleFunc("GET /api/admin/problems/{stream}", s.api(s.apiProblemStream))
	m.HandleFunc("POST /api/admin/problems/{stream}/acknowledge", s.api(s.apiAcknowledge))
}

// eventStreams is WeKan's EVENT_STREAMS.
var eventStreams = []string{"security", "speed", "tests", "cpu", "ram", "disk", "database", "integrity", "mail"}

func clampInt(v string, fallback, lo, hi int) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return max(lo, min(hi, n))
}

// newProblems is WeKan's newProblemsSelector: a stream's rows that are not
// mere information, since it was last acknowledged.
func (s *Server) newProblems(r *http.Request, stream string) int64 {
	filter := bson.M{"stream": stream, "severity": bson.M{"$ne": "info"}}
	var ack struct {
		At time.Time `bson:"at"`
	}
	if s.db.Collection("eventlogAcks").FindOne(r.Context(), bson.M{"stream": stream}).Decode(&ack) == nil {
		filter["at"] = bson.M{"$gt": ack.At}
	}
	n, _ := s.db.Collection("eventlog").CountDocuments(r.Context(), filter)
	return n
}

// apiProblems is WeKan's overview - the problems Weddy can see (cards with no
// board, list or swimlane) - with each stream's new problems.
func (s *Server) apiProblems(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	broken, _ := s.db.Collection("cards").CountDocuments(r.Context(), bson.M{"$or": bson.A{
		bson.M{"boardId": bson.M{"$in": bson.A{nil, ""}}}, bson.M{"swimlaneId": bson.M{"$in": bson.A{nil, ""}}},
		bson.M{"listId": bson.M{"$in": bson.A{nil, ""}}}}})
	problems := []map[string]any{}
	if broken > 0 {
		problems = append(problems, map[string]any{"id": "broken-cards", "severity": "warning", "count": broken, "title": "Broken cards",
			"detail": strconv.FormatInt(broken, 10) + " card(s) with a missing board/list/swimlane or an invalid type. Open the board to auto-repair, or run the repair migration."})
	}
	areas := []map[string]any{}
	for _, st := range eventStreams {
		if n := s.newProblems(r, st); n > 0 {
			areas = append(areas, map[string]any{"stream": st, "count": n})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"anyInProgress": false, "inProgress": []any{}, "anyProblems": len(problems) > 0,
		"problems": problems, "newProblems": areas, "streams": eventStreams})
}

// apiProblemStream is one stream's rows, newest first (the api stream busiest
// first), a page at a time, searchable as WeKan's streamSelector.
func (s *Server) apiProblemStream(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	stream := r.PathValue("stream")
	if !slices.Contains(eventStreams, stream) && stream != "api" {
		apiError(w, http.StatusNotFound, "Unknown event stream")
		return
	}
	q := r.URL.Query()
	limit, skip := clampInt(q.Get("limit"), 50, 1, 200), clampInt(q.Get("skip"), 0, 0, 1<<53)
	filter := bson.M{"stream": stream}
	if search := q.Get("search"); search != "" && len(search) <= 200 {
		rx := bson.M{"$regex": regexp.QuoteMeta(search), "$options": "i"}
		or := bson.A{}
		for _, f := range []string{"category", "bleed", "source", "detail", "db", "kind", "type", "message", "username", "ip", "api"} {
			or = append(or, bson.M{f: rx})
		}
		filter["$or"] = or
	}
	sort := bson.D{{Key: "at", Value: -1}}
	if stream == "api" {
		sort = bson.D{{Key: "count", Value: -1}, {Key: "at", Value: -1}}
	}
	total, _ := s.db.Collection("eventlog").CountDocuments(r.Context(), filter)
	cur, err := s.db.Collection("eventlog").Find(r.Context(), filter, options.Find().SetSort(sort).SetLimit(int64(limit)).SetSkip(int64(skip)))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows := []bson.M{}
	_ = cur.All(r.Context(), &rows)
	writeJSON(w, http.StatusOK, map[string]any{"stream": stream, "total": total, "limit": limit, "skip": skip, "rows": rows})
}

func (s *Server) apiAcknowledge(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	stream := r.PathValue("stream")
	if !slices.Contains(eventStreams, stream) {
		apiError(w, http.StatusNotFound, "Unknown event stream")
		return
	}
	if _, err := s.db.Collection("eventlogAcks").UpdateOne(r.Context(), bson.M{"stream": stream},
		bson.M{"$set": bson.M{"stream": stream, "at": time.Now().UTC()}, "$setOnInsert": bson.M{"_id": randomID()}},
		options.UpdateOne().SetUpsert(true)); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"acknowledged": stream})
}
