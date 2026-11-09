package web

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The card details' votes, planning poker and Scrum fields, as Node.js
// WeKan's server/lib/ruleCardVoting.js and ruleCardScrum.js: a vote's
// question, end and counts - its voters by name only when the vote is public
// - and poker's choice counts and voters only once the poker has ended, each
// only when the board allows votes or poker; the Scrum fields the board
// shows, a sprint or release by its name on this board. What decides what is
// shown is read again afterwards, and a change sends no mail.

var pokerChoices = [][2]string{{"one", "1"}, {"two", "2"}, {"three", "3"}, {"five", "5"}, {"eight", "8"},
	{"thirteen", "13"}, {"twenty", "20"}, {"forty", "40"}, {"oneHundred", "100"}, {"unsure", "?"}}

var scrumDetailFields = [][3]string{{"Sprint", "sprintId", "Scrum sprint"}, {"PastSprints", "pastSprintIds", "Past Scrum sprints"},
	{"Release", "releaseId", "Scrum release"}, {"IssueType", "issueType", "Scrum issue type"},
	{"AcceptanceCriteria", "acceptanceCriteria", "Scrum acceptance criteria"}, {"BacklogRank", "backlogRank", "Scrum backlog rank"}}

var errRuleMailVotingChanged = errors.New("rule-email-details-changed")

// ruleMailVotingPolicy is WeKan's votingVisibility: what decides whether the
// mail may name voters or show poker results.
func ruleMailVotingPolicy(card, board map[string]any) string {
	vote, _ := card["vote"].(map[string]any)
	poker, _ := card["poker"].(map[string]any)
	end := ""
	if t, ok := poker["end"].(time.Time); ok && !t.IsZero() {
		end = t.UTC().Format(time.RFC3339Nano)
	}
	return canonical([]any{board["allowsVote"] != false, board["allowsPoker"] != false, vote["public"] == true, end})
}

// ruleMailScrumPolicy is WeKan's scrumVisibility.
func ruleMailScrumPolicy(board map[string]any) []bool {
	scrum, _ := board["scrum"].(map[string]any)
	vis, _ := scrum["visibility"].(map[string]any)
	out := make([]bool, len(scrumDetailFields))
	for i, f := range scrumDetailFields {
		out[i] = vis["card"+f[0]] == true
	}
	return out
}

func (s *Server) ruleMailRawBoard(ctx context.Context, boardID string) map[string]any {
	var raw bson.M
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&raw) != nil {
		return nil
	}
	m, _ := plain(raw).(map[string]any)
	return m
}

// ruleMailVoting adds the votes and poker of the card (appendRuleCardVoting).
func (s *Server) ruleMailVoting(ctx context.Context, card, board map[string]any, add func(string, any)) {
	members := func(v any) []string {
		var out []string
		seen := map[string]bool{}
		list, _ := v.([]any)
		for _, e := range list {
			if id, ok := e.(string); ok && id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		return out
	}
	names := func(ids []string) []any {
		var out []any
		for _, id := range ids {
			out = append(out, s.ruleMailDisplayName(ctx, id))
		}
		return out
	}
	count := func(v any) int {
		n := 0
		list, _ := v.([]any)
		for _, e := range list {
			if id, ok := e.(string); ok && id != "" {
				n++
			}
		}
		return n
	}
	if vote, _ := card["vote"].(map[string]any); board["allowsVote"] != false && vote != nil {
		if q, _ := vote["question"].(string); q != "" {
			add("Vote question", q)
			add("Vote end", vote["end"])
			for _, f := range [][2]string{{"positive", "For"}, {"negative", "Against"}} {
				add("Votes "+map[string]string{"For": "for", "Against": "against"}[f[1]], float64(count(vote[f[0]])))
				if vote["public"] == true {
					add(f[1]+" voters", names(members(vote[f[0]])))
				}
			}
		}
	}
	if poker, _ := card["poker"].(map[string]any); board["allowsPoker"] != false && poker != nil {
		add("Poker enabled", poker["question"])
		add("Poker end", poker["end"])
		add("Poker estimation", poker["estimation"])
		end, ended := poker["end"].(time.Time)
		if q, _ := poker["question"].(bool); q && ended && !end.IsZero() && end.Before(time.Now()) {
			for _, c := range pokerChoices {
				ids := members(poker[c[0]])
				add("Poker "+c[1]+" votes", float64(count(poker[c[0]])))
				add("Poker "+c[1]+" voters", names(ids))
			}
		}
	}
}

// ruleMailScrum adds the Scrum fields the board shows (appendRuleCardScrum)
// and answers the check of the sprints and releases it named.
func (s *Server) ruleMailScrum(ctx context.Context, card, board map[string]any, boardID string, add func(string, any)) func() error {
	scrum, _ := card["scrum"].(map[string]any)
	type ref struct{ coll, id, name string }
	var refs []ref
	read := func(coll, id string) (string, bool) {
		var r struct {
			ID        string `bson:"_id"`
			BoardID   string `bson:"boardId"`
			Name      any    `bson:"name"`
			DeletedAt any    `bson:"deletedAt"`
		}
		if !validID(id) || s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id), "boardId": eq(boardID)}).Decode(&r) != nil ||
			r.ID != id || r.BoardID != boardID || r.DeletedAt != nil {
			return "", false
		}
		name, ok := r.Name.(string)
		return name, ok
	}
	for i, vis := range ruleMailScrumPolicy(board) {
		if !vis {
			continue
		}
		f := scrumDetailFields[i]
		value := scrum[f[1]]
		var ids []string
		coll := "scrumSprints"
		switch f[1] {
		case "sprintId":
			if id, ok := value.(string); ok {
				ids = []string{id}
			}
		case "pastSprintIds":
			list, _ := value.([]any)
			for _, e := range list {
				if id, ok := e.(string); ok {
					ids = append(ids, id)
				}
			}
		case "releaseId":
			// A card's releases are a list, its legacy single releaseId included.
			coll = "scrumReleases"
			if id, ok := value.(string); ok && id != "" {
				ids = append(ids, id)
			}
			list, _ := scrum["releaseIds"].([]any)
			for _, e := range list {
				if id, ok := e.(string); ok && id != "" {
					ids = append(ids, id)
				}
			}
		default:
			add(f[2], value)
			continue
		}
		var got []any
		seen := map[string]bool{}
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			if name, ok := read(coll, id); ok {
				got = append(got, name)
				refs = append(refs, ref{coll, id, name})
			}
		}
		add(f[2], got)
	}
	return func() error {
		for _, r := range refs {
			if name, ok := read(r.coll, r.id); !ok || name != r.name {
				return errors.New("rule-email-scrum-changed")
			}
		}
		return nil
	}
}
