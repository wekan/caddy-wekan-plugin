package web

// WeKan's models/lib/changeHistoryIntegrity.js: a changeHistory row's
// integrity hash, exactly as WeKan computes and verifies it, so a row Weddy
// writes passes WeKan's hourly audit and a row WeKan wrote passes Weddy's.
// Every row names its predecessor's hash (previousHash): one chain a board.

// jsUndefined is JavaScript's undefined: a protected field a row has not got
// is hashed as {"$undefined":true}, as WeKan's protectedHistory reads it.
type jsUndefined struct{}

// historyProtected are the fields the hash covers (PROTECTED).
var historyProtected = []string{
	"boardId", "swimlaneId", "listId", "cardId", "entityType", "entityId",
	"group", "changeType", "previousContent", "newContent", "userId", "createdAt",
	"batchId", "restoredFromId", "restoredByUserId", "previousHash",
}

// protectedHistory is the row's protected fields, a missing one undefined.
func protectedHistory(row map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range historyProtected {
		if v, ok := row[k]; ok {
			out[k] = v
		} else {
			out[k] = jsUndefined{}
		}
	}
	return out
}

// storedShape is what the database keeps of a content: an undefined value
// is dropped from an object and is null in an array.
func storedShape(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			if _, undef := e.(jsUndefined); undef {
				out[i] = nil
			} else {
				out[i] = storedShape(e)
			}
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if _, undef := e.(jsUndefined); !undef {
				out[k] = storedShape(e)
			}
		}
		return out
	}
	return v
}

// hashHistoryRow is WeKan's: the protected fields, the contents as stored.
func hashHistoryRow(row map[string]any) string {
	shaped := protectedHistory(plain(row).(map[string]any))
	shaped["previousContent"] = storedShape(shaped["previousContent"])
	shaped["newContent"] = storedShape(shaped["newContent"])
	return sha256Hex(canonical(shaped))
}

func plainObject(v any) bool { _, ok := v.(map[string]any); return ok }

// legacyUndefinedVariant: rows written before the content was hashed as
// stored - each key the other side has and this side lost, re-added as
// undefined. "" when there is nothing to re-add.
func legacyUndefinedVariant(row map[string]any) string {
	before, b1 := row["previousContent"].(map[string]any)
	after, b2 := row["newContent"].(map[string]any)
	if !b1 || !b2 {
		return ""
	}
	fill := func(own, other map[string]any) (map[string]any, bool) {
		var missing []string
		for k := range other {
			if _, ok := own[k]; !ok {
				missing = append(missing, k)
			}
		}
		if len(missing) == 0 {
			return own, false
		}
		out := map[string]any{}
		for k, v := range own {
			out[k] = v
		}
		for _, k := range missing {
			out[k] = jsUndefined{}
		}
		return out, true
	}
	fb, cb := fill(before, after)
	fa, ca := fill(after, before)
	if !cb && !ca {
		return ""
	}
	shaped := protectedHistory(row)
	shaped["previousContent"], shaped["newContent"] = fb, fa
	return sha256Hex(canonical(shaped))
}

// historyCleanedEmptyFields: the id and label fields collection2's clean
// stored as null where ” was hashed (#6797).
var historyCleanedEmptyFields = []string{"boardId", "swimlaneId", "listId", "cardId", "group", "batchId", "restoredFromId", "restoredByUserId"}

func cleanedEmptyStringMatches(row map[string]any) bool {
	var nulls []string
	for _, k := range historyCleanedEmptyFields {
		if v, ok := row[k]; !ok || v == nil {
			nulls = append(nulls, k)
		}
	}
	want, _ := row["integrityHash"].(string)
	for mask := 1; mask < 1<<len(nulls); mask++ {
		variant := map[string]any{}
		for k, v := range row {
			variant[k] = v
		}
		for i, k := range nulls {
			if mask&(1<<i) != 0 {
				variant[k] = ""
			}
		}
		if hashHistoryRow(variant) == want || legacyUndefinedVariant(variant) == want {
			return true
		}
	}
	return false
}

// rowHashIsValid is WeKan's: the hash, or one of the two legacy forms.
func rowHashIsValid(row map[string]any) bool {
	row = plain(row).(map[string]any)
	h, _ := row["integrityHash"].(string)
	if !hashPattern.MatchString(h) {
		return false
	}
	if hashHistoryRow(row) == h || legacyUndefinedVariant(row) == h {
		return true
	}
	return cleanedEmptyStringMatches(row)
}
