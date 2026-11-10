package web

import "strconv"

// The issue trackers' and boards' JSON - NextCloud Deck, Kanboard,
// OpenProject, GitHub, Gitea, Forgejo, GitLab, Trello and Asana - as Node.js
// WeKan's externalExportFormatters.js writes them, key for key, so WeKan's
// importers and theirs read Weddy's export as WeKan's. Weddy has no sprints
// yet, so GitLab and OpenProject go without the Scrum parts, as WeKan writes
// them for a board with none.

var apiFormatters = map[string]func(extBoard) any{
	"deck": formatDeck, "kanboard": formatKanboard, "openproject": formatOpenProject,
	"github": formatGithubLike, "gitea": formatGithubLike, "forgejo": formatGithubLike,
	"gitlab": formatGitlab, "trello": formatTrello, "asana": formatAsana,
}

// checklistEntries is checklistItems(): every checklist's items, in order.
func checklistEntries(i extItem) []extChecklistItem {
	var out []extChecklistItem
	for _, c := range i.Checklists {
		out = append(out, c.Items...)
	}
	return out
}

// people is [owner, ...assignees].filter(has).
func people(i extItem) []string {
	var out []string
	for _, p := range append([]string{i.Owner}, i.Assignees...) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- NextCloud Deck

type deckUID struct {
	UID string `json:"uid"`
}

type deckUser struct {
	Participant deckUID `json:"participant"`
}

type deckComment struct {
	Message          string `json:"message"`
	ActorID          string `json:"actorId,omitempty"`
	CreationDateTime string `json:"creationDateTime,omitempty"`
}

type deckTitle struct {
	Title string `json:"title"`
}

type deckCard struct {
	Title         string        `json:"title"`
	Description   string        `json:"description"`
	Duedate       string        `json:"duedate,omitempty"`
	Labels        []deckTitle   `json:"labels"`
	CreatedAt     string        `json:"createdAt,omitempty"`
	Done          string        `json:"done,omitempty"`
	AssignedUsers []deckUser    `json:"assignedUsers,omitempty"`
	Owner         *deckUID      `json:"owner,omitempty"`
	Comments      []deckComment `json:"comments,omitempty"`
}

type deckStack struct {
	Title string     `json:"title"`
	Cards []deckCard `json:"cards"`
}

func formatDeck(d extBoard) any {
	type deckBoard struct {
		Title  string      `json:"title"`
		Stacks []deckStack `json:"stacks"`
	}
	out := deckBoard{Title: d.Board.Title, Stacks: []deckStack{}}
	for _, l := range d.Lists {
		st := deckStack{Title: l.Title, Cards: []deckCard{}}
		for _, i := range d.Items {
			if i.ListTitle != l.Title {
				continue
			}
			c := deckCard{Title: i.Title, Description: i.Description, Duedate: i.DueAt, Labels: []deckTitle{}, CreatedAt: i.CreatedAt, Done: i.EndAt}
			for _, n := range i.Labels {
				c.Labels = append(c.Labels, deckTitle{Title: n})
			}
			if i.Owner != "" || len(i.Assignees) > 0 {
				c.AssignedUsers = []deckUser{}
				for _, p := range people(i) {
					c.AssignedUsers = append(c.AssignedUsers, deckUser{Participant: deckUID{UID: p}})
				}
			}
			if i.Creator != "" {
				c.Owner = &deckUID{UID: i.Creator}
			}
			for _, cm := range i.Comments {
				c.Comments = append(c.Comments, deckComment{Message: cm.Text, ActorID: cm.Author, CreationDateTime: cm.Date})
			}
			st.Cards = append(st.Cards, c)
		}
		out.Stacks = append(out.Stacks, st)
	}
	return out
}

// --- Kanboard

var kanboardLinkLabel = map[string]string{"related-to": "relates to", "blocks": "blocks", "is-blocked-by": "is blocked by",
	"duplicates": "duplicates", "is-duplicated-by": "is duplicated by", "fixes": "fixes", "is-fixed-by": "is fixed by"}

var kanboardOpposite = map[string]string{"relates to": "relates to", "blocks": "is blocked by", "is blocked by": "blocks",
	"duplicates": "is duplicated by", "is duplicated by": "duplicates", "fixes": "is fixed by", "is fixed by": "fixes",
	"is a child of": "is a parent of", "is a parent of": "is a child of"}

type kanboardLink struct {
	ID     int    `json:"id"`
	TaskID int    `json:"task_id"`
	Label  string `json:"label"`
}

// kanboardTaskLinks is WeKan's: each parent and dependency from both ends.
func kanboardTaskLinks(items []extItem) [][]kanboardLink {
	number := map[string]int{}
	for k, i := range items {
		number[i.CardID] = k + 1 // a Map's set: the last card of an id wins
	}
	links := make([][]kanboardLink, len(items))
	written := map[string]bool{}
	id := 0
	add := func(from, to int, label string) {
		key := itoaPair(from, to) + ":" + label
		if from == to || written[key] {
			return
		}
		written[key] = true
		written[itoaPair(to, from)+":"+kanboardOpposite[label]] = true
		id++
		links[from-1] = append(links[from-1], kanboardLink{ID: id, TaskID: to, Label: label})
		id++
		links[to-1] = append(links[to-1], kanboardLink{ID: id, TaskID: from, Label: kanboardOpposite[label]})
	}
	for k, i := range items {
		self := k + 1
		if parent := number[i.ParentCardID]; i.ParentCardID != "" && parent != 0 {
			add(self, parent, "is a child of")
		}
		for _, dep := range i.Dependencies {
			if other := number[dep.CardID]; other != 0 {
				label := kanboardLinkLabel[dep.Type]
				if label == "" {
					label = "relates to"
				}
				add(self, other, label)
			}
		}
	}
	return links
}

func itoaPair(a, b int) string { return strconv.Itoa(a) + ">" + strconv.Itoa(b) }

type kanboardSubtask struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
}

