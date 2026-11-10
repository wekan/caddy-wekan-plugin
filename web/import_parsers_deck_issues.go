package web

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NextCloud Deck's board and the GitHub / Gitea / Forgejo issue arrays, as
// Node.js WeKan's externalParsers.js and issueAttachments.js read them -
// checked against WeKan's own parsers (TestImportParsersAreWeKans).

func init() {
	importParsers["deck"] = parseNextcloudDeck
	importParsers["github"] = func(d any) (jsObject, error) { return parseIssuesArray(d, "GitHub") }
	importParsers["gitea"] = func(d any) (jsObject, error) { return parseIssuesArray(d, "Gitea/Forgejo") }
	importParsers["forgejo"] = importParsers["gitea"]
}

// --- NextCloud Deck

func deckImportUser(u any) (any, bool) {
	if !jsTruthy(u) {
		return nil, false
	}
	if s, ok := u.(string); ok {
		return s, true
	}
	who := jsGet(u, "participant")
	if !jsTruthy(who) {
		who = u
	}
	v := jsOr(jsGet(who, "uid"), jsGet(who, "primaryKey"), jsGet(who, "displayname"))
	if !jsTruthy(v) {
		return nil, false
	}
	return v, true
}

func deckLive(item any) bool {
	if !jsTruthy(item) {
		return false
	}
	n, ok := jsToNumber(jsGetOrNil(item, "deletedAt"))
	return !(ok && n > 0)
}

// jsGetOrNil is o[k], undefined as nil - for Number(), where undefined is
// NaN and null is 0, and both are "not above 0".
func jsGetOrNil(o any, k string) any {
	v, ok := jsLookup(o, k)
	if !ok {
		return undefinedValue{}
	}
	return v
}

func deckOrder(item any) float64 {
	n, ok := jsToNumber(jsGetOrNil(item, "order"))
	if !ok || n != n {
		return 0
	}
	return n
}

func sortByOrder(items []any) []any {
	out := append([]any{}, items...)
	sort.SliceStable(out, func(i, j int) bool { return deckOrder(out[i])-deckOrder(out[j]) < 0 })
	return out
}

func filterLive(items []any) []any {
	out := []any{}
	for _, it := range items {
		if deckLive(it) {
			out = append(out, it)
		}
	}
	return out
}

func indexOfItem(items []any, item any) int {
	for n, it := range items {
		if sameJS(it, item) {
			return n
		}
	}
	return -1
}

// sameJS is indexOf's identity for decoded objects: the same jsObject, which
// shares its first entry; two empty objects are never the same one.
func sameJS(a, b any) bool {
	ao, aok := a.(jsObject)
	bo, bok := b.(jsObject)
	return aok && bok && len(ao) > 0 && len(bo) > 0 && &ao[0] == &bo[0]
}

