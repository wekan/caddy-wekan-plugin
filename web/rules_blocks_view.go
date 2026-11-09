package web

import (
	"context"
	_ "embed"
	"encoding/json"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The Rules page's Blocks view, as Node.js WeKan's client/components/rules/
// rulesBlocks.js, blocks/editor.js and models/lib/rulesBlocks*.js: a rule as
// a Blockly "When" block holding its trigger, joined to a "Then" block
// holding its action, built from the same palette as the Workflow view and
// saved as an ordinary rule (rules.createRule, rules.updateRule) - Blockly is
// only the editor, the documents stay WeKan rules. For the board's admins.
//
// Blockly (static/vendor/blockly.min.js, Apache-2.0) is WeKan's version,
// 13.4.0; its messages are WeKan's translations of them, named by WeKan's
// blocklyMessageKeys.json (blockly_message_keys.json), which cmd/sync-i18n
// reads too, so every language has them.

//go:embed blockly_message_keys.json
var blocklyMessageKeys []byte

// The labels of a block's fields (rulesBlocksDefinition.js FIELD_KEYS).
var blockFieldKeys = map[string]string{"listName": msg("r-list-name"), "oldListName": msg("r-moved-from"),
	"swimlaneName": msg("r-swimlane-name"), "oldSwimlaneName": msg("r-moved-from"), "cardTitle": msg("title"),
	"checklistName": msg("r-checklist"), "itemName": msg("r-item"), "username": msg("username"), "atTime": msg("time"),
	"text": msg("text"), "comment": msg("comment"), "emailTo": msg("r-to"), "emailSubject": msg("r-subject"),
	"emailMsg": msg("r-d-send-email-message")}

// The view's own words.
var _ = []string{msg("r-blocks-view"), msg("r-blocks-help"), msg("r-rule"), msg("r-add-rule"), msg("r-new-rule-name"),
	msg("r-blocks-reload"), msg("r-blocks-unsaved"), msg("r-blocks-saved"), msg("r-blocks-permission"), msg("r-blocks-discard"),
	msg("r-blocks-unavailable"), msg("r-blocks-invalid"), msg("r-blocks-conflict"), msg("r-rule-title-required"),
	msg("r-trigger"), msg("r-action"), msg("r-when"), msg("r-edit-rule-trigger-action"), msg("save")}

type blockRule struct {
	ID, Title       string
	Trigger, Action any
}

type blockPaletteEntry struct {
	Label string `json:"label"`
	Doc   bson.M `json:"doc"`
}

// blocksViewData fills the Blocks view of a board for one of its admins: its
// rules with their trigger and action documents, the palette, and the words
// the editor shows, as JSON for static/weddy-blocks.js.
func (s *Server) blocksViewData(ctx context.Context, b *Board, lang string, rtl bool, data map[string]any) {
	rules := []blockRule{}
	if cur, err := s.db.Collection("rules").Find(ctx, bson.M{"boardId": b.ID}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}})); err == nil {
		var docs []bson.M
		_ = cur.All(ctx, &docs)
		for _, r := range docs {
			var t, a bson.M
			_ = s.db.Collection("triggers").FindOne(ctx, bson.M{"_id": eq(docString(r, "triggerId"))}).Decode(&t)
			_ = s.db.Collection("actions").FindOne(ctx, bson.M{"_id": eq(docString(r, "actionId"))}).Decode(&a)
			br := blockRule{ID: docString(r, "_id"), Title: docString(r, "title")}
			if t != nil {
				br.Trigger = plain(portableRule(t))
			}
			if a != nil {
				br.Action = plain(portableRule(a))
			}
			rules = append(rules, br)
		}
	}
	palette := func(entries []paletteEntry) []blockPaletteEntry {
		out := []blockPaletteEntry{}
		for _, e := range entries {
			doc := bson.M{}
			for k, v := range e.doc {
				doc[k] = v
			}
			out = append(out, blockPaletteEntry{Label: fillParams(tr(lang, e.key), e.params), Doc: doc})
		}
		return out
	}
	var mapping map[string]string
	_ = json.Unmarshal(blocklyMessageKeys, &mapping)
	messages := map[string]string{}
	for k, v := range mapping {
		messages[k] = tr(lang, v)
	}
	fields := map[string]string{}
	for k, v := range blockFieldKeys {
		fields[k] = tr(lang, v)
	}
	words := map[string]string{}
	for _, k := range []string{"r-when", "r-action", "r-trigger", "r-edit-rule-trigger-action", "r-blocks-discard", "r-blocks-unavailable",
		"r-blocks-invalid", "r-blocks-conflict", "r-rule-title-required", "r-blocks-saved", "r-blocks-unsaved"} {
		words[k] = tr(lang, k)
	}
	payload := map[string]any{"rules": rules, "triggers": palette(triggerPalette), "actions": palette(actionPalette),
		"messages": messages, "fields": fields, "words": words, "rtl": rtl}
	raw, _ := json.Marshal(payload)
	data["BlocksData"] = string(raw)
	data["BlockRules"] = rules
}