type kanboardComment struct {
	Comment      string `json:"comment"`
	Username     string `json:"username,omitempty"`
	DateCreation string `json:"date_creation,omitempty"`
}

type kanboardTask struct {
	ID              int               `json:"id"`
	Title           string            `json:"title"`
	Description     string            `json:"description"`
	ColumnName      string            `json:"column_name"`
	SwimlaneName    string            `json:"swimlane_name"`
	DateDue         string            `json:"date_due,omitempty"`
	DateStarted     string            `json:"date_started,omitempty"`
	DateCompleted   string            `json:"date_completed,omitempty"`
	DateCreation    string            `json:"date_creation,omitempty"`
	OwnerUsername   string            `json:"owner_username,omitempty"`
	CreatorUsername string            `json:"creator_username,omitempty"`
	Tags            []string          `json:"tags"`
	Subtasks        []kanboardSubtask `json:"subtasks,omitempty"`
	Comments        []kanboardComment `json:"comments,omitempty"`
	Links           []kanboardLink    `json:"links,omitempty"`
}

type kanboardName struct {
	Name string `json:"name"`
}

func formatKanboard(d extBoard) any {
	type kanboardBoard struct {
		Board     kanboardName   `json:"board"`
		Columns   []deckTitle    `json:"columns"`
		Swimlanes []kanboardName `json:"swimlanes"`
		Tasks     []kanboardTask `json:"tasks"`
	}
	links := kanboardTaskLinks(d.Items)
	out := kanboardBoard{Board: kanboardName{Name: d.Board.Title}, Columns: []deckTitle{}, Swimlanes: []kanboardName{}, Tasks: []kanboardTask{}}
	for _, l := range d.Lists {
		out.Columns = append(out.Columns, deckTitle{Title: l.Title})
	}
	for _, sw := range d.Swimlanes {
		out.Swimlanes = append(out.Swimlanes, kanboardName{Name: sw.Title})
	}
	for k, i := range d.Items {
		t := kanboardTask{ID: k + 1, Title: i.Title, Description: i.Description, ColumnName: i.ListTitle, SwimlaneName: i.SwimlaneTitle,
			DateDue: i.DueAt, DateStarted: i.StartAt, DateCompleted: i.EndAt, DateCreation: i.CreatedAt,
			OwnerUsername: i.Owner, CreatorUsername: i.Creator, Tags: append([]string{}, i.Labels...), Links: links[k]}
		for _, x := range checklistEntries(i) {
			st := 0
			if x.Done {
				st = 2
			}
			t.Subtasks = append(t.Subtasks, kanboardSubtask{Title: x.Title, Status: st})
		}
		for _, c := range i.Comments {
			t.Comments = append(t.Comments, kanboardComment{Comment: c.Text, Username: c.Author, DateCreation: c.Date})
		}
		out.Tasks = append(out.Tasks, t)
	}
	return out
}

// --- OpenProject

type opTitle struct {
	Title string `json:"title,omitempty"`
}

type opHref struct {
	Href string `json:"href"`
}

// opStatus is always written, its title "" too.
type opStatus struct {
	Title string `json:"title"`
}

type opLinks struct {
	Status      opStatus `json:"status"`
	Assignee    *opTitle `json:"assignee,omitempty"`
	Responsible *opTitle `json:"responsible,omitempty"`
	Author      *opTitle `json:"author,omitempty"`
	Parent      *opHref  `json:"parent,omitempty"`
}

type opRaw struct {
	Raw string `json:"raw"`
}

type opActivity struct {
	Type      string `json:"_type"`
	Comment   opRaw  `json:"comment"`
	CreatedAt string `json:"createdAt,omitempty"`
	Links     struct {
		User opTitle `json:"user"`
	} `json:"_links"`
}

