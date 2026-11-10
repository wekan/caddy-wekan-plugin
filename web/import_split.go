package web

import (
	"errors"
	"strconv"
)

// "One board per project", as Node.js WeKan's models/lib/importSplit.js
// splits a document of the generalized importer: one document per swimlane,
// so a tool's export that holds several projects - Vikunja's, Plane's and
// Redmine's projects, Wrike's folders, ClickUp's lists - becomes one board
// per project. Each part keeps every list (or, with swimlane_columns, the
// lane's own), the labels and the board-level data, takes the swimlane's
// name as its board name and puts its cards in its Default swimlane. A
// parent card or a dependency on a card that went to another board is
// reported in that part's losses; the source's own report goes with the
// first part.

// maxSplitBoards is MAX_SPLIT_BOARDS.
const maxSplitBoards = 200

func splitLaneOf(task any) any {
	if v := jsGetOrUndef(task, "swimlane_name"); jsTruthy(v) {
		return v
	}
	if v := jsGetOrUndef(task, "swimlane"); jsTruthy(v) {
		return v
	}
	return "Default"
}

// splitBySwimlane is splitBySwimlane: the parts, or the document itself when
// it has fewer than two swimlanes.
func splitBySwimlane(parsed jsObject) ([]jsObject, error) {
	tasks, _ := jsGet(parsed, "tasks").([]any)
	var lanes []any
	for _, t := range tasks {
		l := splitLaneOf(t)
		seen := false
		for _, x := range lanes {
			if jsSameValueZero(x, l) {
				seen = true
				break
			}
		}
		if !seen {
			lanes = append(lanes, l)
		}
	}
	if len(lanes) < 2 {
		return []jsObject{parsed}, nil
	}
	if len(lanes) > maxSplitBoards {
		return nil, errors.New("the import has " + strconv.Itoa(len(lanes)) + " swimlanes; one board per project makes at most " + strconv.Itoa(maxSplitBoards) + " boards")
	}
	laneOfRef := map[string]any{}
	for _, t := range tasks {
		if ref := jsGetOrUndef(t, "ref"); ref != nil && !isUndef(ref) && ref != "" {
			laneOfRef[jsStr(ref)] = splitLaneOf(t)
		}
	}
	boardName := any("Imported board")
	if n := jsGetOrUndef(jsGet(parsed, "board"), "name"); jsTruthy(n) {
		boardName = n
	}
	var parts []jsObject
	for index, lane := range lanes {
		unsupported := []any{}
		if prev, ok := jsGet(parsed, "unsupported").([]any); index == 0 && ok {
			unsupported = append(unsupported, prev...)
		}
		elsewhere := func(ref any) bool {
			if ref == nil || isUndef(ref) {
				return false
			}
			l, ok := laneOfRef[jsStr(ref)]
			return ok && !jsStrictEq(l, lane)
		}
		lost := func(path, reason string) {
			o := jsObject{}
			o.set("path", path)
			o.set("reason", reason)
			unsupported = append(unsupported, o)
		}
		own := []any{}
		for _, t := range tasks {
			if !jsSameValueZero(splitLaneOf(t), lane) {
				continue
			}
			task, _ := t.(jsObject)
			cp := append(jsObject{}, task...)
			cp.set("swimlane_name", "Default")
			cp.del("swimlane")
			name := jsGetOrUndef(t, "ref")
			if !jsTruthy(name) {
				name = jsGetOrUndef(t, "title")
			}
			where := "/" + jsStr(lane) + "/" + jsStr(name)
			if parent := jsGetOrUndef(t, "parent_ref"); elsewhere(parent) {
				lost(where, `its parent card "`+jsStr(parent)+`" went to the board "`+jsStr(laneOfRef[jsStr(parent)])+`"`)
				cp.del("parent_ref")
			}
			if deps, ok := jsGet(t, "dependencies").([]any); ok {
				kept := []any{}
				for _, d := range deps {
					var ref any = undefinedValue{}
					if jsTruthy(d) {
						ref = jsGetOrUndef(d, "ref")
					}
					if !elsewhere(ref) {
						kept = append(kept, d)
						continue
					}
					lost(where, `its dependency on "`+jsStr(ref)+`" went to the board "`+jsStr(laneOfRef[jsStr(ref)])+`"`)
				}
				cp.set("dependencies", kept)
			}
			own = append(own, cp)
		}
		columns := jsGetOrUndef(parsed, "columns")
		if sc := jsGet(parsed, "swimlane_columns"); jsTruthy(sc) {
			if titles, ok := jsGet(sc, jsStr(lane)).([]any); ok {
				cols := []any{}
				for _, title := range titles {
					o := jsObject{}
					o.set("title", title)
					cols = append(cols, o)
				}
				columns = cols
			}
		}
		board := jsObject{}
		if b, ok := jsGet(parsed, "board").(jsObject); ok {
			board = append(board, b...)
		}
		if jsStrictEq(lane, "Default") {
			board.set("name", boardName)
		} else {
			board.set("name", lane)
		}
		lane0 := jsObject{}
		lane0.set("name", "Default")
		warnings := []any{}
		if w, ok := jsGet(parsed, "warnings").([]any); index == 0 && ok {
			warnings = w
		}
		part := append(jsObject{}, parsed...)
		part.set("columns", columns)
		part.set("board", board)
		part.set("swimlanes", []any{lane0})
		part.set("tasks", own)
		part.set("warnings", warnings)
		part.set("unsupported", unsupported)
		part.del("swimlane_columns")
		parts = append(parts, part)
	}
	return parts, nil
}

// jsSameValueZero is a Set's equality: === with NaN equal to NaN.
func jsSameValueZero(a, b any) bool {
	if x, ok := a.(float64); ok && x != x {
		y, ok := b.(float64)
		return ok && y != y
	}
	return jsStrictEq(a, b)
}