func parseNextcloudDeck(data any) (jsObject, error) {
	board := jsGet(data, "board")
	if !jsTruthy(board) {
		board = data
	}
	var all []any
	if s, ok := jsGet(board, "stacks").([]any); ok {
		all = s
	} else if s, ok := jsGet(data, "stacks").([]any); ok {
		all = s
	}
	stacks := sortByOrder(filterLive(all))
	unsupported, warnings := []any{}, []any{}
	if trashed := len(all) - len(stacks); trashed > 0 {
		warnings = append(warnings, importUnsupportedObj("/stacks", strconv.Itoa(trashed)+" deleted stack(s) skipped"))
	}
	if acl, ok := jsGet(board, "acl").([]any); ok && len(acl) > 0 {
		unsupported = append(unsupported, importUnsupportedObj("/acl", strconv.Itoa(len(acl))+" sharing rule(s): board access is granted in WeKan, not by an import"))
	}
	tasks := []any{}
	for _, stack := range stacks {
		stackIndex := indexOfItem(all, stack)
		cards, _ := jsGet(stack, "cards").([]any)
		live := sortByOrder(filterLive(cards))
		if len(cards) != len(live) {
			warnings = append(warnings, importUnsupportedObj("/stacks/"+strconv.Itoa(stackIndex)+"/cards", strconv.Itoa(len(cards)-len(live))+" deleted card(s) skipped"))
		}
		for _, card := range live {
			at := "/stacks/" + strconv.Itoa(stackIndex) + "/cards/" + strconv.Itoa(indexOfItem(cards, card))
			var people []any
			assigned, _ := jsGet(card, "assignedUsers").([]any)
			for _, a := range assigned {
				if p, ok := deckImportUser(a); ok {
					people = append(people, p)
				}
			}
			var files []any
			if list, ok := jsGet(card, "attachments").([]any); ok {
				for _, f := range filterLive(list) {
					name := jsOr(jsGet(jsGet(jsGet(f, "extendedData"), "info"), "basename"), jsGet(f, "data"), "attachment")
					ref := jsObject{}
					ref.set("boardId", jsGetOrUndef(board, "id"))
					if v, ok := jsLookup(card, "stackId"); ok {
						ref.set("stackId", v)
					} else {
						ref.set("stackId", jsGetOrUndef(stack, "id"))
					}
					ref.set("cardId", jsGetOrUndef(card, "id"))
					ref.set("id", jsGetOrUndef(f, "id"))
					ref.set("type", jsGetOrUndef(f, "type"))
					o := jsObject{}
					o.set("name", jsStr(name))
					o.set("deck", ref)
					files = append(files, o)
				}
			} else if n, ok := jsToNumber(jsGetOrNil(card, "attachmentCount")); ok && n > 0 {
				unsupported = append(unsupported, importUnsupportedObj(at+"/attachments", jsNumber(n)+" attachment(s) not listed in the export (GET .../cards/{id}/attachments lists them)"))
			}
			t := jsObject{}
			if len(files) > 0 {
				t.set("attachments", files)
			}
			t.set("title", jsOr(jsGet(card, "title"), "Imported card"))
			t.set("description", jsOr(jsGet(card, "description"), ""))
			v, def := jsLookup(stack, "title")
			t.setDefined("column_name", v, def)
			t.set("swimlane_name", "Default")
			v, def = jsOrDefined(card, "duedate", "dueDate")
			t.setDefined("date_due", v, def)
			v, def = jsLookup(card, "createdAt")
			t.setDefined("date_creation", v, def)
			done := jsGet(card, "done")
			if s, ok := done.(string); ok {
				t.set("date_end", s)
			}
			if done == true || jsTruthy(done) && func() bool { _, ok := done.(string); return ok }() {
				t.set("due_complete", true)
			}
			t.set("archived", jsGet(card, "archived") == true)
			owner, hasOwner := deckImportUser(jsGet(card, "owner"))
			if len(people) > 0 {
				t.set("owner_username", people[0])
			} else if hasOwner {
				t.set("owner_username", owner)
			}
			rest := []any{}
			if len(people) > 1 {
				rest = people[1:]
			}
			t.set("assignees", rest)
			if hasOwner {
				t.set("requested_by", owner)
			}
			tags := []any{}
			labels, _ := jsOr(jsGet(card, "labels"), []any{}).([]any)
			for _, l := range labels {
				var name any = l
				if _, ok := l.(string); !ok {
					name = nil
					if l != nil {
						name = jsGet(l, "title")
					}
				}
				if jsTruthy(name) {
					tags = append(tags, name)
				}
			}
			t.set("tags", tags)
			comments := []any{}
			list, _ := jsGet(card, "comments").([]any)
			for _, c := range list {
				cm := jsObject{}
				if c == nil {
					cm.set("text", nil)
					cm.set("author", nil)
					cm.set("date", nil)
				} else {
					v, def := jsLookup(c, "message")
					cm.setDefined("text", v, def)
					v, def = jsOrDefined(c, "actorId", "actorDisplayName")
					cm.setDefined("author", v, def)
					v, def = jsLookup(c, "creationDateTime")
					cm.setDefined("date", v, def)
				}
				comments = append(comments, cm)
			}
			t.set("comments", comments)
			tasks = append(tasks, t)
		}
	}
	out := jsObject{}
	b := jsObject{}
	b.set("name", jsOr(jsGet(board, "title"), "Imported NextCloud Deck board"))
	out.set("board", b)
	columns := []any{}
	for _, s := range stacks {
		c := jsObject{}
		v, def := jsLookup(s, "title")
		c.setDefined("title", v, def)
		columns = append(columns, c)
	}
	out.set("columns", columns)
	sw := jsObject{}
	sw.set("name", "Default")
	out.set("swimlanes", []any{sw})
	out.set("tasks", tasks)
	out.set("warnings", warnings)
	out.set("unsupported", unsupported)
	return out, nil
}