type opActivities struct {
	Activities []opActivity `json:"activities"`
}

type opElement struct {
	ID          int           `json:"id"`
	Subject     string        `json:"subject"`
	Description opRaw         `json:"description"`
	DueDate     string        `json:"dueDate,omitempty"`
	StartDate   string        `json:"startDate,omitempty"`
	CreatedAt   string        `json:"createdAt,omitempty"`
	Links       opLinks       `json:"_links"`
	Embedded    *opActivities `json:"_embedded,omitempty"`
}

func formatOpenProject(d extBoard) any {
	type opCollection struct {
		Embedded struct {
			Elements []opElement `json:"elements"`
		} `json:"_embedded"`
	}
	ids := map[string]int{}
	for k, i := range d.Items {
		ids[i.CardID] = k + 1 // a Map's set: the last card of an id wins
	}
	var out opCollection
	out.Embedded.Elements = []opElement{}
	for _, i := range d.Items {
		e := opElement{ID: ids[i.CardID], Subject: i.Title, Description: opRaw{Raw: i.Description}, DueDate: i.DueAt,
			StartDate: i.StartAt, CreatedAt: i.CreatedAt, Links: opLinks{Status: opStatus{Title: i.ListTitle}}}
		if i.Owner != "" {
			e.Links.Assignee = &opTitle{Title: i.Owner}
		}
		if len(i.Assignees) > 0 {
			e.Links.Responsible = &opTitle{Title: i.Assignees[0]}
		}
		if i.Creator != "" {
			e.Links.Author = &opTitle{Title: i.Creator}
		}
		if n, ok := ids[i.ParentCardID]; ok && i.ParentCardID != "" {
			e.Links.Parent = &opHref{Href: "/api/v3/work_packages/" + strconv.Itoa(n)}
		}
		if len(i.Comments) > 0 {
			acts := &opActivities{}
			for _, c := range i.Comments {
				a := opActivity{Type: "Activity::Comment", Comment: opRaw{Raw: c.Text}, CreatedAt: c.Date}
				a.Links.User = opTitle{Title: c.Author}
				acts.Activities = append(acts.Activities, a)
			}
			e.Embedded = acts
		}
		out.Embedded.Elements = append(out.Embedded.Elements, e)
	}
	return out
}

// --- GitHub, Gitea, Forgejo

type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	State   string    `json:"state"`
	Labels  []ghLabel `json:"labels"`
	DueDate string    `json:"due_date,omitempty"`
}

func formatGithubLike(d extBoard) any {
	out := []ghIssue{}
	for _, i := range d.Items {
		g := ghIssue{Title: i.Title, Body: i.Description, State: map[bool]string{true: "closed", false: "open"}[isClosedList(i.ListTitle)],
			Labels: []ghLabel{}, DueDate: i.DueAt}
		for _, n := range i.Labels {
			g.Labels = append(g.Labels, ghLabel{Name: n})
		}
		out = append(out, g)
	}
	return out
}

// --- GitLab

type glUser struct {
	Username string `json:"username,omitempty"`
}

type glNote struct {
	Body      string `json:"body"`
	Author    glUser `json:"author"`
	CreatedAt string `json:"created_at,omitempty"`
	System    bool   `json:"system"`
}

