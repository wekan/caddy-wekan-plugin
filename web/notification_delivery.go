package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Setting how notifications are delivered (#3695 / #5171), as Node.js WeKan's
// models/lib/notificationDelivery.js and server/methods/notificationDelivery.js:
// the setNotificationDelivery method, and Member Settings' own part of it on
// the profile page - the member's e-mail and tray, and leaving their name out
// of webhooks. Every value is validated before anything is written; a key
// given as null clears it (the next level decides), keys not given are kept.
//
//   scope "admin"        Admin Panel's default, any channel - site admins only
//   scope "board"        targetId a board, any channel      - its admins, or a site admin
//   scope "integration"  targetId a webhook, "webhook"      - its board's admins; a global
//                                                             webhook only site admins
//   scope "member"       the CALLER's own profile: "email" and "tray", and for
//                        "webhook" only { hideIdentity }

var (
	deliveryChannels       = []string{"email", "tray", "webhook"}
	deliveryMemberChannels = []string{"email", "tray"}
	deliveryLayouts        = []string{"classic", "clear"}
	deliveryParts          = []string{"board", "actor", "card", "list", "swimlane", "details", "dates", "link"}
	deliveryGroupings      = map[string][]string{"email": emailGroupings, "tray": {"none", "card", "board"}, "webhook": webhookGroupings}
	deliveryScheduleKeys   = []string{"schedule", "intervalMinutes", "dailyTime", "quietStart", "quietEnd", "timezone"}
	deliveryChannelKeys    = map[string][]string{
		"email":   append([]string{"layout", "parts", "grouping"}, deliveryScheduleKeys...),
		"tray":    append([]string{"layout", "parts", "grouping"}, deliveryScheduleKeys...),
		"webhook": append([]string{"text", "fields", "grouping"}, deliveryScheduleKeys...),
	}
)

// webhookFieldGroups is WeKan's GROUP_KEYS: "standard", then the groups.
var webhookFieldGroups = []string{"standard", "ids", "names", "links", "actor", "people", "details"}

// The words of Member Settings' notification part, named so cmd/sync-i18n copies them.
var _ = []string{msg("notification-delivery-layout"), msg("notification-delivery-layout-classic"), msg("notification-delivery-layout-clear"),
	msg("notification-delivery-parts"), msg("notification-delivery-part-board"), msg("notification-delivery-part-actor"),
	msg("notification-delivery-part-card"), msg("notification-delivery-part-list"), msg("notification-delivery-part-swimlane"),
	msg("notification-delivery-part-details"), msg("notification-delivery-part-dates"), msg("notification-delivery-part-link"),
	msg("notification-delivery-grouping"), msg("notification-delivery-grouping-all"), msg("notification-delivery-grouping-board"),
	msg("notification-delivery-grouping-card"), msg("notification-delivery-grouping-none"),
	msg("notification-delivery-schedule"), msg("notification-delivery-schedule-immediate"), msg("notification-delivery-schedule-interval"),
	msg("notification-delivery-schedule-daily"), msg("notification-delivery-interval"), msg("notification-delivery-minutes"),
	msg("notification-delivery-daily-time"), msg("notification-delivery-quiet"), msg("notification-delivery-quiet-from"),
	msg("notification-delivery-quiet-to"), msg("notification-delivery-quiet-clear"), msg("notification-delivery-timezone"),
	msg("notification-delivery-invalid"), msg("notification-settings-tray"), msg("webhook-hide-identity"), msg("outgoing-webhooks"),
	msg("email"), msg("default"), msg("notifications"), msg("save")}

func deliveryError(text string) error { return &methodError{"invalid-notification-delivery", text} }

func clip40(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// deliveryList is WeKan's list(): known values only, in WeKan's order.
func deliveryList(v any, allowed []string, name string) ([]string, error) {
	items, ok := v.([]any)
	if !ok || len(items) > len(allowed)*2 {
		return nil, deliveryError(name + " must be a list")
	}
	for _, it := range items {
		if s, ok := it.(string); !ok || !slices.Contains(allowed, s) {
			return nil, deliveryError("unknown " + name + " value " + clip40(it))
		}
	}
	out := []string{}
	for _, a := range allowed {
		if slices.Contains(items, any(a)) {
			out = append(out, a)
		}
	}
	return out, nil
}

// normalizeChannelSettings is WeKan's: one channel's settings at one level,
// only the keys set, nil when nothing is (inherit); anything unknown refused.
// in is decoded JSON.
func normalizeChannelSettings(channel string, in any) (bson.M, error) {
	allowed, ok := deliveryChannelKeys[channel]
	if !ok {
		return nil, deliveryError("unknown channel")
	}
	if in == nil {
		return nil, nil
	}
	m, ok := in.(map[string]any)
	if !ok {
		return nil, deliveryError("settings must be an object")
	}
	for k := range m {
		if !slices.Contains(allowed, k) {
			return nil, deliveryError("unknown setting " + clip40(k))
		}
	}
	out := bson.M{}
	for _, k := range allowed {
		v, present := m[k]
		if !present || v == nil {
			continue
		}
		str, isStr := v.(string)
		switch k {
		case "layout":
			if !isStr || !slices.Contains(deliveryLayouts, str) {
				return nil, deliveryError("layout must be classic or clear")
			}
			out[k] = str
		case "parts", "fields":
			allowedList, name := deliveryParts, "parts"
			if k == "fields" {
				allowedList, name = webhookFieldGroups, "field group"
			}
			list, err := deliveryList(v, allowedList, name)
			if err != nil {
				return nil, err
			}
			out[k] = list
		case "text":
			b, ok := v.(bool)
			if !ok {
				return nil, deliveryError("text must be true, false or null")
			}
			out[k] = b
		case "grouping":
			if !isStr || !slices.Contains(deliveryGroupings[channel], str) {
				return nil, deliveryError("unknown grouping")
			}
			out[k] = str
		case "schedule":
			if !isStr || !slices.Contains(emailSchedules, str) {
				return nil, deliveryError("unknown schedule")
			}
			out[k] = str
		case "intervalMinutes":
			n, ok := v.(float64)
			if !ok || n != float64(int(n)) || !slices.Contains(emailIntervals, int(n)) {
				return nil, deliveryError("unknown interval")
			}
			out[k] = int32(n)
		case "dailyTime", "quietStart", "quietEnd":
			if !isStr || !hhmm.MatchString(str) {
				return nil, deliveryError(k + " must be HH:MM")
			}
			out[k] = str
		case "timezone":
			if !isStr || !validTimezone(str) {
				return nil, deliveryError("unknown time zone")
			}
			out[k] = str
		}
	}
	if _, start := out["quietStart"]; start != (out["quietEnd"] != nil) {
		return nil, deliveryError("quiet hours need both a start and an end")
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// normalizeDeliverySettings is WeKan's: a level's whole { email?, tray?, webhook? }.
func normalizeDeliverySettings(in any, channels []string) (bson.M, error) {
	if in == nil {
		return nil, nil
	}
	m, ok := in.(map[string]any)
	if !ok {
		return nil, deliveryError("settings must be an object")
	}
	out := bson.M{}
	for k, v := range m {
		if !slices.Contains(channels, k) {
			return nil, deliveryError("unknown channel " + clip40(k))
		}
		n, err := normalizeChannelSettings(k, v)
		if err != nil {
			return nil, err
		}
		if n != nil {
			out[k] = n
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// asJSON is a stored value as decoded JSON, so a level read from the
// database is validated exactly as one sent by a client.
func asJSON(v any) any {
	raw, err := json.Marshal(plain(v))
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

// mergeDelivery is the method's merge: the change over the stored level,
// validated again as a whole. A nil change clears the level.
func mergeDelivery(channel string, stored any, change map[string]any, clearAll bool) (bson.M, error) {
	if clearAll {
		return nil, nil
	}
	next := map[string]any{}
	if m, ok := asJSON(stored).(map[string]any); ok {
		for k, v := range m {
			next[k] = v
		}
	}
	for k, v := range change {
		if v == nil {
			delete(next, k)
		} else {
			next[k] = v
		}
	}
	return normalizeChannelSettings(channel, next)
}

func deliveryModifier(field string, normalized bson.M) bson.M {
	if normalized == nil {
		return bson.M{"$unset": bson.M{field: ""}}
	}
	return bson.M{"$set": bson.M{field: normalized}}
}

// setNotificationDelivery is the method's work, shared with the profile page.
func (s *Server) setNotificationDelivery(ctx context.Context, u *User, scope, targetID, channel string, change map[string]any, clearAll bool) (any, error) {
	if !slices.Contains([]string{"admin", "board", "integration", "member"}, scope) {
		return nil, &methodError{"invalid-scope", "invalid-scope"}
	}
	if !slices.Contains(deliveryChannels, channel) {
		return nil, &methodError{"invalid-channel", "invalid-channel"}
	}
	if scope == "member" {
		if targetID != "" && targetID != u.ID {
			return nil, &methodError{"not-authorized", "not-authorized"}
		}
		if channel == "webhook" {
			// normalizeMemberWebhookSettings: only hideIdentity, true or false.
			for k := range change {
				if k != "hideIdentity" {
					return nil, deliveryError("unknown setting " + clip40(k))
				}
			}
			v := change["hideIdentity"]
			if _, isBool := v.(bool); v != nil && !isBool {
				return nil, deliveryError("hideIdentity must be true, false or null")
			}
			hide := v == true
			mod := bson.M{"$unset": bson.M{"profile.webhookHideIdentity": ""}}
			if hide {
				mod = bson.M{"$set": bson.M{"profile.webhookHideIdentity": true}}
			}
			if _, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(u.ID)}, mod); err != nil {
				return nil, err
			}
			return map[string]any{"hideIdentity": hide}, nil
		}
		if !slices.Contains(deliveryMemberChannels, channel) {
			return nil, &methodError{"invalid-channel", "invalid-channel"}
		}
	}
	if scope == "integration" && channel != "webhook" {
		return nil, &methodError{"invalid-channel", "invalid-channel"}
	}
	// The change itself first, so an unknown key or value is refused even
	// when it would be merged away.
	if !clearAll {
		probe := map[string]any{}
		for k, v := range change {
			probe[k] = v
		}
		if _, err := normalizeChannelSettings(channel, probe); err != nil {
			return nil, err
		}
	}
	field := "notificationDelivery." + channel
	write := func(coll string, filter bson.M, stored any, prefix string) (any, error) {
		normalized, err := mergeDelivery(channel, stored, change, clearAll)
		if err != nil {
			return nil, err
		}
		if _, err := s.db.Collection(coll).UpdateOne(ctx, filter, deliveryModifier(prefix+field, normalized)); err != nil {
			return nil, err
		}
		if normalized == nil {
			return nil, nil
		}
		return plain(normalized), nil
	}
	switch scope {
	case "member":
		var me bson.M
		_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(u.ID)}).Decode(&me)
		return write("users", bson.M{"_id": eq(u.ID)}, nestedField(me, "profile", "notificationDelivery", channel), "profile.")
	case "admin":
		if !u.IsAdmin {
			return nil, &methodError{"not-authorized", "not-authorized"}
		}
		var set bson.M
		if s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set) != nil {
			return nil, &methodError{"settings-not-found", "settings-not-found"}
		}
		return write("settings", bson.M{"_id": set["_id"]}, nestedField(set, "notificationDelivery", channel), "")
	}
	if !validID(targetID) {
		return nil, &methodError{"not-found", "not-found"}
	}
	adminOfBoard := func(boardID string) bool {
		var b Board
		if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
			return false
		}
		return u.IsAdmin || b.AdminOf(u.ID)
	}
	if scope == "board" {
		var board bson.M
		if !adminOfBoard(targetID) {
			return nil, &methodError{"not-authorized", "not-authorized"}
		}
		_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(targetID)}).Decode(&board)
		return write("boards", bson.M{"_id": eq(targetID)}, nestedField(board, "notificationDelivery", channel), "")
	}
	var hook bson.M
	if s.db.Collection("integrations").FindOne(ctx, bson.M{"_id": eq(targetID)}).Decode(&hook) != nil {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	boardID := docString(hook, "boardId")
	if boardID == "_global" {
		if !u.IsAdmin {
			return nil, &methodError{"not-authorized", "not-authorized"}
		}
	} else if !adminOfBoard(boardID) {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	return write("integrations", bson.M{"_id": eq(targetID), "boardId": eq(boardID)}, nestedField(hook, "notificationDelivery", "webhook"), "")
}

// methodSetNotificationDelivery is WeKan's setNotificationDelivery(scope,
// targetId, channel, settings).
func (s *Server) methodSetNotificationDelivery(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	scope, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	var target *string
	if len(args) < 2 || json.Unmarshal(args[1], &target) != nil {
		return nil, &methodError{"Match.Error", "Expected string or null"}
	}
	channel, err := stringArg(args, 2)
	if err != nil {
		return nil, err
	}
	var settings any
	if len(args) > 3 && json.Unmarshal(args[3], &settings) != nil {
		return nil, &methodError{"Match.Error", "Expected object or null"}
	}
	change, isObject := settings.(map[string]any)
	if settings != nil && !isObject {
		return nil, &methodError{"Match.Error", "Expected object or null"}
	}
	targetID := ""
	if target != nil {
		targetID = *target
	}
	return s.setNotificationDelivery(r.Context(), u, scope, targetID, channel, change, settings == nil)
}

// --- The pages that set a level: Member Settings' part of the profile page,
// and a board's Notifications page for its admins.

type deliveryOption struct {
	Value, Label string
	Selected     bool
}

type deliveryChannelView struct {
	Channel, Title                  string
	IsWebhook                       bool
	Layouts, Groupings, Schedules   []deliveryOption
	Intervals, Texts                []deliveryOption
	Parts, Fields                   []deliveryOption
	PartsDefault, FieldsDefault     bool
	DailyTime, QuietStart, QuietEnd string
	Timezone                        string
}

// deliveryForm is what templates/_delivery.pug shows: one form per channel
// of a level, posted to Action.
type deliveryForm struct {
	Scope, Action string
	Views         []deliveryChannelView
}

// deliveryViews are a level's channels as they stand at that level ("" = the
// default, from the next level). stored gives a channel's stored value.
func deliveryViews(stored func(channel string) any, channels []string, lang string) []deliveryChannelView {
	views := []deliveryChannelView{}
	for _, ch := range channels {
		own, _ := normalizeChannelSettings(ch, asJSON(stored(ch)))
		get := func(k string) string { v, _ := own[k].(string); return v }
		opts := func(key string, values []string, label func(string) string) []deliveryOption {
			cur := ""
			if own[key] != nil {
				cur = fmt.Sprint(own[key])
			}
			out := []deliveryOption{{Value: "", Label: tr(lang, "default"), Selected: cur == ""}}
			for _, v := range values {
				out = append(out, deliveryOption{Value: v, Label: label(v), Selected: cur == v})
			}
			return out
		}
		key := func(prefix string) func(string) string {
			return func(v string) string { return tr(lang, prefix+v) }
		}
		intervals := []string{}
		for _, n := range emailIntervals {
			intervals = append(intervals, fmt.Sprint(n))
		}
		titles := map[string]string{"email": "email", "tray": "notification-settings-tray", "webhook": "outgoing-webhooks"}
		v := deliveryChannelView{Channel: ch, Title: tr(lang, titles[ch]), IsWebhook: ch == "webhook",
			Groupings: opts("grouping", deliveryGroupings[ch], key("notification-delivery-grouping-")),
			Schedules: opts("schedule", emailSchedules, key("notification-delivery-schedule-")),
			Intervals: opts("intervalMinutes", intervals, func(n string) string {
				return fillParams(tr(lang, "notification-delivery-minutes"), map[string]string{"count": n})
			})}
		v.DailyTime, v.Timezone, v.QuietStart, v.QuietEnd = get("dailyTime"), get("timezone"), get("quietStart"), get("quietEnd")
		checklist := func(field string, values []string, prefix string) ([]deliveryOption, bool) {
			mine, isSet := own[field].([]string)
			out := []deliveryOption{}
			for _, x := range values {
				labelKey := prefix + x
				out = append(out, deliveryOption{Value: x, Label: tr(lang, labelKey), Selected: !isSet || slices.Contains(mine, x)})
			}
			return out, !isSet
		}
		if v.IsWebhook {
			v.Texts = opts("text", []string{"true", "false"}, func(b string) string { return tr(lang, map[string]string{"true": "yes", "false": "no"}[b]) })
			v.Fields, v.FieldsDefault = checklist("fields", webhookFieldGroups, "webhook-payload-field-")
		} else {
			v.Layouts = opts("layout", deliveryLayouts, key("notification-delivery-layout-"))
			v.Parts, v.PartsDefault = checklist("parts", deliveryParts, "notification-delivery-part-")
		}
		views = append(views, v)
	}
	return views
}

// The words of the webhook channel's own settings.
var _ = []string{msg("webhook-payload-text"), msg("webhook-payload-fields"), msg("webhook-payload-description"), msg("yes"), msg("no"),
	msg("webhook-payload-field-standard"), msg("webhook-payload-field-ids"), msg("webhook-payload-field-names"),
	msg("webhook-payload-field-links"), msg("webhook-payload-field-actor"), msg("webhook-payload-field-people"),
	msg("webhook-payload-field-details")}

// deliveryChange is one channel's form as a change: every setting at once,
// "" for the default.
func deliveryChange(r *http.Request, channel string) (map[string]any, bool) {
	change := map[string]any{}
	keys := []string{"grouping", "schedule", "dailyTime", "timezone"}
	if channel != "webhook" {
		keys = append(keys, "layout")
	}
	for _, k := range keys {
		change[k] = nil
		if v := strings.TrimSpace(r.FormValue(k)); v != "" {
			change[k] = v
		}
	}
	change["intervalMinutes"] = nil
	if v := r.FormValue("intervalMinutes"); v != "" {
		var n float64
		if _, err := fmt.Sscan(v, &n); err != nil {
			return nil, false
		}
		change["intervalMinutes"] = n
	}
	// Quiet hours are one setting: both ends, or neither.
	start, end := strings.TrimSpace(r.FormValue("quietStart")), strings.TrimSpace(r.FormValue("quietEnd"))
	change["quietStart"], change["quietEnd"] = nil, nil
	if start != "" || end != "" {
		change["quietStart"], change["quietEnd"] = start, end
	}
	list, def := "parts", "partsDefault"
	if channel == "webhook" {
		list, def = "fields", "fieldsDefault"
		change["text"] = nil
		switch r.FormValue("text") {
		case "true":
			change["text"] = true
		case "false":
			change["text"] = false
		case "":
		default:
			return nil, false
		}
	}
	change[list] = nil
	if r.FormValue(def) != "true" {
		picked := []any{}
		for _, p := range r.Form[list] {
			picked = append(picked, p)
		}
		change[list] = picked
	}
	// A schedule is read in the clock of whoever set it: the browser's,
	// when the page could tell; no time zone is UTC.
	if change["timezone"] == nil && (change["schedule"] != nil || change["quietStart"] != nil) {
		if tz := strings.TrimSpace(r.FormValue("browserTimezone")); validTimezone(tz) {
			change["timezone"] = tz
		}
	}
	return change, true
}

// saveMemberDelivery is the profile page's form for one channel.
func (s *Server) saveMemberDelivery(w http.ResponseWriter, r *http.Request, u *User) {
	fail := func() { http.Redirect(w, r, "/profile?error=notification-delivery-invalid", http.StatusSeeOther) }
	if err := r.ParseForm(); err != nil {
		fail()
		return
	}
	channel := r.FormValue("channel")
	change := map[string]any{"hideIdentity": r.FormValue("hideIdentity") == "true"}
	if channel != "webhook" {
		var ok bool
		if change, ok = deliveryChange(r, channel); !ok {
			fail()
			return
		}
	}
	if _, err := s.setNotificationDelivery(r.Context(), u, "member", "", channel, change, false); err != nil {
		var me *methodError
		if errors.As(err, &me) {
			fail()
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/profile?saved=notifications#notification-settings", http.StatusSeeOther)
}

func memberDeliveryForm(user bson.M, lang string) deliveryForm {
	return deliveryForm{Scope: "member", Action: "/profile/notifications", Views: deliveryViews(func(ch string) any {
		return nestedField(user, "profile", "notificationDelivery", ch)
	}, deliveryMemberChannels, lang)}
}

// boardNotificationsPage is WeKan's Board Settings / Notifications, its
// delivery part: how this board's e-mail, tray and webhooks deliver, for the
// board's admins. A member's own setting wins over it, a webhook's own too.
func (s *Server) boardNotificationsPage(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !b.AdminOf(u.ID) {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "board-not-found"), "User": u})
		return
	}
	var board bson.M
	_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(b.ID)}).Decode(&board)
	form := deliveryForm{Scope: "board", Action: "/b/" + b.ID + "/notifications", Views: deliveryViews(func(ch string) any {
		return nestedField(board, "notificationDelivery", ch)
	}, deliveryChannels, lang)}
	s.render(w, r, "boardnotifications", http.StatusOK, map[string]any{"Title": tr(lang, "notifications"), "User": u, "Board": b,
		"Delivery": form, "Hooks": s.boardHookForms(r.Context(), b.ID, lang), "Saved": r.URL.Query().Get("saved"), "Error": r.URL.Query().Get("error")})
}

