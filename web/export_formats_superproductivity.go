package web

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// A Super Productivity backup its "Import from File" reads, as Node.js
// WeKan's superProductivityFormat.js writes it - checked byte for byte
// against it. A list with no title (a card whose list is archived) is the
// "Untitled" list, its tag and its panel alike, as WeKan's since 45ea501d.

func spTheme(primary string) jsObject {
	o := jsObject{}
	for _, kv := range []struct {
		k string
		v any
	}{{"isAutoContrast", true}, {"isDisableBackgroundTint", false}, {"primary", primary}, {"huePrimary", "500"},
		{"accent", "#ff4081"}, {"hueAccent", "500"}, {"warn", "#e11826"}, {"hueWarn", "500"}, {"backgroundImageDark", nil},
		{"backgroundImageLight", nil}, {"backgroundOverlayOpacity", 20}, {"backgroundImageBlur", 0}} {
		o.set(kv.k, kv.v)
	}
	return o
}

func spAdvancedCfg() jsObject {
	w := jsObject{}
	for _, kv := range []struct {
		k string
		v any
	}{{"cols", []string{"DATE", "START", "END", "TIME_CLOCK", "TITLES_INCLUDING_SUB"}}, {"roundWorkTimeTo", nil},
		{"roundStartTimeTo", nil}, {"roundEndTimeTo", nil}, {"separateTasksBy", " | "}, {"groupBy", "DATE"}} {
		w.set(kv.k, kv.v)
	}
	o := jsObject{}
	o.set("worklogExportSettings", w)
	return o
}

func spEmptyArchive() jsObject {
	task := jsObject{}
	task.set("ids", []any{})
	task.set("entities", jsObject{})
	tt := jsObject{}
	tt.set("project", jsObject{})
	tt.set("tag", jsObject{})
	o := jsObject{}
	o.set("task", task)
	o.set("timeTracking", tt)
	o.set("lastTimeTrackingFlush", 0)
	return o
}

func spListKind(title string) string {
	switch strings.ToLower(jsTrim(title)) {
	case "done":
		return "done"
	case "in progress":
		return "progress"
	case "backlog":
		return "backlog"
	case "to do", "todo":
		return "todo"
	}
	return "other"
}

