package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's Board Settings / Card (client/components/sidebar/sidebar.jade,
// boardCardSettingsPopup; docs/Features/Cards/Card-Settings.md): both sides at
// once, the Minicard pane and the Card pane, each the board's sections as
// checklists - a section's checkbox whether the whole section is shown on that
// side, its rows each a checkbox of the board's setting for that side.
//
// The table is WeKan's own (models/lib/cardSettingsRows.js and
// cardSettingsSections.js), written to card_settings.json by
// cmd/card-settings/generate.cjs with the board field each checkbox writes.
// A row's checkbox is a board admin's: WeKan's client writes it with
// Boards.update, which its allow rule lets through for a board admin, so here
// it goes through /boards/update, which lets through exactly these fields
// (cardSettingsBoardWrite). The Scrum rows are scrum.configure's visibility
// flags, and the labels-text rows marked personal the user's own override.

//go:embed card_settings.json
var cardSettingsJSON []byte

type cardSettingsSide struct {
	Kind      string `json:"kind"` // board, scrum or personal
	Field     string `json:"field"`
	Scrum     string `json:"scrum"`
	After     string `json:"after"`
	Default   bool   `json:"default"`
	NeedsCard bool   `json:"needsCard"`
}

type cardSettingsRow struct {
	Key            string            `json:"key"`
	Icons          []string          `json:"icons"`
	Label          []string          `json:"label"`
	LabelSeparator string            `json:"labelSeparator"`
	Card           *cardSettingsSide `json:"card"`
	Minicard       *cardSettingsSide `json:"minicard"`
}

func (r *cardSettingsRow) side(side string) *cardSettingsSide {
	if side == "minicard" {
		return r.Minicard
	}
	return r.Card
}

type cardSettingsSection struct {
	ID       string   `json:"id"`
	Icon     string   `json:"icon"`
	LabelKey string   `json:"labelKey"`
	General  bool     `json:"general"`
	Keys     []string `json:"keys"`
}

var cardSettings struct {
	Rows     []cardSettingsRow     `json:"rows"`
	Sections []cardSettingsSection `json:"sections"`
	Order    map[string][]string   `json:"order"`
	I18nKeys []string              `json:"i18nKeys"`
}

// cardSettingsBoardFields are the board fields the rows' checkboxes write.
var cardSettingsBoardFields = map[string]bool{}

// cardSettingsHiddenField is where a side keeps its hidden sections
// (cardSettingsSections.js HIDDEN_SECTIONS_FIELD).
var cardSettingsHiddenField = map[string]string{"card": "cardSettingsHiddenOnCard", "minicard": "cardSettingsHiddenOnMinicard"}

func init() {
	if err := json.Unmarshal(cardSettingsJSON, &cardSettings); err != nil {
		panic("card_settings.json: " + err.Error())
	}
	for i := range cardSettings.Rows {
		for _, s := range []*cardSettingsSide{cardSettings.Rows[i].Card, cardSettings.Rows[i].Minicard} {
			if s != nil && s.Kind == "board" {
				cardSettingsBoardFields[s.Field] = true
			}
		}
	}
}

func cardSettingsRowOf(key string) *cardSettingsRow {
	for i := range cardSettings.Rows {
		if cardSettings.Rows[i].Key == key {
			return &cardSettings.Rows[i]
		}
	}
	return nil
}

func cardSettingsSectionIDs() []string {
	ids := make([]string, len(cardSettings.Sections))
	for i, s := range cardSettings.Sections {
		ids[i] = s.ID
	}
	return ids
}