// --- issueAttachments.js

const maxAttachmentsPerIssue = 50

var (
	githubFile   = regexp.MustCompile(`(?i)^https://(?:github\.com/user-attachments/(?:assets|files)/|(?:private-)?user-images\.githubusercontent\.com/)`)
	giteaFile    = regexp.MustCompile(`(?i)^https?://[^/\s]+(?:/[^\s]*)?/attachments/[0-9a-f-]{36}$`)
	markdownLink = regexp.MustCompile(`(?i)!?\[([^\]]*)\]\((\S+?)(?:\s+"[^"]*")?\)|<img\b[^>]*\bsrc="([^"]+)"[^>]*>`)
	uuidSegment  = regexp.MustCompile(`(?i)^[0-9a-f-]{36}$`)
	gitlabUpload = regexp.MustCompile(`(?i)^/uploads/[0-9a-f]{32}/[^\s]+$`)
	httpURL      = regexp.MustCompile(`(?i)^https?://`)
)

func nameFromURL(raw, fallback string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if fallback != "" {
			return fallback
		}
		return "attachment"
	}
	var last string
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if seg != "" {
			last = seg
		}
	}
	if dec, err := url.PathUnescape(last); err == nil {
		last = dec
	} else {
		if fallback != "" {
			return fallback
		}
		return "attachment"
	}
	if fallback != "" && uuidSegment.MatchString(last) {
		return fallback
	}
	if last != "" {
		return last
	}
	if fallback != "" {
		return fallback
	}
	return "attachment"
}

func gitlabProjectURL(issue any) string {
	web, ok := jsOr(jsGet(issue, "web_url"), jsGet(issue, "webUrl")).(string)
	if !ok {
		return ""
	}
	if at := strings.Index(web, "/-/issues/"); at > 0 && httpURL.MatchString(web) {
		return web[:at]
	}
	return ""
}

func linkedFiles(text any, issue any) []jsObject {
	var found []jsObject
	body := ""
	if jsTruthy(text) {
		body = jsStr(text)
	}
	project := gitlabProjectURL(issue)
	for _, m := range markdownLink.FindAllStringSubmatch(body, -1) {
		label := jsTrim(m[1])
		u := m[2]
		if u == "" {
			u = m[3]
		}
		u = strings.TrimSuffix(strings.TrimPrefix(u, "<"), ">")
		if project != "" && gitlabUpload.MatchString(u) {
			u = project + u
		}
		if !httpURL.MatchString(u) {
			continue
		}
		isGitlab := project != "" && strings.HasPrefix(u, project+"/uploads/")
		if !githubFile.MatchString(u) && !giteaFile.MatchString(u) && !isGitlab {
			continue
		}
		o := jsObject{}
		o.set("name", nameFromURL(u, label))
		o.set("url", u)
		found = append(found, o)
	}
	return found
}

func assetFiles(assets any) []jsObject {
	var out []jsObject
	list, _ := assets.([]any)
	for _, a := range list {
		u, ok := jsGet(a, "browser_download_url").(string)
		if a == nil || !ok || !httpURL.MatchString(u) {
			continue
		}
		o := jsObject{}
		if n := jsGet(a, "name"); jsTruthy(n) {
			o.set("name", jsStr(n))
		} else {
			o.set("name", nameFromURL(u, ""))
		}
		o.set("url", u)
		if size, ok := jsGet(a, "size").(float64); ok && !isInf(size) && size == size {
			o.set("size", size)
		}
		out = append(out, o)
	}
	return out
}