type glIssue struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
	DueDate     string   `json:"due_date,omitempty"`
	Assignees   []glUser `json:"assignees,omitempty"`
	Author      *glUser  `json:"author,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	ClosedAt    string   `json:"closed_at,omitempty"`
	Notes       []glNote `json:"notes,omitempty"`
}

func formatGitlab(d extBoard) any {
	out := []glIssue{}
	for _, i := range d.Items {
		closed := isClosedList(i.ListTitle)
		g := glIssue{Title: i.Title, Description: i.Description, State: map[bool]string{true: "closed", false: "opened"}[closed],
			Labels: append([]string{}, i.Labels...), DueDate: i.DueAt, CreatedAt: i.CreatedAt}
		if i.Owner != "" {
			// Not filtered, as WeKan's: every assignee after the owner.
			for _, p := range append([]string{i.Owner}, i.Assignees...) {
				g.Assignees = append(g.Assignees, glUser{Username: p})
			}
		}
		if i.Creator != "" {
			g.Author = &glUser{Username: i.Creator}
		}
		if closed {
			g.ClosedAt = i.EndAt
		}
		for _, c := range i.Comments {
			g.Notes = append(g.Notes, glNote{Body: c.Text, Author: glUser{Username: c.Author}, CreatedAt: c.Date})
		}
		out = append(out, g)
	}
	return out
}

// --- Trello

type trelloLabel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type trelloList struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Closed bool   `json:"closed"`
}

type trelloCard struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Desc         string   `json:"desc"`
	IDList       string   `json:"idList"`
	Due          *string  `json:"due"`
	Closed       bool     `json:"closed"`
	IDLabels     []string `json:"idLabels"`
	IDMembers    []string `json:"idMembers"`
	IDChecklists []string `json:"idChecklists"`
}

func formatTrello(d extBoard) any {
	type trelloPrefs struct {
		Background      string `json:"background"`
		PermissionLevel string `json:"permissionLevel"`
	}
	type trelloBoard struct {
		Name       string        `json:"name"`
		Prefs      trelloPrefs   `json:"prefs"`
		Labels     []trelloLabel `json:"labels"`
		Lists      []trelloList  `json:"lists"`
		Cards      []trelloCard  `json:"cards"`
		Checklists []string      `json:"checklists"`
		Actions    []string      `json:"actions"`
	}
	out := trelloBoard{Name: d.Board.Title, Prefs: trelloPrefs{"blue", "private"}, Labels: []trelloLabel{}, Lists: []trelloList{},
		Cards: []trelloCard{}, Checklists: []string{}, Actions: []string{}}
	for _, l := range d.Board.Labels {
		out.Labels = append(out.Labels, trelloLabel{ID: l.ID, Name: l.Name, Color: l.Color})
	}
	for _, l := range d.Lists {
		out.Lists = append(out.Lists, trelloList{ID: l.ID, Name: l.Title})
	}
	for _, i := range d.Items {
		c := trelloCard{ID: i.CardID, Name: i.Title, Desc: i.Description, IDList: i.ListID, IDLabels: append([]string{}, i.LabelIDs...),
			IDMembers: []string{}, IDChecklists: []string{}}
		if i.DueAt != "" {
			due := i.DueAt
			c.Due = &due
		}
		out.Cards = append(out.Cards, c)
	}
	return out
}

// --- Asana

type asanaName struct {
	Name string `json:"name,omitempty"`
}

type asanaGID struct {
	GID string `json:"gid"`
}

type asanaSubtask struct {
	Name      string `json:"name"`
	Completed bool   `json:"completed"`
}

type asanaStory struct {
	ResourceSubtype string    `json:"resource_subtype"`
	Text            string    `json:"text"`
	CreatedBy       asanaName `json:"created_by"`
	CreatedAt       string    `json:"created_at,omitempty"`
}

// asanaSectionName is always written, its name "" too.
type asanaSectionName struct {
	Name string `json:"name"`
}

type asanaSection struct {
	Section asanaSectionName `json:"section"`
}

type asanaTask struct {
	GID         string         `json:"gid"`
	Name        string         `json:"name"`
	Notes       string         `json:"notes"`
	Completed   bool           `json:"completed"`
	CompletedAt string         `json:"completed_at,omitempty"`
	DueOn       string         `json:"due_on,omitempty"`
	StartOn     string         `json:"start_on,omitempty"`
	CreatedAt   string         `json:"created_at,omitempty"`
	Assignee    *asanaName     `json:"assignee,omitempty"`
	Parent      *asanaGID      `json:"parent,omitempty"`
	Memberships []asanaSection `json:"memberships"`
	Tags        []ghLabel      `json:"tags"`
	Subtasks    []asanaSubtask `json:"subtasks,omitempty"`
	Stories     []asanaStory   `json:"stories,omitempty"`
}

func formatAsana(d extBoard) any {
	type asanaDoc struct {
		Data []asanaTask `json:"data"`
	}
	out := asanaDoc{Data: []asanaTask{}}
	for _, i := range d.Items {
		closed := isClosedList(i.ListTitle)
		a := asanaTask{GID: i.CardID, Name: i.Title, Notes: i.Description, Completed: closed, DueOn: i.DueAt, StartOn: i.StartAt,
			CreatedAt: i.CreatedAt, Memberships: []asanaSection{{Section: asanaSectionName{Name: i.ListTitle}}}, Tags: []ghLabel{}}
		if closed {
			a.CompletedAt = i.EndAt
		}
		if i.Owner != "" {
			a.Assignee = &asanaName{Name: i.Owner}
		}
		if i.ParentCardID != "" {
			a.Parent = &asanaGID{GID: i.ParentCardID}
		}
		for _, n := range i.Labels {
			a.Tags = append(a.Tags, ghLabel{Name: n})
		}
		for _, x := range checklistEntries(i) {
			a.Subtasks = append(a.Subtasks, asanaSubtask{Name: x.Title, Completed: x.Done})
		}
		for _, c := range i.Comments {
			a.Stories = append(a.Stories, asanaStory{ResourceSubtype: "comment_added", Text: c.Text, CreatedBy: asanaName{Name: c.Author}, CreatedAt: c.Date})
		}
		out.Data = append(out.Data, a)
	}
	return out
}
