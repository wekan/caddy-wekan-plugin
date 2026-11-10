package web

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// A Taiga project dump, as Node.js WeKan's formatTaiga
// (models/lib/taigaFormat.js) writes it - every key Taiga's
// ProjectExportSerializer writes, in its order: the lists as user story
// statuses, cards as stories, the Epics and Issues swimlanes' cards as epics
// and issues, subtask cards and a story's checklist items as tasks, comments
// as history, and the board's labels as tag colors. Weddy leaves custom
// fields out of exports and has no sprints yet: the dump is WeKan's for a
// board without them - no custom attributes, no milestones, every story's
// points "?".

var (
	taigaTaskStatuses  = [][2]any{{"New", false}, {"In progress", false}, {"Ready for test", true}, {"Closed", true}, {"Needs Info", false}}
	taigaIssueStatuses = [][2]any{{"New", false}, {"In progress", false}, {"Ready for test", true}, {"Closed", true}, {"Needs Info", false}, {"Rejected", true}, {"Postponed", false}}
	taigaEpicStatuses  = [][2]any{{"New", false}, {"Ready", false}, {"In progress", false}, {"Ready for test", false}, {"Done", true}}
	taigaRolePerms     = []string{"view_project", "view_milestones", "add_milestone", "modify_milestone", "delete_milestone",
		"view_epics", "add_epic", "modify_epic", "comment_epic", "delete_epic",
		"view_us", "add_us", "modify_us", "comment_us", "delete_us",
		"view_tasks", "add_task", "modify_task", "comment_task", "delete_task",
		"view_issues", "add_issue", "modify_issue", "comment_issue", "delete_issue",
		"view_wiki_pages", "add_wiki_page", "modify_wiki_page", "comment_wiki_page", "delete_wiki_page",
		"view_wiki_links", "add_wiki_link", "modify_wiki_link", "delete_wiki_link"}
	taigaClosedList  = regexp.MustCompile(`(?i)done|closed|complete|archiv|finished`)
	taigaStatusColor = "#70728F"
	taigaFacets      = []string{"type", "priority", "severity"}
	taigaMarks       = regexp.MustCompile(`[\x{0300}-\x{036f}]`)
	taigaNotSlug     = regexp.MustCompile(`[^a-z0-9]+`)
	taigaEdgeDashes  = regexp.MustCompile(`^-+|-+$`)
	taigaHexColor    = regexp.MustCompile(`(?i)^#[0-9a-f]{6}$`)
)

// taigaSlug is WeKan's: the text without accents, lower case, dashed, at
// most 50 long, numbered when it is taken.
func taigaSlug(text string, used map[string]bool) string {
	base := taigaMarks.ReplaceAllString(norm.NFKD.String(text), "")
	base = strings.ToLower(base)
	base = taigaEdgeDashes.ReplaceAllString(taigaNotSlug.ReplaceAllString(base, "-"), "")
	base = jsSlice16(base, 50)
	if base == "" {
		base = "item"
	}
	slug := base
	for n := 2; used != nil && used[slug]; n++ {
		slug = base + "-" + strconv.Itoa(n)
	}
	if used != nil {
		used[slug] = true
	}
	return slug
}

func taigaUniqueName(name string, used map[string]bool) string {
	base := jsOr(jsTrim(name), "Untitled").(string)
	out := base
	for n := 2; used[out]; n++ {
		out = base + " (" + strconv.Itoa(n) + ")"
	}
	used[out] = true
	return out
}

func taigaHexOf(color string) any {
	if taigaHexColor.MatchString(color) {
		return strings.ToLower(color)
	}
	for _, c := range taigaLabelHex {
		if c.name == strings.ToLower(color) {
			return c.hex
		}
	}
	return nil
}

