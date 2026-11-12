package web

import (
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The minicard draws its parts in the Minicard pane's order (WeKan's
// orderedMinicardSections): the sections in minicardSettingsSectionOrder,
// none Card Settings hides on the minicard, each section's fields in
// minicardFieldOrder. minicardParts are the parts Weddy's minicard draws, in
// that order, each once: a field key, or the part a key stands for (the Scrum
// fields are one line, the dates another).

// minicardPartOf is the part a field of MINICARD_LAYOUT is drawn as.
var minicardPartOf = map[string]string{
	"scrumSprint": "scrum", "scrumPastSprints": "scrum", "scrumRelease": "scrum", "scrumAcceptanceCriteria": "scrum",
	"scrumBacklogRank": "scrum", "scrumIssueType": "issueType",
	"receivedDate": "dates", "startDate": "dates", "dueDate": "dates", "endDate": "dates",
	"location": "location", "assignee": "assignee", "members": "members", "requestedBy": "requestedBy",
	"checklists": "checklists", "comments": "comments", "activities": "activities", "descriptionText": "descriptionBadge",
}

// minicardPart is one part of the minicard, by its key.
type minicardPart struct{ Key string }

func minicardParts(board bson.M) []minicardPart {
	flat := sideFieldOrder(board, "minicard")
	hidden, _ := plain(board[cardSettingsHiddenField["minicard"]]).([]any)
	var out []minicardPart
	for _, id := range sectionOrderOf(board[cardSettingsOrderField["minicard"]], "minicard") {
		if slices.Contains(hidden, any(id)) {
			continue
		}
		sec := minicardLayout.sectionByKey(id)
		if sec == nil {
			continue
		}
		for _, f := range flat {
			if !slices.Contains(sec.Fields, f) {
				continue
			}
			if p := minicardPartOf[f]; p != "" && !slices.Contains(out, minicardPart{p}) {
				out = append(out, minicardPart{p})
			}
		}
	}
	return out
}

func (l *fieldLayout) sectionByKey(key string) *cardSection {
	for i := range l.Sections {
		if l.Sections[i].Key == key {
			return &l.Sections[i]
		}
	}
	return nil
}

// minicardDate is one date on the minicard's dates line.
type minicardDate struct{ Kind, Icon, Title, Text string }

// minicardDatesOf are the card's dates the board shows on minicards, in the
// minicard's field order.
func minicardDatesOf(board bson.M, c *Card, dateFormat string) []minicardDate {
	if dateFormat == "" {
		dateFormat = "YYYY-MM-DD"
	}
	at := map[string]*time.Time{"receivedDate": c.ReceivedAt, "startDate": c.StartAt, "dueDate": c.DueAt, "endDate": c.EndAt}
	kinds := map[string]cardDateKind{}
	for _, k := range cardDateKinds {
		kinds[dateFieldKeys[k.Kind]] = k
	}
	var out []minicardDate
	for _, f := range sideFieldOrder(board, "minicard") {
		k, ok := kinds[f]
		if !ok || at[f] == nil || at[f].IsZero() {
			continue
		}
		row := cardSettingsRowOf(f)
		if row == nil || row.Minicard == nil || !boardFlag(board, row.Minicard.Field, row.Minicard.Default) {
			continue
		}
		out = append(out, minicardDate{Kind: k.Kind, Icon: k.Icon, Title: k.Label,
			Text: formatDateByUserPreference(*at[f], dateFormat, true, "")})
	}
	return out
}
