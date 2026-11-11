package web

// The rest of WeKan's password policy (server/passwordChangeRequired.js,
// server/lib/shortPasswordLogins.js, models/lib/shortPasswordLogins.js):
//
//   - A sign-in with a password shorter than PASSWORD_MIN_LENGTH still signs
//     in, and is listed in Admin Panel / Problems / Password length - ONE ROW
//     PER ACCOUNT in shortPasswordLogins, its count growing, with the first and
//     the latest sign-in and the latest IPv4 and IPv6 address. A fixed row seen
//     short again is open again. WeKan's browser has to tell the server the
//     password was short, as Meteor sends it hashed; Weddy's sign-in form and
//     REST login receive the password itself, so the server measures it.
//   - With PASSWORD_MIN_LENGTH_ENFORCE, the account must change its password
//     before anything else: every page leads to the profile's change form.
//   - Changing one's own password - the profile's change, a reset link -
//     ends the requirement, marks the row fixed, and signs EVERY session of
//     the account out, this one too, so the next sign-in is with the new
//     password everywhere.
//
// Only accounts that sign in with a WeKan password: LDAP, OAuth2, OIDC, SAML,
// CAS and Sandstorm passwords are their provider's.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// signsInWithPassword is twoFactorRequirement.js's: a WeKan password account.
func signsInWithPassword(method string, hasPassword bool) bool {
	return hasPassword && (method == "" || method == "password")
}

func (s *Server) passwordAccount(ctx context.Context, userID string) (required, password bool) {
	var d struct {
		Method   string `bson:"authenticationMethod"`
		Required bool   `bson:"passwordChangeRequired"`
		Services struct {
			Password bson.M `bson:"password"`
		} `bson:"services"`
	}
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&d) != nil {
		return false, false
	}
	pw := signsInWithPassword(d.Method, d.Services.Password["bcrypt"] != nil || d.Services.Password["argon2"] != nil)
	return d.Required && pw, pw
}

// noteSignInPassword is called after a password sign-in that worked: a short
// password is reported, and while enforced the change becomes required.
func (s *Server) noteSignInPassword(r *http.Request, u *User, password string) {
	defer func() { _ = recover() }() // a report must never break the sign-in
	ctx := r.Context()
	policy := s.passwordPolicy(ctx)
	if !policy.passwordTooShort(password) {
		return
	}
	if _, pw := s.passwordAccount(ctx, u.ID); !pw {
		return
	}
	now := time.Now().UTC()
	set := bson.M{"at": now}
	if v4, v6 := classifyAddress(clientKey(r)); v4 != "" {
		set["ipv4"] = v4
	} else if v6 != "" {
		set["ipv6"] = v6
	}
	set["username"] = u.Username // $set, written below
	_, _ = s.db.Collection("shortPasswordLogins").UpdateOne(ctx, bson.M{"userId": eq(u.ID)}, bson.M{
		"$set": set, "$inc": bson.M{"count": 1}, "$setOnInsert": bson.M{"firstAt": now, "_id": randomID()},
		// Seen short again: not fixed any more.
		"$unset": bson.M{"fixedAt": ""}}, options.UpdateOne().SetUpsert(true))
	if policy.Enforce {
		_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{"passwordChangeRequired": true}})
	}
}

// endPasswordChangeRequirement: one's own password changed. Every session of
// the account ends, the requirement too, and an open report is fixed - a row
// already fixed keeps the date it was first fixed.
func (s *Server) endPasswordChangeRequirement(ctx context.Context, userID string) {
	_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(userID)}, bson.M{
		"$unset": bson.M{"passwordChangeRequired": ""}, "$set": bson.M{"services.resume.loginTokens": bson.A{}}})
	_, _ = s.db.Collection("shortPasswordLogins").UpdateOne(ctx, bson.M{"userId": eq(userID), "fixedAt": nil},
		bson.M{"$set": bson.M{"fixedAt": time.Now().UTC()}})
}

// passwordChangeGate: while the minimum is enforced and the account must
// change its password, a page other than the change itself (and signing out
// and the files it needs) leads there. True when the request was answered.
func (s *Server) passwordChangeGate(w http.ResponseWriter, r *http.Request, u *User) bool {
	p := r.URL.Path
	if (p == "/profile" && r.Method == http.MethodGet) || p == "/profile/password" || p == "/logout" || strings.HasPrefix(p, "/static/") {
		return false
	}
	ctx := r.Context()
	if !s.passwordPolicy(ctx).Enforce {
		return false
	}
	if required, _ := s.passwordAccount(ctx, u.ID); !required {
		return false
	}
	if r.Method == http.MethodGet {
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
	} else {
		http.Error(w, tr(languageOf(r, u).Tag, "password-change-required-title"), http.StatusForbidden)
	}
	return true
}