func spChecklistMarkdown(cls []extChecklist) string {
	var blocks []string
	for _, c := range cls {
		if len(c.Items) == 0 {
			continue
		}
		title := c.Title
		if title == "" {
			title = "Checklist"
		}
		lines := []string{"### " + jsTrim(title)}
		for _, x := range c.Items {
			box := " "
			if x.Done {
				box = "x"
			}
			lines = append(lines, "- ["+box+"] "+jsTrim(x.Title))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

type spProject struct {
	id, title        string
	enableBacklog    bool
	taskIDs, backlog []string
}

type spTag struct {
	id, title, primary string
	icon               any
	taskIDs            []string
}

func formatSuperProductivity(d extBoard, now int64) string {
	boardTitle := jsTrim(d.Board.Title)
	if boardTitle == "" {
		boardTitle = "WeKan board"
	}
	cards := d.Items
	lane := func(i extItem) string {
		if i.SwimlaneTitle == "" {
			return "Default"
		}
		return i.SwimlaneTitle
	}
	var laneOrder []string
	for _, l := range d.Swimlanes {
		laneOrder = append(laneOrder, l.Title)
	}
	for _, i := range cards {
		if !contains(laneOrder, lane(i)) {
			laneOrder = append(laneOrder, lane(i))
		}
	}
	var lanesUsed []string
	for _, l := range laneOrder {
		for _, i := range cards {
			if lane(i) == l {
				lanesUsed = append(lanesUsed, l)
				break
			}
		}
	}
	single := len(lanesUsed) <= 1
	var projects []*spProject
	projectIDs := map[string]string{}
	projectByID := map[string]*spProject{}
	titles := lanesUsed
	if single {
		titles = []string{boardTitle}
	}
	for n, t := range titles {
		p := &spProject{id: "wekan-project-" + strconv.Itoa(n+1), title: t, taskIDs: []string{}, backlog: []string{}}
		key := t
		if single {
			key = ""
		}
		projectIDs[key] = p.id
		projectByID[p.id] = p
		projects = append(projects, p)
	}
	projectFor := func(i extItem) string {
		if single {
			return projectIDs[""]
		}
		return projectIDs[lane(i)]
	}
	tags := []*spTag{{id: "TODAY", title: "Today", primary: "#6495ED", icon: "wb_sunny", taskIDs: []string{}}}
	tagByID := map[string]*spTag{"TODAY": tags[0]}
	tagIDByTitle := map[string]string{}
	tagFor := func(title string) string {
		name := jsTrim(title)
		if name == "" {
			return ""
		}
		if id, ok := tagIDByTitle[name]; ok {
			return id
		}
		id := "wekan-tag-" + strconv.Itoa(len(tagIDByTitle)+1)
		tagIDByTitle[name] = id
		t := &spTag{id: id, title: name, primary: "#a05db1", taskIDs: []string{}}
		tags = append(tags, t)
		tagByID[id] = t
		return id
	}
	listTag := func(title string) string {
		t := jsTrim(title)
		if t == "" {
			t = "Untitled"
		}
		return tagFor(t)
	}
	inProgress := func() string {
		if tagByID["KANBAN_IN_PROGRESS"] == nil {
			t := &spTag{id: "KANBAN_IN_PROGRESS", title: "in-progress", primary: "#ffa726", taskIDs: []string{}}
			tags = append(tags, t)
			tagByID[t.id] = t
		}
		return "KANBAN_IN_PROGRESS"
	}
	// Task ids: the card ids, kept unique.
	taskIDOf := make([]string, len(cards))
	used := map[string]bool{}
	for n, i := range cards {
		id := i.CardID
		if id == "" {
			id = "wekan-task-" + strconv.Itoa(n+1)
		}
		for used[id] {
			id = id + "-" + strconv.Itoa(n+1)
		}
		used[id] = true
		taskIDOf[n] = id
	}
	byCard := map[string]int{}
	for n, i := range cards {
		if i.CardID != "" {
			byCard[i.CardID] = n // a Map's set: the last card of an id wins
		}
	}
	// One level of nesting: a sub-task of a sub-task hangs under the
	// top-level ancestor; a cycle has no root.
	rootOf := func(n int) int {
		seen := map[int]bool{n: true}
		parent, ok := byCard[cards[n].ParentCardID]
		root := -1
		for ok && !seen[parent] {
			root = parent
			seen[parent] = true
			parent, ok = byCard[cards[parent].ParentCardID]
		}
		if ok {
			return -1
		}
		return root
	}
	var listTitles []string
	for _, t := range append(d.listTitles(), func() []string {
		var out []string
		for _, i := range cards {
			out = append(out, i.ListTitle)
		}
		return out
	}()...) {
		if !contains(listTitles, t) {
			listTitles = append(listTitles, t)
		}
	}
	panelTasks := map[string][]string{}
	backlogUsed := false
	taskObjs := make([]jsObject, len(cards))
	subTasks := make([][]string, len(cards))
	tagIDsOf := make([][]string, len(cards))
	for n, i := range cards {
		id := taskIDOf[n]
		root := rootOf(n)
		projectID := projectFor(i)
		if root >= 0 {
			projectID = projectFor(cards[root])
		}
		kind := spListKind(i.ListTitle)
		ended, hasEnded := focalboardMillis(i.EndAt)
		isDone := kind == "done" || hasEnded
		var tagIDs []string
		for _, l := range i.Labels {
			if t := tagFor(l); t != "" {
				tagIDs = append(tagIDs, t)
			}
		}
		if kind == "progress" && !isDone {
			tagIDs = append(tagIDs, inProgress())
		}
		if kind == "other" {
			tagIDs = append(tagIDs, listTag(i.ListTitle))
		}
		tagIDs = uniqueNonEmpty(tagIDs)
		var notes []string
		for _, s := range []string{jsTrim(i.Description), spChecklistMarkdown(i.Checklists)} {
			if s != "" {
				notes = append(notes, s)
			}
		}
		created, ok := focalboardMillis(i.CreatedAt)
		if !ok || created == 0 {
			created = now
		}
		timeSpent := int64(0)
		if i.SpentTime > 0 {
			timeSpent = int64(math.Round(i.SpentTime * 3600000))
		}
		onDay := jsObject{}
		if timeSpent != 0 {
			at := created
			if hasEnded && ended != 0 {
				at = ended
			}
			onDay.set(time.UnixMilli(at).UTC().Format("2006-01-02"), timeSpent)
		}
		title := jsTrim(i.Title)
		if title == "" {
			title = "Untitled"
		}
		t := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", id}, {"title", title}, {"subTaskIds", nil}, {"timeSpentOnDay", onDay}, {"timeSpent", timeSpent},
			{"timeEstimate", 0}, {"isDone", isDone}, {"notes", strings.Join(notes, "\n\n")}, {"tagIds", tagIDs},
			{"created", created}, {"attachments", []any{}}, {"projectId", projectID}} {
			t.set(kv.k, kv.v)
		}
		if root >= 0 {
			t.set("parentId", taskIDOf[root])
		}
		if isDone {
			v := now
			if hasEnded && ended != 0 {
				v = ended
			}
			t.set("doneOn", v)
		}
		if due, ok := focalboardMillis(i.DueAt); ok {
			if du := time.UnixMilli(due).UTC(); du.Format("15:04:05.000") == "00:00:00.000" {
				t.set("dueDay", du.Format("2006-01-02"))
			} else {
				t.set("dueWithTime", due)
			}
		}
		taskObjs[n], tagIDsOf[n] = t, tagIDs
		for _, tid := range tagIDs {
			tagByID[tid].taskIDs = append(tagByID[tid].taskIDs, id)
		}
		panelTasks[i.ListTitle] = append(panelTasks[i.ListTitle], id)
		if root >= 0 {
			continue // listed in its parent's subTaskIds, not in the project
		}
		p := projectByID[projectID]
		if kind == "backlog" && !isDone {
			p.backlog = append(p.backlog, id)
			p.enableBacklog = true
			backlogUsed = true
		} else {
			p.taskIDs = append(p.taskIDs, id)
		}
	}
	for n := range cards {
		if root := rootOf(n); root >= 0 {
			subTasks[root] = append(subTasks[root], taskIDOf[n])
		}
	}
	var otherTags []string
	for _, t := range listTitles {
		if spListKind(t) == "other" {
			otherTags = append(otherTags, listTag(t))
		}
	}
	var panels []any
	for n, t := range listTitles {
		title := t
		if title == "" {
			title = "Untitled"
		}
		ids := panelTasks[t]
		if ids == nil {
			ids = []string{}
		}
		included, excluded := []string{}, []string{}
		done, backlog := 3, 2
		switch spListKind(t) {
		case "done":
			done, backlog = 2, 1
		case "progress":
			included = []string{inProgress()}
		case "backlog":
			backlog = 3
		case "todo":
			if tagByID["KANBAN_IN_PROGRESS"] != nil {
				excluded = append(excluded, "KANBAN_IN_PROGRESS")
			}
			excluded = append(excluded, otherTags...)
			backlog = 1
			if backlogUsed {
				backlog = 2
			}
		default:
			included = []string{listTag(t)}
		}
		p := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", "wekan-panel-" + strconv.Itoa(n+1)}, {"title", title}, {"taskIds", ids}, {"includedTagIds", included},
			{"excludedTagIds", excluded}, {"projectIds", []string{""}}, {"taskDoneState", done}, {"scheduledState", 1},
			{"backlogState", backlog}, {"isParentTasksOnly", false}} {
			p.set(kv.k, kv.v)
		}
		panels = append(panels, p)
	}
	// The state, built once every task, tag and project is complete.
	entities := func(keys []string, of func(string) any) jsObject {
		o := jsObject{}
		for _, k := range keys {
			o.set(k, of(k))
		}
		return o
	}
	var pIDs []string
	for _, p := range projects {
		pIDs = append(pIDs, p.id)
	}
	projectState := jsObject{}
	projectState.set("ids", pIDs)
	projectState.set("entities", entities(pIDs, func(id string) any {
		p := projectByID[id]
		o := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", p.id}, {"title", p.title}, {"isHiddenFromMenu", false}, {"isArchived", false}, {"isEnableBacklog", p.enableBacklog},
			{"taskIds", p.taskIDs}, {"backlogTaskIds", p.backlog}, {"noteIds", []any{}}, {"icon", nil}, {"theme", spTheme("#29a1aa")},
			{"advancedCfg", spAdvancedCfg()}, {"created", now}} {
			o.set(kv.k, kv.v)
		}
		return o
	}))
	taskIDs := append([]string{}, taskIDOf...)
	for n := range taskObjs {
		st := subTasks[n]
		if st == nil {
			st = []string{}
		}
		taskObjs[n].set("subTaskIds", st)
	}
	taskEntities := jsObject{}
	for n, id := range taskIDOf {
		taskEntities.set(id, taskObjs[n])
	}
	taskState := jsObject{}
	for _, kv := range []struct {
		k string
		v any
	}{{"ids", taskIDs}, {"entities", taskEntities}, {"currentTaskId", nil}, {"selectedTaskId", nil},
		{"lastCurrentTaskId", nil}, {"isDataLoaded", false}} {
		taskState.set(kv.k, kv.v)
	}
	var tIDs []string
	for _, t := range tags {
		tIDs = append(tIDs, t.id)
	}
	tagState := jsObject{}
	tagState.set("ids", tIDs)
	tagState.set("entities", entities(tIDs, func(id string) any {
		t := tagByID[id]
		o := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", t.id}, {"title", t.title}, {"color", nil}, {"created", now}, {"taskIds", t.taskIDs}, {"icon", t.icon},
			{"theme", spTheme(t.primary)}, {"advancedCfg", spAdvancedCfg()}} {
			o.set(kv.k, kv.v)
		}
		return o
	}))
	note := jsObject{}
	note.set("ids", []any{})
	note.set("entities", jsObject{})
	note.set("todayOrder", []any{})
	boardCfgs := []any{}
	if len(panels) > 0 {
		b := jsObject{}
		b.set("id", "wekan-board")
		b.set("title", boardTitle)
		b.set("cols", min(len(panels), 4))
		b.set("panels", panels)
		boardCfgs = append(boardCfgs, b)
	}
	boards := jsObject{}
	boards.set("boardCfgs", boardCfgs)
	data := jsObject{}
	for _, kv := range []struct {
		k string
		v any
	}{{"project", projectState}, {"task", taskState}, {"tag", tagState}, {"note", note}, {"boards", boards},
		{"archiveYoung", spEmptyArchive()}, {"archiveOld", spEmptyArchive()}} {
		data.set(kv.k, kv.v)
	}
	out := jsObject{}
	out.set("timestamp", now)
	out.set("lastUpdate", now)
	out.set("crossModelVersion", 4.5)
	out.set("data", data)
	return string(jsJSON(out, ""))
}