// taigaDateTimeText is WeKan's: an ISO date as Taiga's "...+0000", or null.
func taigaDateTimeText(iso string) any {
	if iso == "" {
		return nil
	}
	t, ok := jsDateV8(iso)
	if !ok {
		return nil
	}
	return jsISO(t)[:19] + "+0000"
}

func taigaNowText(now time.Time) string { return jsISO(now)[:19] + "+0000" }

func taigaChecklistMarkdown(checklists []extChecklist) string {
	var blocks []string
	for _, c := range checklists {
		if len(c.Items) == 0 {
			continue
		}
		lines := []string{"### " + jsOr(jsTrim(c.Title), "Checklist").(string)}
		for _, it := range c.Items {
			mark := " "
			if it.Done {
				mark = "x"
			}
			lines = append(lines, "- ["+mark+"] "+it.Title)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

func taigaHistory(comments []extComment, now time.Time) []any {
	out := []any{}
	for _, c := range comments {
		if jsTrim(c.Text) == "" {
			continue
		}
		user := []any{}
		if c.Author != "" {
			user = []any{c.Author, c.Author}
		}
		h := jsObject{}
		h.set("user", user)
		h.set("diff", jsObject{})
		h.set("snapshot", nil)
		h.set("values", jsObject{})
		h.set("comment", c.Text)
		h.set("delete_comment_date", nil)
		h.set("delete_comment_user", []any{})
		h.set("comment_versions", nil)
		h.set("created_at", jsOr(taigaDateTimeText(c.Date), taigaNowText(now)))
		h.set("edit_comment_date", nil)
		h.set("is_hidden", false)
		h.set("is_snapshot", false)
		h.set("type", 1.0)
		out = append(out, h)
	}
	return out
}

func taigaStatusRows(rows [][2]any) []any {
	slugs := map[string]bool{}
	out := []any{}
	for i, r := range rows {
		o := jsObject{}
		o.set("name", r[0])
		o.set("slug", taigaSlug(r[0].(string), slugs))
		o.set("order", float64(i+1))
		o.set("is_closed", r[1])
		o.set("color", taigaStatusColor)
		out = append(out, o)
	}
	return out
}

func taigaAddStatus(rows *[][2]any, name string) {
	if name == "" {
		return
	}
	for _, r := range *rows {
		if r[0] == name {
			return
		}
	}
	*rows = append(*rows, [2]any{name, taigaClosedList.MatchString(name)})
}

func taigaStatusClosed(rows [][2]any, name string) bool {
	for _, r := range rows {
		if r[0] == name {
			return r[1].(bool)
		}
	}
	return false
}

func taigaPersonOf(s string) string { return jsTrim(s) }

func formatTaiga(d extBoard, now time.Time) jsObject {
	all := d.Items
	nowText := taigaNowText(now)
	byCard := map[string]int{}
	for i, it := range all {
		if _, has := byCard[it.CardID]; !has {
			byCard[it.CardID] = i
		}
	}
	// byCard is a Map: the last of an id wins.
	for i, it := range all {
		byCard[it.CardID] = i
	}
	// What each card is: epic and issue by swimlane, task by its parent.
	kinds := map[string]string{}
	var kindOf func(i int, seen map[string]bool) string
	kindOf = func(i int, seen map[string]bool) string {
		it := all[i]
		if k, ok := kinds[it.CardID]; ok {
			return k
		}
		kind := "story"
		switch it.SwimlaneTitle {
		case "Epics":
			kind = "epic"
		case "Issues":
			kind = "issue"
		default:
			seen[it.CardID] = true
			if p, ok := byCard[it.ParentCardID]; ok && it.ParentCardID != "" && !seen[all[p].CardID] {
				if pk := kindOf(p, seen); pk == "story" || pk == "task" {
					kind = "task"
				}
			}
		}
		kinds[it.CardID] = kind
		return kind
	}
	for i := range all {
		kindOf(i, map[string]bool{})
	}
	storyOf := func(i int) int {
		current := i
		seen := map[string]bool{}
		for current != -1 && kinds[all[current].CardID] == "task" && !seen[all[current].CardID] {
			seen[all[current].CardID] = true
			if p, ok := byCard[all[current].ParentCardID]; ok && all[current].ParentCardID != "" {
				current = p
			} else {
				current = -1
			}
		}
		if current != -1 && kinds[all[current].CardID] == "story" {
			return current
		}
		return -1
	}
	// Refs: one sequence for the whole project, as Taiga numbers them.
	refs := map[string]float64{}
	nextRef := 1.0
	for _, kind := range []string{"epic", "story", "task", "issue"} {
		for _, it := range all {
			if kinds[it.CardID] == kind {
				refs[it.CardID] = nextRef
				nextRef++
			}
		}
	}
	// Statuses: lists for stories; Taiga's defaults plus what is used for the rest.
	usedNames := map[string]bool{}
	usStatusByList := map[string]string{}
	var usStatuses []jsObject
	for idx, l := range d.Lists {
		name := taigaUniqueName(l.Title, usedNames)
		usStatusByList[l.ID] = name
		var wip any
		if l.WipLimit != nil && l.WipLimit.Enabled && l.WipLimit.Value > 0 {
			wip = l.WipLimit.Value
		}
		s := jsObject{}
		s.set("name", name)
		s.set("slug", "")
		s.set("order", float64(idx+1))
		s.set("is_closed", taigaClosedList.MatchString(name))
		s.set("is_archived", false)
		s.set("color", jsOr(taigaHexOf(l.Color), taigaStatusColor))
		s.set("wip_limit", wip)
		usStatuses = append(usStatuses, s)
	}
	if len(usStatuses) == 0 {
		s := jsObject{}
		s.set("name", "New")
		s.set("slug", "")
		s.set("order", 1.0)
		s.set("is_closed", false)
		s.set("is_archived", false)
		s.set("color", taigaStatusColor)
		s.set("wip_limit", nil)
		usStatuses = append(usStatuses, s)
	}
	statusSlugs := map[string]bool{}
	closedStatus := map[string]bool{}
	for i := range usStatuses {
		name := jsGet(usStatuses[i], "name").(string)
		usStatuses[i].set("slug", taigaSlug(name, statusSlugs))
		if jsGet(usStatuses[i], "is_closed") == true {
			closedStatus[name] = true
		}
	}
	storyStatus := func(it extItem) string {
		if n, ok := usStatusByList[it.ListID]; ok && n != "" {
			return n
		}
		for _, s := range usStatuses {
			if jsGet(s, "name") == it.ListTitle {
				return it.ListTitle
			}
		}
		return jsGet(usStatuses[0], "name").(string)
	}
	taskStatuses := append([][2]any{}, taigaTaskStatuses...)
	issueStatuses := append([][2]any{}, taigaIssueStatuses...)
	epicStatuses := append([][2]any{}, taigaEpicStatuses...)
	issueTypes := []string{"Bug", "Question", "Enhancement"}
	priorities := []string{"Low", "Normal", "High"}
	severities := []string{"Wishlist", "Minor", "Normal", "Important", "Critical"}
	// Swimlanes: none when the board has only its Default one.
	var laneTitles []string
	for _, s := range d.Swimlanes {
		if s.Title != "" && s.Title != "Epics" && s.Title != "Issues" {
			laneTitles = addOnce(laneTitles, s.Title)
		}
	}
	taigaLanes := laneTitles
	if len(laneTitles) == 1 && laneTitles[0] == "Default" {
		taigaLanes = nil
	}
	points := []any{}
	q := jsObject{}
	q.set("name", "?")
	q.set("order", 1.0)
	q.set("value", nil)
	points = append(points, q)
	peopleOf := func(it extItem) []any {
		var names []string
		for _, p := range append([]string{it.Owner}, it.Assignees...) {
			names = append(names, taigaPersonOf(p))
		}
		return taigaUniq(names)
	}
	plainTags := func(it extItem) []any {
		var names []string
		for _, l := range it.Labels {
			names = append(names, jsTrim(l))
		}
		return taigaUniq(names)
	}
	common := func(it extItem, kind string) jsObject {
		assigned := peopleOf(it)
		o := jsObject{}
		owner := jsOr(taigaPersonOf(it.Creator), taigaPersonOf(it.RequestedBy))
		if owner == "" {
			owner = nil
		}
		o.set("owner", owner)
		if len(assigned) > 0 {
			o.set("assigned_to", assigned[0])
		} else {
			o.set("assigned_to", nil)
		}
		o.set("modified_date", jsOr(taigaDateTimeText(it.CreatedAt), nowText))
		o.set("created_date", jsOr(taigaDateTimeText(it.CreatedAt), nowText))
		o.set("ref", refs[it.CardID])
		o.set("subject", jsOr(jsTrim(it.Title), "Untitled"))
		if kind == "story" {
			o.set("description", it.Description)
		} else {
			var parts []string
			for _, p := range []string{it.Description, taigaChecklistMarkdown(it.Checklists)} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			o.set("description", strings.Join(parts, "\n\n"))
		}
		o.set("version", 1.0)
		o.set("blocked_note", "")
		o.set("is_blocked", false)
		o.set("attachments", []any{})
		o.set("history", taigaHistory(it.Comments, now))
		o.set("watchers", []any{})
		o.set("custom_attributes_values", jsObject{})
		return o
	}
	epicsOut, storiesOut, tasksOut, issuesOut := []any{}, []any{}, []any{}, []any{}
	kanbanOrder := map[string]float64{}
	storyMilestone := map[float64]any{}
	for _, it := range all {
		switch kinds[it.CardID] {
		case "epic":
			taigaAddStatus(&epicStatuses, it.ListTitle)
			e := common(it, "epic")
			e.set("status", jsOr(it.ListTitle, "New"))
			e.set("epics_order", float64(len(epicsOut)+1))
			e.set("color", jsOr(taigaHexOf(it.Color), taigaStatusColor))
			e.set("client_requirement", false)
			e.set("team_requirement", false)
			e.set("tags", plainTags(it))
			related := []any{}
			for _, child := range all {
				if child.ParentCardID == it.CardID && kinds[child.CardID] == "story" {
					r := jsObject{}
					r.set("user_story", refs[child.CardID])
					r.set("order", float64(len(related)+1))
					r.set("source_project_slug", nil)
					related = append(related, r)
				}
			}
			e.set("related_user_stories", related)
			epicsOut = append(epicsOut, e)
		case "story":
			status := storyStatus(it)
			kanbanOrder[status]++
			var lane any
			if contains(taigaLanes, it.SwimlaneTitle) {
				lane = it.SwimlaneTitle
			} else if len(taigaLanes) > 0 {
				lane = taigaLanes[0]
			}
			assigned := peopleOf(it)
			rest := common(it, "story")
			s := jsObject{}
			s.set("watchers", jsGet(rest, "watchers"))
			rp := jsObject{}
			rp.set("role", "Team")
			rp.set("points", "?")
			s.set("role_points", []any{rp})
			s.set("owner", jsGet(rest, "owner"))
			s.set("assigned_to", jsGet(rest, "assigned_to"))
			s.set("assigned_users", assigned)
			s.set("status", status)
			s.set("swimlane", lane)
			s.set("milestone", nil)
			s.set("modified_date", jsGet(rest, "modified_date"))
			s.set("created_date", jsGet(rest, "created_date"))
			if closedStatus[status] {
				s.set("finish_date", jsOr(taigaDateTimeText(it.EndAt), nowText))
			} else {
				s.set("finish_date", nil)
			}
			s.set("generated_from_issue", nil)
			s.set("generated_from_task", nil)
			s.set("from_task_ref", nil)
			s.set("ref", jsGet(rest, "ref"))
			s.set("is_closed", closedStatus[status])
			s.set("backlog_order", jsGet(rest, "ref"))
			s.set("sprint_order", jsGet(rest, "ref"))
			s.set("kanban_order", kanbanOrder[status])
			s.set("subject", jsGet(rest, "subject"))
			s.set("description", jsGet(rest, "description"))
			s.set("client_requirement", false)
			s.set("team_requirement", false)
			s.set("external_reference", nil)
			s.set("tribe_gig", nil)
			s.set("version", 1.0)
			s.set("blocked_note", "")
			s.set("is_blocked", false)
			s.set("tags", plainTags(it))
			s.set("due_date", taigaDateTimeText(it.DueAt))
			s.set("due_date_reason", "")
			s.set("attachments", []any{})
			s.set("history", jsGet(rest, "history"))
			s.set("custom_attributes_values", jsObject{})
			storyMilestone[refs[it.CardID]] = nil
			storiesOut = append(storiesOut, s)
		case "issue":
			facet := func(name string) string {
				for _, l := range it.Labels {
					if strings.HasPrefix(l, name+":") {
						return jsTrim(l[len(name)+1:])
					}
				}
				return ""
			}
			typ := jsOr(facet("type"), "Bug").(string)
			priority := jsOr(facet("priority"), "Normal").(string)
			severity := jsOr(facet("severity"), "Normal").(string)
			issueTypes = addOnce(issueTypes, typ)
			priorities = addOnce(priorities, priority)
			severities = addOnce(severities, severity)
			status := jsOr(it.ListTitle, "New").(string)
			taigaAddStatus(&issueStatuses, status)
			o := common(it, "issue")
			o.set("status", status)
			o.set("priority", priority)
			o.set("severity", severity)
			o.set("type", typ)
			o.set("milestone", nil)
			o.set("votes", []any{})
			if taigaStatusClosed(issueStatuses, status) {
				o.set("finished_date", jsOr(taigaDateTimeText(it.EndAt), nowText))
			} else {
				o.set("finished_date", nil)
			}
			o.set("external_reference", nil)
			tags := []any{}
			for _, t := range plainTags(it) {
				facetTag := false
				for _, f := range taigaFacets {
					facetTag = facetTag || strings.HasPrefix(t.(string), f+":")
				}
				if !facetTag {
					tags = append(tags, t)
				}
			}
			o.set("tags", tags)
			o.set("due_date", taigaDateTimeText(it.DueAt))
			o.set("due_date_reason", "")
			issuesOut = append(issuesOut, o)
		}
	}
	// Tasks: subtask cards, then a story's checklist items.
	taskOrder := map[any]float64{}
	nextTaskOrder := func(storyRef any) float64 { taskOrder[storyRef]++; return taskOrder[storyRef] }
	pushTask := func(f jsObject, finished any) {
		status := jsGet(f, "status").(string)
		taigaAddStatus(&taskStatuses, status)
		if taigaStatusClosed(taskStatuses, status) {
			f.set("finished_date", jsOr(finished, nowText))
		} else {
			f.set("finished_date", nil)
		}
		tasksOut = append(tasksOut, f)
	}
	for i, it := range all {
		if kinds[it.CardID] != "task" {
			continue
		}
		var storyRef any
		if st := storyOf(i); st != -1 {
			storyRef = refs[all[st].CardID]
		}
		rest := common(it, "task")
		order := nextTaskOrder(storyRef)
		f := jsObject{}
		f.set("owner", jsGet(rest, "owner"))
		status := "New"
		if it.EndAt != "" {
			status = "Closed"
		}
		f.set("status", status)
		f.set("user_story", storyRef)
		f.set("milestone", nil)
		f.set("assigned_to", jsGet(rest, "assigned_to"))
		f.set("modified_date", jsGet(rest, "modified_date"))
		f.set("created_date", jsGet(rest, "created_date"))
		f.set("finished_date", taigaDateTimeText(it.EndAt))
		f.set("ref", jsGet(rest, "ref"))
		f.set("subject", jsGet(rest, "subject"))
		f.set("us_order", order)
		f.set("taskboard_order", order)
		f.set("description", jsGet(rest, "description"))
		f.set("is_iocaine", false)
		f.set("external_reference", nil)
		f.set("version", 1.0)
		f.set("blocked_note", "")
		f.set("is_blocked", false)
		f.set("tags", plainTags(it))
		f.set("due_date", taigaDateTimeText(it.DueAt))
		f.set("due_date_reason", "")
		f.set("attachments", []any{})
		f.set("history", jsGet(rest, "history"))
		f.set("watchers", []any{})
		f.set("custom_attributes_values", jsObject{})
		pushTask(f, taigaDateTimeText(it.EndAt))
	}
	for _, it := range all {
		if kinds[it.CardID] != "story" {
			continue
		}
		storyRef := refs[it.CardID]
		for _, cl := range it.Checklists {
			for _, entry := range cl.Items {
				if jsTrim(entry.Title) == "" {
					continue
				}
				ref := nextRef
				nextRef++
				order := nextTaskOrder(storyRef)
				f := jsObject{}
				f.set("owner", nil)
				f.set("status", map[bool]string{true: "Closed", false: "New"}[entry.Done])
				f.set("user_story", storyRef)
				f.set("milestone", nil)
				f.set("assigned_to", nil)
				f.set("modified_date", nowText)
				f.set("created_date", jsOr(taigaDateTimeText(it.CreatedAt), nowText))
				f.set("finished_date", nil)
				f.set("ref", ref)
				f.set("subject", jsTrim(entry.Title))
				f.set("us_order", order)
				f.set("taskboard_order", order)
				f.set("description", "")
				f.set("is_iocaine", false)
				f.set("external_reference", nil)
				f.set("version", 1.0)
				f.set("blocked_note", "")
				f.set("is_blocked", false)
				f.set("tags", []any{})
				f.set("due_date", nil)
				f.set("due_date_reason", "")
				f.set("attachments", []any{})
				f.set("history", []any{})
				f.set("watchers", []any{})
				f.set("custom_attributes_values", jsObject{})
				pushTask(f, nil)
			}
		}
	}
	// Tags with their colors: every board label but the issue facets.
	tagsColors := []any{}
	for _, l := range d.Board.Labels {
		name := jsTrim(l.Name)
		facetTag := false
		for _, f := range taigaFacets {
			facetTag = facetTag || strings.HasPrefix(l.Name, f+":")
		}
		if name != "" && !facetTag {
			tagsColors = append(tagsColors, []any{name, taigaHexOf(l.Color)})
		}
	}
	title := jsOr(jsTrim(d.Board.Title), "WeKan board").(string)
	facetRows := func(names []string) []any {
		out := []any{}
		for i, n := range names {
			o := jsObject{}
			o.set("name", n)
			o.set("order", float64(i+1))
			o.set("color", taigaStatusColor)
			out = append(out, o)
		}
		return out
	}
	dueDates := func() []any {
		var out []any
		for _, r := range []struct {
			name, color string
			def         bool
			days        any
		}{{"Default", "#9dce0a", true, nil}, {"Due soon", "#ff9900", false, 14.0}, {"Past due", "#ff8a84", false, 0.0}} {
			o := jsObject{}
			o.set("name", r.name)
			o.set("order", float64(len(out)+1))
			o.set("by_default", r.def)
			o.set("color", r.color)
			o.set("days_to_due", r.days)
			out = append(out, o)
		}
		return out
	}
	statusesAny := []any{}
	for _, s := range usStatuses {
		statusesAny = append(statusesAny, s)
	}
	lanes := []any{}
	for i, name := range taigaLanes {
		var sts []any
		for _, s := range usStatuses {
			o := jsObject{}
			o.set("status", jsGet(s, "name"))
			o.set("wip_limit", jsGet(s, "wip_limit"))
			sts = append(sts, o)
		}
		o := jsObject{}
		o.set("name", name)
		o.set("order", float64(i+1))
		o.set("statuses", sts)
		lanes = append(lanes, o)
	}
	role := jsObject{}
	role.set("name", "Team")
	role.set("slug", taigaSlug("Team", nil))
	role.set("order", 10.0)
	role.set("computable", true)
	role.set("permissions", taigaRolePerms)
	var defaultLane any
	if len(taigaLanes) > 0 {
		defaultLane = taigaLanes[0]
	}
	out := jsObject{}
	set := func(k string, v any) { out.set(k, v) }
	set("watchers", []any{})
	set("name", title)
	set("slug", taigaSlug(title, nil))
	set("description", jsOr(jsTrim(d.Board.Description), title))
	set("created_date", jsOr(taigaDateTimeText(d.Board.CreatedAt), nowText))
	set("logo", nil)
	set("total_milestones", nil)
	set("total_story_points", nil)
	set("is_epics_activated", len(epicsOut) > 0)
	set("is_backlog_activated", false)
	set("is_kanban_activated", true)
	set("is_wiki_activated", false)
	set("is_issues_activated", len(issuesOut) > 0)
	for _, k := range []string{"videoconferences", "videoconferences_extra_data", "creation_template"} {
		set(k, nil)
	}
	set("is_private", true)
	set("is_featured", false)
	set("is_looking_for_people", false)
	set("looking_for_people_note", "")
	for _, k := range []string{"epics_csv_uuid", "userstories_csv_uuid", "tasks_csv_uuid", "issues_csv_uuid", "transfer_token", "blocked_code"} {
		set(k, nil)
	}
	set("totals_updated_datetime", nowText)
	for _, k := range []string{"total_fans", "total_fans_last_week", "total_fans_last_month", "total_fans_last_year",
		"total_activity", "total_activity_last_week", "total_activity_last_month", "total_activity_last_year"} {
		set(k, 0.0)
	}
	set("anon_permissions", []any{})
	set("public_permissions", []any{})
	set("modified_date", nowText)
	set("roles", []any{role})
	set("owner", nil)
	set("memberships", []any{})
	set("points", points)
	set("epic_statuses", taigaStatusRows(epicStatuses))
	set("us_statuses", statusesAny)
	set("us_duedates", dueDates())
	set("task_statuses", taigaStatusRows(taskStatuses))
	set("task_duedates", dueDates())
	set("issue_types", facetRows(issueTypes))
	set("issue_statuses", taigaStatusRows(issueStatuses))
	set("issue_duedates", dueDates())
	set("priorities", facetRows(priorities))
	set("severities", facetRows(severities))
	set("swimlanes", lanes)
	set("tags_colors", tagsColors)
	set("default_points", "?")
	set("default_epic_status", "New")
	set("default_us_status", jsGet(usStatuses[0], "name"))
	set("default_task_status", "New")
	set("default_priority", "Normal")
	set("default_severity", "Normal")
	set("default_issue_status", "New")
	set("default_issue_type", "Bug")
	set("default_swimlane", defaultLane)
	for _, k := range []string{"epiccustomattributes", "userstorycustomattributes", "taskcustomattributes", "issuecustomattributes"} {
		set(k, []any{})
	}
	set("epics", epicsOut)
	set("user_stories", storiesOut)
	set("tasks", tasksOut)
	set("milestones", []any{})
	set("issues", issuesOut)
	for _, k := range []string{"wiki_links", "wiki_pages", "tags", "timeline"} {
		set(k, []any{})
	}
	_ = storyMilestone
	return out
}