func (s *Server) saveBoardDelivery(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.saveLevelDelivery(w, r, u, "board", b.ID, "/b/"+b.ID+"/"+b.Slug+"/notifications")
}

// adminNotificationsPage is WeKan's Admin Panel / Notifications, its delivery
// part: the instance's defaults for e-mail, tray and webhooks, for site
// admins. A board's, a webhook's and a member's own win over them.
func (s *Server) adminNotificationsPage(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	if !u.IsAdmin {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "error-notAuthorized"), "User": u})
		return
	}
	var set bson.M
	_ = s.db.Collection("settings").FindOne(r.Context(), bson.M{}).Decode(&set)
	form := deliveryForm{Scope: "admin", Action: "/admin/notifications", Views: deliveryViews(func(ch string) any {
		return nestedField(set, "notificationDelivery", ch)
	}, deliveryChannels, lang)}
	s.render(w, r, "adminnotifications", http.StatusOK, map[string]any{"Title": tr(lang, "notifications"), "User": u,
		"Delivery": form, "Saved": r.URL.Query().Get("saved"), "Error": r.URL.Query().Get("error")})
}

// saveLevelDelivery saves one channel's form at a level, back to the page.
func (s *Server) saveLevelDelivery(w http.ResponseWriter, r *http.Request, u *User, scope, targetID, back string) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, back+"?error=notification-delivery-invalid", http.StatusSeeOther)
		return
	}
	channel := r.FormValue("channel")
	change, ok := deliveryChange(r, channel)
	if !ok {
		http.Redirect(w, r, back+"?error=notification-delivery-invalid", http.StatusSeeOther)
		return
	}
	if _, err := s.setNotificationDelivery(r.Context(), u, scope, targetID, channel, change, false); err != nil {
		var me *methodError
		switch {
		case errors.As(err, &me) && me.Code == "not-authorized":
			http.Error(w, "Forbidden", http.StatusForbidden)
		case errors.As(err, &me):
			http.Redirect(w, r, back+"?error=notification-delivery-invalid", http.StatusSeeOther)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, back+"?saved=notifications", http.StatusSeeOther)
}

