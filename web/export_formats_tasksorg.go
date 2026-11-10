package web

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// A Tasks.org backup its Import backup reads, as Node.js WeKan's
// tasksorgFormat.js writes it - checked byte for byte against it.

// tasksorgID is id(): "wekan-<kind>-" and the value with every UTF-16 unit
// but A-Z a-z 0-9 _ - as "_", as the regex without the u flag sees it.
func tasksorgID(kind, value string) string {
	var b strings.Builder
	for _, u := range utf16.Encode([]rune(value)) {
		c := rune(u)
		if c < 128 && (c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return "wekan-" + kind + "-" + b.String()
}

// tasksorgMillis is WeKan's: an all-day date at noon UTC, a timed one at
// its minute and a second, as Tasks.org keeps them.
func tasksorgMillis(iso string, allDayAware bool) int64 {
	t := parseISO(iso)
	ms := t.UnixMilli()
	if t.IsZero() || ms <= 0 {
		return 0
	}
	if !allDayAware {
		return ms
	}
	if part := ms % (24 * 3600000); part == 0 || part == 12*3600000 {
		return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, time.UTC).UnixMilli()
	}
	return ms - ms%60000 + 1000
}

func formatTasksOrgBackup(d extBoard, now time.Time) any {
	timestamp := now.UnixMilli()
	boardSrc := d.Board.ID
	if boardSrc == "" {
		boardSrc = "export"
	}
	account := tasksorgID("board", boardSrc)
	var calendars []any
	calendarBy := map[string]string{}
	calendarFor := func(listID, title string) string {
		key := listID
		if key == "" {
			t := title
			if t == "" {
				t = "No list"
			}
			key = "title:" + t
		}
		if uuid, ok := calendarBy[key]; ok {
			return uuid
		}
		src := listID
		if src == "" {
			src = title
		}
		if src == "" {
			src = "none"
		}
		uuid := tasksorgID("list", src)
		calendarBy[key] = uuid
		name := title
		if name == "" {
			name = "No list"
		}
		c := jsObject{}
		c.set("account", account)
		c.set("uuid", uuid)
		c.set("name", name)
		c.set("order", len(calendars))
		calendars = append(calendars, c)
		return uuid
	}
	for _, l := range d.Lists {
		calendarFor(l.ID, l.Title)
	}
	tagDefs := jsObject{}
	tagFor := func(name string) string {
		id := tasksorgID("tag", name)
		for _, kv := range tagDefs {
			if kv.Key == name {
				return id
			}
		}
		t := jsObject{}
		t.set("remoteId", id)
		t.set("name", name)
		tagDefs.set(name, t)
		return id
	}
	byCard := map[string]extItem{}
	for _, i := range d.Items {
		byCard[i.CardID] = i // a Map's set: the last card of an id wins
	}
	tasks := []any{}
	backup := func(remoteID, calendar, parent string, task jsObject, tags []string, comments []any) {
		task.set("remoteId", remoteID)
		b := jsObject{}
		b.set("task", task)
		if len(tags) > 0 {
			ts := []any{}
			for _, n := range tags {
				t := jsObject{}
				t.set("name", n)
				t.set("tagUid", tagFor(n))
				ts = append(ts, t)
			}
			b.set("tags", ts)
		}
		if len(comments) > 0 {
			b.set("comments", comments)
		}
		ct := jsObject{}
		ct.set("calendar", calendar)
		ct.set("remoteId", remoteID)
		ct.set("object", remoteID+".ics")
		if parent != "" {
			ct.set("remoteParent", parent)
		}
		b.set("caldavTasks", []any{ct})
		tasks = append(tasks, b)
	}
	for _, i := range d.Items {
		remoteID := tasksorgID("card", i.CardID)
		calendar := calendarFor(i.ListID, i.ListTitle)
		parent := ""
		if p, ok := byCard[i.ParentCardID]; ok && i.ParentCardID != "" && calendarFor(p.ListID, p.ListTitle) == calendar {
			parent = tasksorgID("card", p.CardID)
		}
		created := tasksorgMillis(i.CreatedAt, false)
		if created == 0 {
			created = timestamp
		}
		title := jsTrim(i.Title)
		if title == "" {
			title = "Untitled"
		}
		task := jsObject{}
		task.set("title", title)
		if v := tasksorgMillis(i.DueAt, true); v != 0 {
			task.set("dueDate", v)
		}
		if v := tasksorgMillis(i.StartAt, true); v != 0 {
			task.set("hideUntil", v)
		}
		task.set("creationDate", created)
		task.set("modificationDate", timestamp)
		if v := tasksorgMillis(i.EndAt, false); v != 0 {
			task.set("completionDate", v)
		}
		if i.Description != "" {
			task.set("notes", i.Description)
		}
		if i.SpentTime > 0 {
			task.set("elapsedSeconds", int64(math.Round(i.SpentTime*3600)))
		}
		var comments []any
		n := 0
		for _, c := range i.Comments {
			if jsTrim(c.Text) == "" {
				continue
			}
			when := tasksorgMillis(c.Date, false)
			if when == 0 {
				when = timestamp
			}
			o := jsObject{}
			o.set("remoteId", tasksorgID("comment", i.CardID+"-"+strconv.Itoa(n)))
			o.set("message", c.Text)
			o.set("created", when)
			comments = append(comments, o)
			n++
		}
		var tags []string
		for _, l := range i.Labels {
			if t := jsTrim(l); t != "" && !contains(tags, t) {
				tags = append(tags, t)
			}
		}
		backup(remoteID, calendar, parent, task, tags, comments)
		for c, cl := range i.Checklists {
			for e, x := range cl.Items {
				t := jsTrim(x.Title)
				if t == "" {
					continue
				}
				sub := jsObject{}
				sub.set("title", t)
				sub.set("creationDate", created)
				sub.set("modificationDate", timestamp)
				if x.Done {
					v := tasksorgMillis(i.EndAt, false)
					if v == 0 {
						v = timestamp
					}
					sub.set("completionDate", v)
				}
				backup(tasksorgID("item", i.CardID+"-"+strconv.Itoa(c)+"-"+strconv.Itoa(e)), calendar, remoteID, sub, nil, nil)
			}
		}
	}
	tagList := []any{}
	for _, kv := range tagDefs {
		tagList = append(tagList, kv.Value)
	}
	if calendars == nil {
		calendars = []any{}
	}
	name := d.Board.Title
	if name == "" {
		name = "WeKan board"
	}
	acct := jsObject{}
	acct.set("uuid", account)
	acct.set("name", name)
	acct.set("accountType", 2)
	data := jsObject{}
	for _, kv := range []struct {
		k string
		v any
	}{{"tasks", tasks}, {"places", []any{}}, {"tags", tagList}, {"filters", []any{}}, {"caldavAccounts", []any{acct}},
		{"caldavCalendars", calendars}, {"taskListMetadata", []any{}}, {"taskAttachments", []any{}}, {"intPrefs", jsObject{}},
		{"longPrefs", jsObject{}}, {"stringPrefs", jsObject{}}, {"boolPrefs", jsObject{}}, {"setPrefs", jsObject{}}} {
		data.set(kv.k, kv.v)
	}
	out := jsObject{}
	out.set("version", 151300)
	out.set("timestamp", timestamp)
	out.set("data", data)
	return out
}
