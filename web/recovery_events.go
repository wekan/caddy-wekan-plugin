package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Admin Panel / Problems / Recovery's events: the recoveryEvents rows -
// what imports left out, among the data-safety actions Node.js WeKan
// records - as its report shows them (server/publications/recoveryReport.js,
// models/lib/recoveryReportQuery.js, client/components/settings/
// adminProblems.js report-recovery): newest first, ten a page, searched by
// event, detail and database, shown by status.

const recoveryEventsPerPage = 10

var _ = []string{msg("recovery-report-desc"), msg("recovery-no-events"), msg("recovery-event"), msg("recovery-detail"),
	msg("done"), msg("date"), msg("username"), msg("event-ipv4"), msg("event-ipv6"), msg("location")}

// recoveryReportQuery is WeKan's.
func recoveryReportQuery(search, status string) bson.M {
	var clauses []bson.M
	if search != "" {
		re := bson.Regex{Pattern: jsEscapeRegExp(search), Options: "i"}
		clauses = append(clauses, bson.M{"$or": bson.A{bson.M{"type": re}, bson.M{"detail": re}, bson.M{"db": re}}})
	}
	switch status {
	case "done":
		clauses = append(clauses, bson.M{"$or": bson.A{bson.M{"done": true}, bson.M{"done": bson.M{"$exists": false}}}})
	case "failed":
		clauses = append(clauses, bson.M{"done": false})
	case "deleted":
		clauses = append(clauses, bson.M{"deletedData": true})
	}
	switch len(clauses) {
	case 0:
		return bson.M{}
	case 1:
		return clauses[0]
	}
	all := bson.A{}
	for _, c := range clauses {
		all = append(all, c)
	}
	return bson.M{"$and": all}
}

// recoveryLocation is WeKan's locationLabel: "City, Country" and the like.
func recoveryLocation(v any) string {
	loc, _ := plain(v).(map[string]any)
	city, region, country := strOf(loc["city"]), strOf(loc["region"]), strOf(loc["country"])
	switch {
	case city != "" && country != "":
		return city + ", " + country
	case city != "":
		return city
	case region != "" && country != "":
		return region + ", " + country
	}
	return jsOr(region, country, "").(string)
}

type recoveryEventRow struct {
	Done, Deleted                bool
	Date, Type, UserID, Username string
	IPv4, IPv4Location           string
	IPv6, IPv6Location           string
	Detail, Severity             string
}

func (s *Server) recoveryEventsPage(r *http.Request) map[string]any {
	q := r.URL.Query()
	search := q.Get("q")
	if utf8.RuneCountInString(search) > 200 {
		search = string([]rune(search)[:200])
	}
	status := q.Get("status")
	if status != "done" && status != "failed" && status != "deleted" {
		status = "all"
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	ctx := r.Context()
	filter := recoveryReportQuery(search, status)
	total, _ := s.db.Collection("recoveryEvents").CountDocuments(ctx, filter)
	pages := int((total + recoveryEventsPerPage - 1) / recoveryEventsPerPage)
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	var docs []bson.M
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetSkip(int64((page - 1) * recoveryEventsPerPage)).SetLimit(recoveryEventsPerPage)
	if cur, err := s.db.Collection("recoveryEvents").Find(context.WithoutCancel(ctx), filter, opts); err == nil {
		_ = cur.All(ctx, &docs)
	}
	var rows []recoveryEventRow
	for _, d := range docs {
		row := recoveryEventRow{Done: d["done"] != false, Deleted: d["deletedData"] == true, Type: strOf(d["type"]),
			UserID: strOf(d["userId"]), Username: strOf(d["username"]), Detail: strOf(d["detail"]),
			Severity: jsOr(strOf(d["severity"]), "info").(string)}
		if t, ok := d["createdAt"].(bson.DateTime); ok {
			row.Date = t.Time().UTC().Format(time.DateTime)
		}
		// WeKan's addressValue: the row's own family field, else its ip's.
		v4, v6 := strOf(d["ipv4"]), strOf(d["ipv6"])
		if v4 == "" && v6 == "" {
			v4, v6 = classifyAddress(strOf(d["ip"]))
		}
		row.IPv4, row.IPv6 = v4, v6
		if v4 != "" {
			row.IPv4Location = recoveryLocation(jsOr(d["ipv4Location"], d["location"]))
		}
		if v6 != "" {
			row.IPv6Location = recoveryLocation(jsOr(d["ipv6Location"], d["location"]))
		}
		rows = append(rows, row)
	}
	link := func(p int) string {
		v := url.Values{}
		if search != "" {
			v.Set("q", search)
		}
		if status != "all" {
			v.Set("status", status)
		}
		v.Set("page", strconv.Itoa(p))
		return "/admin/problems/recovery?" + v.Encode()
	}
	out := map[string]any{"RecoveryEvents": rows, "RecoverySearch": search, "RecoveryStatus": status,
		"RecoveryTotal": total, "RecoveryPage": page, "RecoveryPages": pages,
		"RecoveryStatuses": []struct{ Value, Label string }{{"all", "All"}, {"done", "Done"}, {"failed", "Failed"}, {"deleted", "Deleted"}}}
	if page > 1 {
		out["RecoveryPrev"] = link(page - 1)
	}
	if page < pages {
		out["RecoveryNext"] = link(page + 1)
	}
	return out
}
