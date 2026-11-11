package web

// The Scrum fields of a card and a minicard, as WeKan's
// client/components/boards/scrum/scrumFields.js: each field Card Settings
// shows on that side (the board's scrum.visibility, cardX and minicardX) as
// "label: value", names for the sprint, past sprints and releases; in the
// card details, for whoever may change the card, a form that edits them -
// several releases in a multiple select - through scrum.updateCard.

import (
	"context"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// scrumFieldDefs are WeKan's definitions: suffix, field, label.
var scrumFieldDefs = map[string][][3]string{
	"card": {{"Sprint", "sprintId", msg("scrum-sprint")}, {"PastSprints", "pastSprintIds", msg("scrum-past-sprints")}, {"Release", "releaseIds", msg("scrum-release")},
		{"IssueType", "issueType", msg("scrum-issue-type")}, {"AcceptanceCriteria", "acceptanceCriteria", msg("scrum-acceptance-criteria")}, {"BacklogRank", "backlogRank", msg("scrum-backlog-rank")}},
	"minicard": {{"Sprint", "sprintId", msg("scrum-sprint")}, {"PastSprints", "pastSprintIds", msg("scrum-past-sprints")}, {"Release", "releaseIds", msg("scrum-release")},
		{"AcceptanceCriteria", "acceptanceCriteria", msg("scrum-acceptance-criteria")}, {"BacklogRank", "backlogRank", msg("scrum-backlog-rank")}},
}

type scrumFieldRow struct {
	Field, Label, Value string
	Options             []scrumOption
	Multiple, Number    bool
}

// scrumNames are a board's sprints and releases, for the fields.
type scrumNames struct {
	visibility        map[string]any
	sprints, releases []map[string]any
}

func (s *Server) scrumNamesOf(ctx context.Context, boardID string) *scrumNames {
	var board bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}, options.FindOne().SetProjection(bson.M{"scrum": 1})).Decode(&board)
	sc, _ := plain(board["scrum"]).(map[string]any)
	vis, _ := sc["visibility"].(map[string]any)
	if len(vis) == 0 {
		return &scrumNames{}
	}
	f := options.Find().SetProjection(bson.M{"_id": 1, "name": 1, "state": 1})
	return &scrumNames{vis, s.findDocs(ctx, "scrumSprints", bson.M{"boardId": eq(boardID)}, f), s.findDocs(ctx, "scrumReleases", bson.M{"boardId": eq(boardID)}, f)}
}

func nameOf(list []map[string]any, id string) string {
	for _, r := range list {
		if docString(r, "_id") == id {
			return docString(r, "name")
		}
	}
	return ""
}

// rows is fieldRows(context, editing): the side's shown fields of a card's
// Scrum metadata.
func (n *scrumNames) rows(lang, kind string, sc map[string]any, editing bool) []scrumFieldRow {
	if n == nil || len(n.visibility) == 0 {
		return nil
	}
	out := []scrumFieldRow{}
	for _, d := range scrumFieldDefs[kind] {
		if n.visibility[kind+d[0]] != true || editing && d[1] == "pastSprintIds" {
			continue
		}
		row := scrumFieldRow{Field: d[1], Label: tr(lang, d[2]), Number: d[1] == "backlogRank"}
		switch d[1] {
		case "releaseIds":
			ids := cardReleaseIds(sc)
			var names []string
			for _, id := range ids {
				if v := nameOf(n.releases, id); v != "" {
					names = append(names, v)
				}
			}
			row.Value, row.Multiple = strings.Join(names, ", "), true
			for _, r := range n.releases {
				row.Options = append(row.Options, scrumOption{docString(r, "_id"), docString(r, "name"), containsString(ids, docString(r, "_id"))})
			}
		case "sprintId":
			cur := docString(sc, "sprintId")
			row.Value = nameOf(n.sprints, cur)
			row.Options = []scrumOption{{"", "—", cur == ""}}
			for _, r := range n.sprints {
				if st := docString(r, "state"); st == "planned" || st == "active" || docString(r, "_id") == cur {
					row.Options = append(row.Options, scrumOption{docString(r, "_id"), docString(r, "name"), docString(r, "_id") == cur})
				}
			}
		case "pastSprintIds":
			var names []string
			for _, id := range stringsOf(sc["pastSprintIds"]) {
				if v := nameOf(n.sprints, id); v != "" {
					names = append(names, v)
				}
			}
			row.Value = strings.Join(names, ", ")
		case "backlogRank":
			if sc["backlogRank"] != nil {
				row.Value = scrumNum(sc["backlogRank"])
			}
		default:
			row.Value = docString(sc, d[1])
		}
		out = append(out, row)
	}
	return out
}

// annotateScrumMinicards puts each minicard's shown Scrum fields on it.
func (s *Server) annotateScrumMinicards(ctx context.Context, b *Board, lang string, lanes []lane) {
	n := s.scrumNamesOf(ctx, b.ID)
	if len(n.visibility) == 0 {
		return
	}
	for i := range lanes {
		for j := range lanes[i].Lists {
			for k := range lanes[i].Lists[j].Cards {
				c := &lanes[i].Lists[j].Cards[k]
				c.MinicardScrum = n.rows(lang, "minicard", plainScrum(c.Scrum), false)
			}
		}
	}
}

func plainScrum(v any) map[string]any {
	m, _ := plain(v).(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