// shortPasswordLoginsPage is WeKan's method of the table: newest sign-in
// first, by filter short / fixed / all, searched by name or address. Site
// administrators only.
func (s *Server) shortPasswordLoginsPage(ctx context.Context, u *User, filter, search string, limit, skip int64) (map[string]any, error) {
	var d struct {
		IsAdmin  bool `bson:"isAdmin"`
		Disabled bool `bson:"loginDisabled"`
	}
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(u.ID)}).Decode(&d) != nil || !d.IsAdmin || d.Disabled {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	sel := bson.M{}
	switch filter {
	case "fixed":
		sel["fixedAt"] = bson.M{"$type": "date"}
	case "all":
	default:
		sel["fixedAt"] = nil
	}
	if search = strings.TrimSpace(search); search != "" {
		if len(search) > 100 {
			search = search[:100]
		}
		sel["$or"] = bson.A{bson.M{"username": containsFold(search)}, bson.M{"ipv4": containsFold(search)}, bson.M{"ipv6": containsFold(search)}}
	}
	if limit < 1 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}
	if skip < 0 {
		skip = 0
	}
	coll := s.db.Collection("shortPasswordLogins")
	total, _ := coll.CountDocuments(ctx, sel)
	cur, err := coll.Find(ctx, sel, options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetSkip(skip).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	rows := []bson.M{}
	_ = cur.All(ctx, &rows)
	return map[string]any{"rows": rows, "total": total}, nil
}

// methodShortPasswordLoginsPage is WeKan's shortPasswordLoginsPage(options).
func (s *Server) methodShortPasswordLoginsPage(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var o struct {
		Filter string `json:"filter"`
		Search string `json:"search"`
		Limit  int64  `json:"limit"`
		Skip   int64  `json:"skip"`
	}
	if len(args) > 0 && json.Unmarshal(args[0], &o) != nil {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	return s.shortPasswordLoginsPage(r.Context(), u, o.Filter, o.Search, o.Limit, o.Skip)
}

type passwordLengthRow struct {
	Username, First, Last, Fixed, IPv4, IPv6 string
	Count                                    int64
}

// passwordLengthPage is Admin Panel / Problems / Password length.
func (s *Server) passwordLengthPage(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	if !u.IsAdmin {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "error-notAuthorized"), "User": u})
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	filter := q.Get("filter")
	if filter != "fixed" && filter != "all" {
		filter = "short"
	}
	page, _ := strconv.ParseInt(q.Get("page"), 10, 64)
	if page < 1 {
		page = 1
	}
	const per = 10
	res, err := s.shortPasswordLoginsPage(ctx, u, filter, q.Get("q"), per, (page-1)*per)
	if err != nil {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "error-notAuthorized"), "User": u})
		return
	}
	date := func(v any) string {
		if t, ok := v.(bson.DateTime); ok {
			return t.Time().UTC().Format("2006-01-02 15:04")
		}
		return ""
	}
	var rows []passwordLengthRow
	for _, d := range res["rows"].([]bson.M) {
		n, _ := safeInt(d["count"])
		rows = append(rows, passwordLengthRow{Username: orDefault(docString(d, "username"), docString(d, "userId")), First: date(d["firstAt"]),
			Last: date(d["at"]), Fixed: date(d["fixedAt"]), Count: n, IPv4: docString(d, "ipv4"), IPv6: docString(d, "ipv6")})
	}
	total := res["total"].(int64)
	pages := (total + per - 1) / per
	if pages < 1 {
		pages = 1
	}
	link := func(p int64) string {
		v := url.Values{"filter": {filter}, "page": {strconv.FormatInt(p, 10)}}
		if s := q.Get("q"); s != "" {
			v.Set("q", s)
		}
		return "/admin/problems/password-length?" + v.Encode()
	}
	policy := s.passwordPolicy(ctx)
	var set struct {
		LoginOptions map[string]any `bson:"loginOptions"`
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	_, storedMin := set.LoginOptions["passwordMinLength"]
	_, storedEnforce := set.LoginOptions["passwordMinLengthEnforce"]
	data := map[string]any{"Title": tr(lang, "password-length-report-title"), "User": u,
		"MinLength": policy.MinLength, "Enforce": policy.Enforce, "Saved": q.Get("saved") != "",
		"FromEnvironment": (!storedMin && os.Getenv("PASSWORD_MIN_LENGTH") != "") || (!storedEnforce && os.Getenv("PASSWORD_MIN_LENGTH_ENFORCE") != ""),
		"Filter":          filter, "Search": q.Get("q"), "Rows": rows, "Total": total, "Page": page, "Pages": pages,
		"Filters": []map[string]string{{"Value": "short", "Label": tr(lang, "password-length-filter-short")},
			{"Value": "fixed", "Label": tr(lang, "password-length-filter-fixed")}, {"Value": "all", "Label": tr(lang, "password-length-filter-all")}}}
	if page > 1 {
		data["Prev"] = link(page - 1)
	}
	if page < pages {
		data["Next"] = link(page + 1)
	}
	if e := q.Get("error"); e != "" {
		data["SaveError"] = tr(lang, "password-min-length") + ": 1 - " + strconv.Itoa(passwordMaxMinLength)
	}
	s.render(w, r, "passwordlength", http.StatusOK, data)
}

// savePasswordLength is the page's Save: the minimum (1 to 256; empty takes
// the environment's back) and enforcing it, as Admin Panel's login settings.
func (s *Server) savePasswordLength(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	set, unset := bson.M{}, bson.M{}
	if v := strings.TrimSpace(r.FormValue("minLength")); v == "" {
		unset["loginOptions.passwordMinLength"] = ""
	} else if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= passwordMaxMinLength {
		set["loginOptions.passwordMinLength"] = n
	} else {
		http.Redirect(w, r, "/admin/problems/password-length?error=minLength", http.StatusSeeOther)
		return
	}
	set["loginOptions.passwordMinLengthEnforce"] = r.FormValue("enforce") == "true"
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if _, err := s.db.Collection("settings").UpdateOne(r.Context(), bson.M{}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/problems/password-length?saved=1", http.StatusSeeOther)
}
