package web

import (
	"archive/zip"
	"bytes"
	"regexp"
	"strings"
	"time"
)

// Vikunja's user data export, as Node.js WeKan's vikunjaFormat.js writes it
// (formatVikunja, vikunjaArchiveFiles) and server/lib/vikunjaArchive.js zips
// it: data.json, filters.json and VERSION. data.json is checked byte for byte
// against WeKan's.

var vikunjaParagraphs = regexp.MustCompile(`\n{2,}`)

var vikunjaEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// vikunjaHTML is vikunjaHtml(): paragraphs, line breaks, and TipTap task
// lists, each under a heading with its title but a single "Checklist".
func vikunjaHTML(text string, checklists []extChecklist) string {
	var out []string
	norm := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for _, part := range vikunjaParagraphs.Split(norm, -1) {
		if t := jsTrim(part); t != "" {
			lines := strings.Split(t, "\n")
			for k, l := range lines {
				lines[k] = vikunjaEscape.Replace(l)
			}
			out = append(out, "<p>"+strings.Join(lines, "<br>")+"</p>")
		}
	}
	var lists []extChecklist
	for _, c := range checklists {
		if len(c.Items) > 0 {
			lists = append(lists, c)
		}
	}
	for _, c := range lists {
		title := jsTrim(c.Title)
		if title != "" && !(len(lists) == 1 && title == "Checklist") {
			out = append(out, "<h3>"+vikunjaEscape.Replace(title)+"</h3>")
		}
		var b strings.Builder
		b.WriteString(`<ul data-type="taskList">`)
		for _, x := range c.Items {
			checked := "false"
			if x.Done {
				checked = "true"
			}
			b.WriteString(`<li data-checked="` + checked + `" data-type="taskItem"><p>` + vikunjaEscape.Replace(jsTrim(x.Title)) + `</p></li>`)
		}
		b.WriteString(`</ul>`)
		out = append(out, b.String())
	}
	return strings.Join(out, "")
}

func vikunjaTime(iso string) string {
	if iso == "" || strings.HasPrefix(iso, "0001-01-01") {
		return "0001-01-01T00:00:00Z"
	}
	if t := parseISO(iso); !t.IsZero() {
		return isoDate(t)
	}
	return "0001-01-01T00:00:00Z"
}

