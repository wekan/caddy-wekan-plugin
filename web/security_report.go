package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Admin Panel / Problems / Security Report (adminProblems.jade's
// eventStreamReport, stream "security"): the security stream's summary rows -
// one a problem, its count and who - newest first, by search, a page at a
// time. WeKan's EVENT_STREAM_COLUMNS: when, category, which Bleed, severity,
// action, source, who, from where, how many attempts and the detail. The row
// holds what the security log wrote and nothing a request carried, so no
// attempted value is shown.

const securityReportPerPage = 20

type securityReportRow struct {
	Date, Category, Bleed, Severity, Action, Source string
	Username, IP, Attempts, Detail                  string
}

func (s *Server) securityReportPage(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	if !u.IsAdmin {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "error-notAuthorized"), "User": u})
		return
	}
	q := r.URL.Query()
	search := q.Get("q")
	if utf8.RuneCountInString(search) > 200 {
		search = string([]rune(search)[:200])
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	filter := bson.M{"stream": "security"}
	if search != "" {
		rx := bson.M{"$regex": regexp.QuoteMeta(search), "$options": "i"}
		or := bson.A{}
		for _, f := range []string{"category", "bleed", "source", "detail", "username", "ip", "severity", "action"} {
			or = append(or, bson.M{f: rx})
		}
		filter["$or"] = or
	}
	ctx := r.Context()
	total, _ := s.db.Collection("eventlog").CountDocuments(ctx, filter)
	pages := int((total + securityReportPerPage - 1) / securityReportPerPage)
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	var docs []bson.M
	opts := options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetSkip(int64((page - 1) * securityReportPerPage)).SetLimit(securityReportPerPage)
	if cur, err := s.db.Collection("eventlog").Find(context.WithoutCancel(ctx), filter, opts); err == nil {
		_ = cur.All(ctx, &docs)
	}
	rows := []securityReportRow{}
	for _, d := range docs {
		row := securityReportRow{Category: strOf(d["category"]), Bleed: strOf(d["bleed"]), Severity: strOf(d["severity"]),
			Action: strOf(d["action"]), Source: strOf(d["source"]), Username: strOf(d["username"]), IP: strOf(d["ip"]),
			Detail: strOf(d["detail"])}
		if t, ok := d["at"].(bson.DateTime); ok {
			row.Date = t.Time().UTC().Format(time.DateTime)
		}
		switch n := d["count"].(type) {
		case int32:
			if n > 1 {
				row.Attempts = strconv.Itoa(int(n))
			}
		case int64:
			if n > 1 {
				row.Attempts = strconv.FormatInt(n, 10)
			}
		}
		rows = append(rows, row)
	}
	link := func(p int) string {
		v := url.Values{}
		if search != "" {
			v.Set("q", search)
		}
		v.Set("page", strconv.Itoa(p))
		return "/admin/problems/security-report?" + v.Encode()
	}
	data := map[string]any{"Title": tr(lang, "securityReportTitle"), "User": u, "SecurityRows": rows, "SecuritySearch": search,
		"SecurityTotal": total, "SecurityPage": page, "SecurityPages": pages}
	if page > 1 {
		data["SecurityPrev"] = link(page - 1)
	}
	if page < pages {
		data["SecurityNext"] = link(page + 1)
	}
	s.render(w, r, "securityreport", http.StatusOK, data)
}
