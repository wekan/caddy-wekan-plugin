package web

import (
	"encoding/json"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The Rules workflow view's palette, as Node.js WeKan's
// models/lib/rulesWorkflowPalette.js: rules that need no parameter, as chips
// to drag into the builder - a trigger into When, an action into Then - or an
// action onto an existing rule to change what it does.

type paletteEntry struct {
	key    string
	params map[string]string
	doc    bson.M
}

var triggerPalette = []paletteEntry{
	{"r-w-card-created", nil, bson.M{"activityType": "createCard", "listName": "*", "swimlaneName": "*", "cardTitle": "*", "userId": "*"}},
	{"r-when-a-card-is-moved", nil, bson.M{"activityType": "moveCard", "listName": "*", "oldListName": "*", "swimlaneName": "*", "cardTitle": "*", "userId": "*"}},
	{"r-w-card-archived", nil, bson.M{"activityType": "archivedCard", "userId": "*"}},
	{"r-w-card-unarchived", nil, bson.M{"activityType": "restoredCard", "userId": "*"}},
	{"r-w-label-added", nil, bson.M{"activityType": "addedLabel", "labelId": "*", "userId": "*"}},
	{"r-w-label-removed", nil, bson.M{"activityType": "removedLabel", "labelId": "*", "userId": "*"}},
	{"r-w-member-added", nil, bson.M{"activityType": "joinMember", "username": "*", "userId": "*"}},
	{"r-w-member-removed", nil, bson.M{"activityType": "unjoinMember", "username": "*", "userId": "*"}},
	{"r-w-assignee-added", nil, bson.M{"activityType": "joinAssignee", "username": "*", "userId": "*"}},
	{"r-w-assignee-removed", nil, bson.M{"activityType": "unjoinAssignee", "username": "*", "userId": "*"}},
	{"r-w-checklist-added", nil, bson.M{"activityType": "addChecklist", "checklistName": "*", "userId": "*"}},
	{"r-w-attachment-added", nil, bson.M{"activityType": "addAttachment", "userId": "*"}},
	{"r-w-every-day-at", map[string]string{"time": "09:00"}, bson.M{"activityType": "scheduledTrigger", "scheduleKind": "calendar", "scheduleType": "daily", "atTime": "09:00", "listName": "*", "swimlaneName": "*"}},
}

var actionPalette = []paletteEntry{
	{"r-d-move-to-top-gen", nil, bson.M{"actionType": "moveCardToTop", "listName": "*", "swimlaneName": "*"}},
	{"r-d-move-to-bottom-gen", nil, bson.M{"actionType": "moveCardToBottom", "listName": "*", "swimlaneName": "*"}},
	{"r-d-archive", nil, bson.M{"actionType": "archive"}},
	{"r-d-unarchive", nil, bson.M{"actionType": "unarchive"}},
	{"r-mark-complete", nil, bson.M{"actionType": "markCardComplete"}},
	{"r-mark-incomplete", nil, bson.M{"actionType": "markCardIncomplete"}},
	{"r-remove-all", nil, bson.M{"actionType": "removeMember", "username": "*"}},
	{"r-remove-all-assignees", nil, bson.M{"actionType": "removeAssignee", "username": "*"}},
	{"r-w-set-received-now", nil, bson.M{"actionType": "setDate", "dateField": "receivedAt"}},
}

// paletteChip is a palette entry as the page shows it: its label in the
// language, and the rule part it makes, as JSON.
type paletteChip struct {
	Idx        int
	Label, Doc string
}

func paletteChips(lang string, entries []paletteEntry) []paletteChip {
	out := make([]paletteChip, 0, len(entries))
	for i, e := range entries {
		label := fillParams(tr(lang, e.key), e.params)
		doc := bson.M{"desc": label}
		for k, v := range e.doc {
			doc[k] = v
		}
		raw, _ := json.Marshal(doc)
		out = append(out, paletteChip{Idx: i, Label: label, Doc: string(raw)})
	}
	return out
}

// The keys, named through msg so that sync-i18n copies them.
var _ = []string{msg("r-w-card-created"), msg("r-when-a-card-is-moved"), msg("r-w-card-archived"), msg("r-w-card-unarchived"),
	msg("r-w-label-added"), msg("r-w-label-removed"), msg("r-w-member-added"), msg("r-w-member-removed"), msg("r-w-assignee-added"),
	msg("r-w-assignee-removed"), msg("r-w-checklist-added"), msg("r-w-attachment-added"), msg("r-w-every-day-at"),
	msg("r-d-move-to-top-gen"), msg("r-d-move-to-bottom-gen"), msg("r-d-archive"), msg("r-d-unarchive"), msg("r-mark-complete"),
	msg("r-mark-incomplete"), msg("r-remove-all"), msg("r-remove-all-assignees"), msg("r-w-set-received-now")}