func issueAttachments(issue any) []any {
	if _, ok := issue.(jsObject); !ok {
		return []any{}
	}
	var comments []any
	for _, k := range []string{"comments_data", "comments", "notes"} {
		if c, ok := jsGet(issue, k).([]any); ok {
			comments = c
			break
		}
	}
	all := assetFiles(jsGet(issue, "assets"))
	all = append(all, linkedFiles(jsOr(jsGet(issue, "body"), jsGet(issue, "description")), issue)...)
	for _, c := range comments {
		if _, ok := c.(jsObject); ok {
			all = append(all, assetFiles(jsGet(c, "assets"))...)
			all = append(all, linkedFiles(jsGet(c, "body"), issue)...)
		}
	}
	seen := map[string]bool{}
	out := []any{}
	for _, f := range all {
		u := jsGet(f, "url").(string)
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, f)
		if len(out) == maxAttachmentsPerIssue {
			break
		}
	}
	return out
}

// --- GitHub / Gitea / Forgejo

func issueComments(issue any, path string, unsupported *[]any) []any {
	key := ""
	if _, ok := jsGet(issue, "comments_data").([]any); ok {
		key = "comments_data"
	} else if _, ok := jsGet(issue, "comments").([]any); ok {
		key = "comments"
	}
	var embedded []any
	if key != "" {
		embedded = jsGet(issue, key).([]any)
	}
	out := []any{}
	for n, c := range embedded {
		body := ""
		if s, ok := jsGet(c, "body").(string); ok {
			body = jsTrim(s)
		}
		if body == "" {
			*unsupported = append(*unsupported, importUnsupportedObj(path+"/"+key+"/"+strconv.Itoa(n), "comment has no text; not imported"))
			continue
		}
		user, ok := jsGet(c, "user").(jsObject)
		if !ok {
			user, _ = jsGet(c, "author").(jsObject)
		}
		author := jsOr(jsGet(user, "login"), jsGet(user, "username"), jsGet(user, "name"))
		if !jsTruthy(author) {
			if s, ok := jsGet(c, "author").(string); ok {
				author = s
			}
		}
		o := jsObject{}
		o.set("text", body)
		if jsTruthy(author) {
			o.set("author", jsStr(author))
		}
		if d := jsOr(jsGet(c, "created_at"), jsGet(c, "date")); jsTruthy(d) {
			o.set("date", d)
		}
		if id := jsGet(c, "id"); id != nil {
			o.set("externalId", jsStr(id))
		}
		out = append(out, o)
	}
	if _, isArr := jsGet(issue, "comments").([]any); !isArr {
		if counted, ok := jsToNumber(jsGetOrNil(issue, "comments")); ok && !isInf(counted) && counted > float64(len(embedded)) {
			*unsupported = append(*unsupported, importUnsupportedObj(path+"/comments",
				jsNumber(counted-float64(len(embedded)))+" comment(s) exist upstream but were not embedded in this export"))
		}
	}
	return out
}