// cardSettingsBoardWrite is a /boards/update $set that is Card Settings'
// own, as it is to be written: every path a row's boolean board field, a
// side's hidden sections as known section ids, a side's section order
// (normalised as WeKan's setCardSettingsSectionOrder) or the card's field
// order (as setCardFieldOrder). Anything else is not.
func cardSettingsBoardWrite(set map[string]any) (bson.M, bool) {
	known := cardSettingsSectionIDs()
	out := bson.M{}
	stringList := func(v any) ([]string, bool) {
		list, ok := v.([]any)
		if !ok || len(list) > 200 {
			return nil, false
		}
		var ss []string
		for _, x := range list {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			ss = append(ss, s)
		}
		return ss, true
	}
	for path, v := range set {
		switch {
		case cardSettingsBoardFields[path]:
			if _, ok := v.(bool); !ok {
				return nil, false
			}
			out[path] = v
		case path == cardSettingsHiddenField["card"] || path == cardSettingsHiddenField["minicard"]:
			ids, ok := stringList(v)
			if !ok {
				return nil, false
			}
			for _, id := range ids {
				if !slices.Contains(known, id) {
					return nil, false
				}
			}
			out[path] = ids
		case path == cardSettingsOrderField["card"] || path == cardSettingsOrderField["minicard"]:
			ids, ok := stringList(v)
			if !ok {
				return nil, false
			}
			side := "card"
			if path == cardSettingsOrderField["minicard"] {
				side = "minicard"
			}
			list := make([]any, len(ids))
			for i, id := range ids {
				list[i] = id
			}
			out[path] = sectionOrderOf(list, side)
		case path == "cardFieldOrder" || path == "minicardFieldOrder":
			keys, ok := stringList(v)
			if !ok {
				return nil, false
			}
			side := "card"
			if path == "minicardFieldOrder" {
				side = "minicard"
			}
			out[path] = sideLayout(side).apply(keys)
		default:
			return nil, false
		}
	}
	return out, len(out) > 0
}

// labelTextShown is WeKan's resolveShowLabelTextValue: the user's override,
// else the board's setting, else shown.
func labelTextShown(override, board any) bool {
	if v, ok := override.(bool); ok {
		return v
	}
	if v, ok := board.(bool); ok {
		return v
	}
	return true
}

type cardSettingsLine struct {
	Key, Side, Kind, Field, Scrum, Title string
	Icons                                []string
	Checked, Writable                    bool
	// Positioned: the row has a place of its own in its side's order, so it
	// is dragged (by Handles, the user's drag handles, or by its name).
	Positioned, Handles bool
}

type cardSettingsPaneSection struct {
	ID, Icon, Title string
	Shown, Writable bool
	Lines           []cardSettingsLine
	CheckedCount    int
}

type cardSettingsPane struct {
	Side, Title string
	// ScrumRevision is the board's, for the Scrum rows' scrum.configure.
	ScrumRevision int
	Sections      []cardSettingsPaneSection
}