func (s *Server) saveAdminDelivery(w http.ResponseWriter, r *http.Request, u *User) {
	s.saveLevelDelivery(w, r, u, "admin", "", "/admin/notifications")
}

// saveHookDelivery is one webhook's own form, WeKan's outgoingWebhooksPopup
// delivery part, on its board's Notifications page.
func (s *Server) saveHookDelivery(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	hookID := r.PathValue("hook")
	if n, _ := s.db.Collection("integrations").CountDocuments(r.Context(), bson.M{"_id": eq(hookID), "boardId": b.ID}); !validID(hookID) || n == 0 {
		http.NotFound(w, r) // a webhook of this board only
		return
	}
	if err := r.ParseForm(); err == nil && r.FormValue("channel") != "webhook" {
		http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/notifications?error=notification-delivery-invalid", http.StatusSeeOther)
		return
	}
	s.saveLevelDelivery(w, r, u, "integration", hookID, "/b/"+b.ID+"/"+b.Slug+"/notifications")
}

// boardHookForms are the forms of a board's own webhooks, each its own level.
func (s *Server) boardHookForms(ctx context.Context, boardID, lang string) []map[string]any {
	out := []map[string]any{}
	cur, err := s.db.Collection("integrations").Find(ctx, bson.M{"boardId": eq(boardID), "bot": bson.M{"$ne": true}})
	if err != nil {
		return out
	}
	var hooks []bson.M
	_ = cur.All(ctx, &hooks)
	for _, h := range hooks {
		title := docString(h, "title")
		if title == "" {
			title = botHost(docString(h, "url"))
		}
		// The partial reads its words as $.L: each webhook's form has its own.
		out = append(out, map[string]any{"L": lang, "ID": docString(h, "_id"), "Title": title, "Delivery": deliveryForm{Scope: "integration",
			Action: "/b/" + boardID + "/integrations/" + docString(h, "_id") + "/notifications",
			Views:  deliveryViews(func(string) any { return nestedField(h, "notificationDelivery", "webhook") }, []string{"webhook"}, lang)}})
	}
	return out
}