// formatVikunja is WeKan's formatVikunja and vikunjaArchiveFiles together:
// data.json's text.
func formatVikunja(d extBoard, now time.Time) string {
	stamp := isoDate(now)
	const step = 65536
	title := jsTrim(d.Board.Title)
	if title == "" {
		title = "WeKan board"
	}
	var lanes []string
	for _, l := range d.Swimlanes {
		t := jsTrim(l.Title)
		if t == "" {
			t = "Default"
		}
		lanes = append(lanes, t)
	}
	several := len(lanes) > 1
	counters := map[string]int{}
	next := func(kind string) int { counters[kind]++; return counters[kind] }
	users := map[string]jsObject{}
	var userOrder []string
	user := func(name string) jsObject {
		key := jsTrim(name)
		if key == "" {
			return nil
		}
		if u, ok := users[key]; ok {
			return u
		}
		u := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", len(userOrder) + 1}, {"name", ""}, {"username", key}, {"created", stamp}, {"updated", stamp}} {
			u.set(kv.k, kv.v)
		}
		users[key] = u
		userOrder = append(userOrder, key)
		return u
	}
	labels := map[string]jsObject{}
	label := func(name string) jsObject {
		if l, ok := labels[name]; ok {
			return l
		}
		l := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", len(labels) + 1}, {"title", name}, {"description", ""}, {"hex_color", ""}, {"created", stamp}, {"updated", stamp}} {
			l.set(kv.k, kv.v)
		}
		labels[name] = l
		return l
	}
	untitled := func(s string) string {
		if t := jsTrim(s); t != "" {
			return t
		}
		return "Untitled"
	}
	var listTitles []string
	wip := map[string]float64{}
	for _, l := range d.Lists {
		listTitles = append(listTitles, untitled(l.Title))
	}
	for _, i := range d.Items {
		if n := untitled(i.ListTitle); !contains(listTitles, n) {
			listTitles = append(listTitles, n)
		}
	}
	for _, l := range d.Lists {
		v := 0.0
		if l.WipLimit != nil && l.WipLimit.Enabled && l.WipLimit.Value > 0 {
			v = l.WipLimit.Value
		}
		wip[untitled(l.Title)] = v // a Map's set: the last list of a title wins
	}
	project := func(id int, title string, parent int, position int, views, buckets []any) jsObject {
		o := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", id}, {"title", title}, {"description", ""}, {"identifier", ""}, {"hex_color", ""}, {"parent_project_id", parent},
			{"is_archived", false}, {"background_information", nil}, {"position", position}, {"views", views}, {"child_projects", nil},
			{"tasks", []any{}}, {"buckets", buckets}, {"task_buckets", []any{}}, {"positions", []any{}}, {"background_file_id", 0},
			{"created", stamp}, {"updated", stamp}} {
			o.set(kv.k, kv.v)
		}
		return o
	}
	var projects []jsObject
	parentID := 0
	if several {
		parentID = next("project")
		projects = append(projects, project(parentID, title, 0, step, []any{}, []any{}))
	}
	type laneProject struct {
		obj                jsObject
		id, kanban, doneID int
		buckets            []jsObject
		tasks, tb, pos     []any
	}
	laneTitles := lanes
	if !several {
		laneTitles = []string{title}
	}
	var lps []*laneProject
	for n, lt := range laneTitles {
		pid := next("project")
		var views []jsObject
		for vi, kind := range []string{"list", "gantt", "table", "kanban"} {
			mode := "none"
			if kind == "kanban" {
				mode = "manual"
			}
			v := jsObject{}
			for _, kv := range []struct {
				k string
				v any
			}{{"id", next("view")}, {"title", strings.ToUpper(kind[:1]) + kind[1:]}, {"project_id", pid}, {"view_kind", kind},
				{"filter", nil}, {"position", (vi + 1) * 100}, {"bucket_configuration_mode", mode}, {"bucket_configuration", nil},
				{"default_bucket_id", 0}, {"done_bucket_id", 0}, {"created", stamp}, {"updated", stamp}} {
				v.set(kv.k, kv.v)
			}
			views = append(views, v)
		}
		kanbanID := views[3][0].Value.(int)
		var buckets []jsObject
		for bi, bt := range listTitles {
			b := jsObject{}
			for _, kv := range []struct {
				k string
				v any
			}{{"id", next("bucket")}, {"title", bt}, {"project_view_id", kanbanID}, {"limit", wip[bt]}, {"position", (bi + 1) * step},
				{"created", stamp}, {"updated", stamp}} {
				b.set(kv.k, kv.v)
			}
			buckets = append(buckets, b)
		}
		def, done := 0, 0
		if len(buckets) > 0 {
			def = buckets[0][0].Value.(int)
		}
		for _, b := range buckets {
			if strings.EqualFold(b[1].Value.(string), "done") {
				done = b[0].Value.(int)
				break
			}
		}
		views[3].set("default_bucket_id", def)
		views[3].set("done_bucket_id", done)
		va, ba := []any{}, []any{}
		for _, v := range views {
			va = append(va, v)
		}
		for _, b := range buckets {
			ba = append(ba, b)
		}
		lps = append(lps, &laneProject{obj: project(pid, lt, parentID, (n+1)*step, va, ba), id: pid, kanban: kanbanID, doneID: done,
			buckets: buckets, tasks: []any{}, tb: []any{}, pos: []any{}})
	}
	type taskRef struct {
		taskID int
		task   jsObject
	}
	refs := map[string]*taskRef{}
	for n, i := range d.Items {
		if _, ok := refs[i.CardID]; i.CardID != "" && !ok {
			refs[i.CardID] = &taskRef{taskID: n + 1}
		}
	}
	checklists := map[int][]extChecklist{}
	related := map[int]*jsObject{}
	for n, i := range d.Items {
		taskID := n + 1
		lane := 0
		if several {
			lane = max(0, indexOf(lanes, untitled2(i.SwimlaneTitle, "Default")))
		}
		p := lps[lane]
		var bucket jsObject
		for _, b := range p.buckets {
			if b[1].Value == untitled(i.ListTitle) {
				bucket = b
				break
			}
		}
		if bucket == nil && len(p.buckets) > 0 {
			bucket = p.buckets[0]
		}
		bucketID := 0
		if bucket != nil {
			bucketID = bucket[0].Value.(int)
		}
		isDone := bucket != nil && bucketID == p.doneID
		var assignees []any
		seen := map[string]bool{}
		for _, name := range append([]string{i.Owner}, i.Assignees...) {
			if u := user(name); u != nil && !seen[jsTrim(name)] {
				seen[jsTrim(name)] = true
				assignees = append(assignees, u)
			}
		}
		if assignees == nil {
			assignees = []any{}
		}
		var labelNames []string
		for _, l := range i.Labels {
			if t := jsTrim(l); t != "" && !contains(labelNames, t) {
				labelNames = append(labelNames, t)
			}
		}
		labelObjs := []any{}
		for _, l := range labelNames {
			labelObjs = append(labelObjs, label(l))
		}
		position := (len(p.tasks) + 1) * step
		name := jsTrim(i.Title)
		if name == "" {
			name = "Untitled"
		}
		doneAt := "0001-01-01T00:00:00Z"
		if isDone {
			e := i.EndAt
			if e == "" {
				e = stamp
			}
			doneAt = vikunjaTime(e)
		}
		created := i.CreatedAt
		if created == "" {
			created = stamp
		}
		rel := &jsObject{}
		related[taskID] = rel
		t := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", taskID}, {"title", name}, {"description", ""}, {"done", isDone}, {"done_at", doneAt},
			{"due_date", vikunjaTime(i.DueAt)}, {"start_date", vikunjaTime(i.StartAt)}, {"end_date", vikunjaTime(i.EndAt)},
			{"reminders", []any{}}, {"repeat_after", 0}, {"repeat_mode", 0}, {"priority", 0}, {"assignees", assignees},
			{"labels", labelObjs}, {"hex_color", ""}, {"percent_done", 0}, {"identifier", ""}, {"index", len(p.tasks) + 1},
			{"project_id", p.id}, {"related_tasks", rel}, {"attachments", []any{}}, {"cover_image_attachment_id", 0},
			{"is_favorite", false}, {"bucket_id", bucketID}, {"position", position}, {"created", vikunjaTime(created)},
			{"updated", stamp}} {
			t.set(kv.k, kv.v)
		}
		by := i.Creator
		if by == "" {
			by = i.RequestedBy
		}
		if u := user(by); u != nil {
			t.set("created_by", u)
		}
		comments := []any{}
		for _, c := range i.Comments {
			if jsTrim(c.Text) == "" {
				continue
			}
			when := c.Date
			if when == "" {
				when = stamp
			}
			o := jsObject{}
			o.set("id", next("comment"))
			o.set("comment", vikunjaHTML(c.Text, nil))
			if u := user(c.Author); u != nil {
				o.set("author", u)
			}
			o.set("created", vikunjaTime(when))
			o.set("updated", vikunjaTime(when))
			comments = append(comments, o)
		}
		t.set("comments", comments)
		has := false
		for _, c := range i.Checklists {
			has = has || len(c.Items) > 0
		}
		if has {
			checklists[taskID] = i.Checklists
		}
		t.set("description", vikunjaHTML(i.Description, checklists[taskID]))
		p.tasks = append(p.tasks, t)
		if r, ok := refs[i.CardID]; ok && r.taskID == taskID {
			r.task = t
		}
		if bucket != nil {
			o := jsObject{}
			o.set("bucket_id", bucketID)
			o.set("task_id", taskID)
			o.set("project_view_id", p.kanban)
			p.tb = append(p.tb, o)
		}
		o := jsObject{}
		o.set("task_id", taskID)
		o.set("project_view_id", p.kanban)
		o.set("position", position)
		p.pos = append(p.pos, o)
	}
	// Parent cards as Vikunja's subtask/parenttask pair, by id only.
	for _, i := range d.Items {
		child, okc := refs[i.CardID]
		parent, okp := refs[i.ParentCardID]
		if i.CardID == "" || i.ParentCardID == "" || !okc || !okp || child.task == nil || parent.task == nil || child.taskID == parent.taskID {
			continue
		}
		ref := func(id int) jsObject { o := jsObject{}; o.set("id", id); return o }
		related[child.taskID].set("parenttask", []any{ref(parent.taskID)})
		pr := related[parent.taskID]
		var subs []any
		for _, kv := range *pr {
			if kv.Key == "subtask" {
				subs = kv.Value.([]any)
			}
		}
		pr.set("subtask", append(subs, ref(child.taskID)))
	}
	all := []any{}
	for _, p := range projects {
		all = append(all, p)
	}
	for _, p := range lps {
		p.obj.set("tasks", p.tasks)
		p.obj.set("task_buckets", p.tb)
		p.obj.set("positions", p.pos)
		all = append(all, p.obj)
	}
	return string(jsJSON(all, ""))
}

func untitled2(s, fallback string) string {
	if t := jsTrim(s); t != "" {
		return t
	}
	return fallback
}

// vikunjaArchive is the export zip: data.json, filters.json and VERSION
// (no trailing newline, as Vikunja reads it).
func vikunjaArchive(data string) []byte {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, f := range []struct{ name, text string }{{"data.json", data}, {"filters.json", "[]"}, {"VERSION", "v1.0.0"}} {
		w, err := z.Create(f.name)
		if err != nil {
			return nil
		}
		_, _ = w.Write([]byte(f.text))
	}
	_ = z.Close()
	return buf.Bytes()
}