// cardSettingsPanes are the two panes for the board, as u sees them: the
// Minicard pane first, then the Card pane, as WeKan draws them.
func (s *Server) cardSettingsPanes(ctx context.Context, b *Board, u *User, lang string) []cardSettingsPane {
	var board bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&board)
	var visibility map[string]any
	if sc, ok := plain(board["scrum"]).(map[string]any); ok {
		visibility, _ = sc["visibility"].(map[string]any)
	}
	var prof bson.M
	if u != nil {
		var doc struct {
			Profile bson.M `bson:"profile"`
		}
		_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(u.ID)}).Decode(&doc)
		prof = doc.Profile
	}
	admin := u != nil && (u.IsAdmin || b.AdminOf(u.ID))
	var panes []cardSettingsPane
	titles := map[string]string{"minicard": tr(lang, "show-on-minicard"), "card": tr(lang, "show-on-card")}
	for _, side := range []string{"minicard", "card"} {
		pane := cardSettingsPane{Side: side, Title: titles[side], ScrumRevision: int(revisionOf(board["scrumRevision"]))}
		hidden, _ := plain(board[cardSettingsHiddenField[side]]).([]any)
		rows := rowsForSide(side, sideFieldOrder(board, side))
		for _, id := range sectionOrderOf(board[cardSettingsOrderField[side]], side) {
			var sec *cardSettingsSection
			for i := range cardSettings.Sections {
				if cardSettings.Sections[i].ID == id {
					sec = &cardSettings.Sections[i]
				}
			}
			if sec == nil {
				continue
			}
			ps := cardSettingsPaneSection{ID: sec.ID, Icon: sec.Icon, Title: tr(lang, sec.LabelKey),
				Shown: !slices.Contains(hidden, any(sec.ID)), Writable: admin}
			for _, row := range rows {
				if !slices.Contains(sec.Keys, row.Key) {
					continue
				}
				sp := row.side(side)
				if sp == nil || sp.NeedsCard {
					continue
				}
				parts := make([]string, len(row.Label))
				for i, k := range row.Label {
					parts[i] = tr(lang, k)
				}
				line := cardSettingsLine{Key: row.Key, Side: side, Kind: sp.Kind, Field: sp.Field, Scrum: sp.Scrum,
					Title: strings.Join(parts, row.LabelSeparator), Icons: row.Icons, Writable: admin,
					Positioned: sp.After == "" && sideLayout(side).sectionOf(row.Key) != nil,
					Handles:    u != nil && u.Profile.ShowDesktopDragHandles}
				switch sp.Kind {
				case "board":
					line.Checked = sp.Default
					if v, ok := board[sp.Field].(bool); ok {
						line.Checked = v
					}
				case "scrum":
					line.Checked = visibility[sp.Scrum] == true
				case "personal":
					line.Writable = u != nil
					if side == "minicard" {
						line.Checked = labelTextShown(prof["showLabelTextOverride"], board["showLabelText"])
					} else {
						line.Checked = labelTextShown(prof["cardLabelTextOverride"], board["allowsLabelTextOnCard"])
					}
				}
				if line.Checked {
					ps.CheckedCount++
				}
				ps.Lines = append(ps.Lines, line)
			}
			// A section with no row on this side is left out; Card, holding
			// what is not a field, stays.
			if len(ps.Lines) == 0 && sec.ID != "card" {
				continue
			}
			pane.Sections = append(pane.Sections, ps)
		}
		panes = append(panes, pane)
	}
	return panes
}

// methodSetShowLabelTextOverride is WeKan's setShowLabelTextOverride(value):
// the user's own "labels text on minicards", true, false, or null to follow
// the board.
func (s *Server) methodSetShowLabelTextOverride(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	update := bson.M{"$unset": bson.M{"profile.showLabelTextOverride": ""}}
	if len(args) > 0 && string(args[0]) != "null" {
		var v bool
		if json.Unmarshal(args[0], &v) != nil {
			return nil, &methodError{"Match.Error", "Expected boolean or null"}
		}
		update = bson.M{"$set": bson.M{"profile.showLabelTextOverride": v}}
	}
	if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, update); err != nil {
		return nil, err
	}
	return nil, nil
}

// issueTypeBadge is WeKan's (models/lib/issueTypeIcon.js): the work item
// type's name, with an icon and a tone for the kinds it knows.
type issueBadge struct{ Name, Icon, Tone string }

var issueTypeIcons = []struct {
	names      []string
	icon, tone string
}{
	{[]string{"bug", "defect", "incident", "problem"}, "fa-bug", "red"},
	{[]string{"story", "userstory", "requirement"}, "fa-bookmark", "green"},
	{[]string{"task"}, "fa-check-square-o", "blue"},
	{[]string{"subtask", "technicaltask"}, "fa-level-down", "blue"},
	{[]string{"epic", "initiative", "theme"}, "fa-bolt", "purple"},
	{[]string{"improvement", "enhancement", "changerequest"}, "fa-arrow-up", "green"},
	{[]string{"newfeature", "feature"}, "fa-plus-square", "green"},
	{[]string{"spike", "research", "investigation"}, "fa-search", "orange"},
	{[]string{"test", "testcase", "qa"}, "fa-flask", "orange"},
}

func issueTypeBadge(v any) *issueBadge {
	name, ok := v.(string)
	if !ok {
		return nil
	}
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}
	if name == "" {
		return nil
	}
	k := strings.NewReplacer(" ", "", "\t", "", "_", "", "-", "").Replace(strings.ToLower(name))
	for _, e := range issueTypeIcons {
		if slices.Contains(e.names, k) {
			return &issueBadge{name, e.icon, e.tone}
		}
	}
	return &issueBadge{name, "fa-circle-o", "grey"}
}