func parseIssuesArray(data any, system string) (jsObject, error) {
	issues, ok := data.([]any)
	if !ok {
		issues, _ = jsOr(jsGet(data, "issues"), []any{}).([]any)
	}
	unsupported := []any{}
	tasks := []any{}
	n := -1
	for _, issue := range issues {
		if jsTruthy(jsGet(issue, "pull_request")) {
			continue
		}
		n++
		path := "/" + strconv.Itoa(n)
		milestone := jsGet(issue, "milestone")
		var milestoneTitle any
		if jsTruthy(milestone) {
			milestoneTitle = jsOr(jsGet(milestone, "title"), jsGet(milestone, "name"))
		}
		var extra []any
		if a, ok := jsGet(issue, "assignees").([]any); ok && len(a) > 1 {
			extra = a[1:]
		}
		tags := []any{}
		labels, _ := jsOr(jsGet(issue, "labels"), []any{}).([]any)
		for _, l := range labels {
			if s, ok := l.(string); ok {
				tags = append(tags, s)
			} else {
				tags = append(tags, jsGetOrUndef(l, "name"))
			}
		}
		if jsTruthy(milestoneTitle) {
			tags = append(tags, "milestone:"+jsStr(milestoneTitle))
		}
		for _, a := range extra {
			if login := jsOr(jsGet(a, "login"), jsGet(a, "username"), jsGet(a, "name")); a != nil && jsTruthy(login) {
				tags = append(tags, "assignee:"+jsStr(login))
			}
		}
		if len(extra) > 0 {
			unsupported = append(unsupported, importUnsupportedObj(path+"/assignees", "only the first assignee becomes the task Owner; the rest are kept as tags"))
		}
		if r := jsGet(issue, "state_reason"); jsTruthy(r) && r != "completed" {
			tags = append(tags, "state_reason:"+jsStr(r))
		}
		var footer []string
		if num := jsGet(issue, "number"); num != nil {
			footer = append(footer, "Source: #"+jsStr(num))
		}
		if u := jsOr(jsGet(issue, "html_url"), jsGet(issue, "url")); jsTruthy(u) {
			footer = append(footer, jsStr(u))
		}
		comments := issueComments(issue, path, &unsupported)
		var desc []string
		for _, s := range []string{jsOrString(jsOr(jsGet(issue, "body"), jsGet(issue, "description"))), strings.Join(footer, " ")} {
			if s != "" {
				desc = append(desc, s)
			}
		}
		t := jsObject{}
		if num := jsGet(issue, "number"); num != nil {
			t.set("externalId", jsStr(num))
		} else if id := jsGet(issue, "id"); id != nil {
			t.set("externalId", jsStr(id))
		}
		t.set("title", jsOr(jsGet(issue, "title"), "Imported issue"))
		t.set("description", jsTrim(strings.Join(desc, "\n\n")))
		if jsGet(issue, "state") == "closed" {
			t.set("column_name", "Closed")
		} else {
			t.set("column_name", "Open")
		}
		t.set("swimlane_name", "Default")
		var due any
		dueDef := false
		if jsTruthy(milestone) {
			due, dueDef = jsOrDefined(milestone, "due_on", "due_date")
		}
		if !jsTruthy(due) {
			due, dueDef = jsLookup(issue, "due_date")
		}
		t.setDefined("date_due", due, dueDef)
		if a := jsGet(issue, "assignee"); jsTruthy(a) {
			if v := jsOr(jsGet(a, "login"), jsGet(a, "username")); jsTruthy(v) {
				t.set("owner_username", v)
			}
		}
		var by any
		if u := jsGet(issue, "user"); jsTruthy(u) {
			by = jsOr(jsGet(u, "login"), jsGet(u, "name"))
		}
		if !jsTruthy(by) {
			if a := jsGet(issue, "author"); jsTruthy(a) {
				by = jsOr(jsGet(a, "login"), jsGet(a, "username"), jsGet(a, "name"))
			}
		}
		if jsTruthy(by) {
			t.set("requested_by", by)
		}
		t.set("tags", tags)
		if len(comments) > 0 {
			t.set("comments", comments)
		}
		if att := issueAttachments(issue); len(att) > 0 {
			t.set("attachments", att)
		}
		tasks = append(tasks, t)
	}
	out := jsObject{}
	b := jsObject{}
	b.set("name", "Imported "+system+" issues")
	out.set("board", b)
	open, closed := jsObject{}, jsObject{}
	open.set("title", "Open")
	closed.set("title", "Closed")
	out.set("columns", []any{open, closed})
	sw := jsObject{}
	sw.set("name", "Default")
	out.set("swimlanes", []any{sw})
	out.set("tasks", tasks)
	out.set("warnings", []any{})
	out.set("unsupported", unsupported)
	return out, nil
}
