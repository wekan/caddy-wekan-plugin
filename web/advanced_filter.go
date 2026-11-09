package web

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's Advanced Filter (imports/lib/advancedFilter.js): the language of the
// Filter sidebar's text field, and of a rule's "card matches advanced filter"
// trigger - one implementation, so the sidebar and the rule never disagree:
//
//	Points = 2 and not (Other > 1)      'Field 1' == 'Value 1'
//	F1 == /Tes.*/i                      @dueAt < '2026-09-01'   @endAt = none
//
// A filter becomes a query: a custom field by its name, compared in the SAME
// array entry as its id (customFields.$elemMatch), a dropdown value by its
// option's name, a number by the whole decimal token, a date field by a
// range, and the card's own dates by @name. A filter that does not parse is
// an error - the sidebar keeps the last valid one, a rule matches nothing.

var errAdvancedFilter = errors.New("invalid advanced filter")

type filterToken struct {
	cmd         string
	str, regex  bool
	selector    bson.M // a condition, once processed
	hasSelector bool
}

// tokenizeAdvancedFilter is WeKan's tokenizeAdvancedFilter.
func tokenizeAdvancedFilter(filter string) ([]filterToken, error) {
	var out []filterToken
	var cur strings.Builder
	str, regex, wasStr, ignore := false, false, false, false
	push := func() {
		out = append(out, filterToken{cmd: cur.String(), str: wasStr, regex: regex})
		cur.Reset()
		wasStr, regex = false, false
	}
	for _, ch := range filter {
		if ignore {
			ignore = false
			cur.WriteRune(ch)
			continue
		}
		switch {
		case ch == '/' && (!str || regex):
			str = !str
			if str {
				regex = true
			}
			cur.WriteRune(ch)
		case ch == '\'' && !regex:
			str = !str
			if str {
				wasStr = true
			}
		case ch == '\\' && !str:
			ignore = true
		case (ch == '(' || ch == ')') && !str:
			if cur.Len() > 0 || wasStr || regex {
				push()
			}
			out = append(out, filterToken{cmd: string(ch)})
		case ch == ' ' && !str:
			push()
		default:
			cur.WriteRune(ch)
		}
	}
	if str || ignore {
		return nil, errAdvancedFilter
	}
	if cur.Len() > 0 || wasStr || regex {
		push()
	}
	return out, nil
}

var (
	ymdRe = regexp.MustCompile(`^(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})(?:[ T](\d{1,2}):(\d{2})(?::(\d{2}))?)?$`)
	dmyRe = regexp.MustCompile(`^(\d{1,2})[-/.](\d{1,2})[-/.](\d{4})(?:[ T](\d{1,2}):(\d{2})(?::(\d{2}))?)?$`)
)

type dateRange struct{ start, end time.Time }

// filterDateRange is WeKan's buildRange: the day, minute or second typed, in
// the server's time zone; a date that does not exist (31 February) is none.
func filterDateRange(y, mo, d int, h, mi, sec string, loc *time.Location) *dateRange {
	hh, mm, ss := 0, 0, 0
	if h != "" {
		hh, _ = strconv.Atoi(h)
		mm, _ = strconv.Atoi(mi)
	}
	if sec != "" {
		ss, _ = strconv.Atoi(sec)
	}
	start := time.Date(y, time.Month(mo), d, hh, mm, ss, 0, loc)
	if start.Year() != y || int(start.Month()) != mo || start.Day() != d || start.Hour() != hh || start.Minute() != mm || start.Second() != ss {
		return nil
	}
	end := start.AddDate(0, 0, 1)
	if sec != "" {
		end = start.Add(time.Second)
	} else if h != "" {
		end = start.Add(time.Minute)
	}
	return &dateRange{start, end}
}

// parseAdvancedFilterDate is WeKan's: YYYY-MM-DD, or a/b/YYYY read day-first
// when dayFirst - an unambiguous one either way - with an optional time.
func parseAdvancedFilterDate(s string, dayFirst bool) *dateRange {
	s = strings.TrimSpace(s)
	n := func(v string) int { i, _ := strconv.Atoi(v); return i }
	if m := ymdRe.FindStringSubmatch(s); m != nil {
		return filterDateRange(n(m[1]), n(m[2]), n(m[3]), m[4], m[5], m[6], time.Local)
	}
	if m := dmyRe.FindStringSubmatch(s); m != nil {
		a, b, y := n(m[1]), n(m[2]), n(m[3])
		day, month := b, a
		switch {
		case a > 12 && b <= 12:
			day, month = a, b
		case b > 12 && a <= 12:
			day, month = b, a
		case dayFirst:
			day, month = a, b
		}
		return filterDateRange(y, month, day, m[4], m[5], m[6], time.Local)
	}
	return nil
}

// dateValueSelector is WeKan's buildDateValueSelector.
func dateValueSelector(op string, r *dateRange) bson.M {
	if r == nil {
		return nil
	}
	switch op {
	case "=", "==", "===":
		return bson.M{"$gte": r.start, "$lt": r.end}
	case "!=", "!==":
		return bson.M{"$not": bson.M{"$gte": r.start, "$lt": r.end}}
	case ">", "gt", "Gt", "GT":
		return bson.M{"$gte": r.end}
	case ">=", ">==", "gte", "Gte", "GTE":
		return bson.M{"$gte": r.start}
	case "<", "lt", "Lt", "LT":
		return bson.M{"$lt": r.start}
	case "<=", "<==", "lte", "Lte", "LTE":
		return bson.M{"$lt": r.end}
	}
	return nil
}

var filterNumberRe = regexp.MustCompile(`(?i)^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:e[+-]?\d+)?$`)

// filterNumber is WeKan's: the whole decimal token, never "2" of "2hours".
func filterNumber(v string) (float64, bool) {
	if !filterNumberRe.MatchString(v) {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// filterField is one custom field of the board, as the resolvers read it.
type filterField struct {
	ID, Type string
	Options  map[string]string // dropdown option name -> id
}

// advancedFilter is a parse with the board's custom fields.
type advancedFilter struct {
	fields   map[string]filterField
	dayFirst bool
}

func (f advancedFilter) condition(field, op string, value filterToken) (bson.M, error) {
	def, ok := f.fields[field]
	if !ok || def.ID == "" {
		return nil, errAdvancedFilter
	}
	elem := func(v any) bson.M {
		return bson.M{"customFields": bson.M{"$elemMatch": bson.M{"_id": def.ID, "value": v}}}
	}
	var date bson.M
	if def.Type == "date" {
		date = dateValueSelector(op, parseAdvancedFilterDate(value.cmd, f.dayFirst))
	}
	equal := func() bson.A {
		v := value.cmd
		if id, ok := def.Options[v]; ok {
			v = id
		}
		out := bson.A{v}
		if n, ok := filterNumber(value.cmd); ok {
			out = append(out, n)
		}
		return out
	}
	regex := func() (bson.Regex, error) {
		m := regexp.MustCompile(`^/(.*?)/([gimy]*)$`).FindStringSubmatch(value.cmd)
		if m == nil {
			return bson.Regex{}, errAdvancedFilter
		}
		if _, err := regexp.Compile(m[1]); err != nil {
			return bson.Regex{}, errAdvancedFilter
		}
		return bson.Regex{Pattern: m[1], Options: strings.NewReplacer("g", "", "y", "").Replace(m[2])}, nil
	}
	switch op {
	case "=", "==", "===":
		if value.regex {
			re, err := regex()
			if err != nil {
				return nil, err
			}
			return elem(re), nil
		}
		if date != nil {
			return elem(date), nil
		}
		return elem(bson.M{"$in": equal()}), nil
	case "!=", "!==":
		if value.regex {
			re, err := regex()
			if err != nil {
				return nil, err
			}
			return elem(bson.M{"$not": re}), nil
		}
		if date != nil {
			return elem(date), nil
		}
		return elem(bson.M{"$not": bson.M{"$in": equal()}}), nil
	}
	cmp := map[string]string{">": "$gt", "gt": "$gt", "Gt": "$gt", "GT": "$gt", ">=": "$gte", ">==": "$gte", "gte": "$gte", "Gte": "$gte", "GTE": "$gte",
		"<": "$lt", "lt": "$lt", "Lt": "$lt", "LT": "$lt", "<=": "$lte", "<==": "$lte", "lte": "$lte", "Lte": "$lte", "LTE": "$lte"}[op]
	if cmp == "" {
		return nil, errAdvancedFilter
	}
	if date != nil {
		return elem(date), nil
	}
	n, ok := filterNumber(value.cmd)
	if !ok {
		return nil, errAdvancedFilter
	}
	return elem(bson.M{cmp: n}), nil
}

var cardDateFields = map[string]string{"@createdat": "createdAt", "@receivedat": "receivedAt", "@startat": "startAt",
	"@dueat": "dueAt", "@endat": "endAt", "@listenteredat": "listEnteredAt"}

// cardDateCondition is WeKan's: @dueAt and the card's other dates, "none" for
// a card without one - and a date that is not one never matches a comparison.
func (f advancedFilter) cardDateCondition(name, op, value filterToken) (bson.M, error) {
	field := cardDateFields[strings.ToLower(name.cmd)]
	if field == "" || op.str || op.regex || value.regex {
		return nil, errAdvancedFilter
	}
	if strings.EqualFold(value.cmd, "none") {
		switch op.cmd {
		case "=", "==", "===":
			return bson.M{field: nil}, nil
		case "!=", "!==":
			return bson.M{field: bson.M{"$type": 9, "$ne": nil, "$exists": true}}, nil
		}
		return nil, errAdvancedFilter
	}
	cmp := dateValueSelector(op.cmd, parseAdvancedFilterDate(value.cmd, f.dayFirst))
	if cmp == nil {
		return nil, errAdvancedFilter
	}
	cmp["$type"], cmp["$ne"], cmp["$exists"] = 9, nil, true
	return bson.M{field: cmp}, nil
}

// selector is WeKan's advancedFilterStringToSelector: and/or left to right,
// brackets to choose otherwise, not on one atom.
func (f advancedFilter) selector(filter string) (bson.M, error) {
	raw, err := tokenizeAdvancedFilter(filter)
	if err != nil {
		return nil, err
	}
	var tokens []filterToken
	for _, t := range raw {
		if t.cmd != "" || t.str || t.regex {
			tokens = append(tokens, t)
		}
	}
	i := 0
	op := func(t *filterToken) string {
		if t == nil || t.str || t.regex {
			return ""
		}
		return strings.ToLower(t.cmd)
	}
	at := func(k int) *filterToken {
		if k < len(tokens) {
			return &tokens[k]
		}
		return nil
	}
	var expression func(depth int) (bson.M, error)
	var atom func(depth int) (bson.M, error)
	atom = func(depth int) (bson.M, error) {
		if depth > 100 {
			return nil, errAdvancedFilter
		}
		t := at(i)
		switch op(t) {
		case "not", "!":
			i++
			inner, err := atom(depth + 1)
			if err != nil {
				return nil, err
			}
			return bson.M{"$nor": bson.A{inner}}, nil
		case "(":
			i++
			inner, err := expression(depth + 1)
			if err != nil {
				return nil, err
			}
			if op(at(i)) != ")" {
				return nil, errAdvancedFilter
			}
			i++
			return inner, nil
		case ")":
			return nil, errAdvancedFilter
		}
		if t == nil || i+3 > len(tokens) {
			return nil, errAdvancedFilter
		}
		name, cmp, value := tokens[i], tokens[i+1], tokens[i+2]
		i += 3
		if !name.str && !name.regex && strings.HasPrefix(name.cmd, "@") {
			return f.cardDateCondition(name, cmp, value)
		}
		if cmp.str {
			return nil, errAdvancedFilter
		}
		return f.condition(name.cmd, cmp.cmd, value)
	}
	expression = func(depth int) (bson.M, error) {
		left, err := atom(depth)
		if err != nil {
			return nil, err
		}
		for {
			o := op(at(i))
			key := map[string]string{"and": "$and", "&": "$and", "&&": "$and", "or": "$or", "|": "$or", "||": "$or"}[o]
			if key == "" {
				return left, nil
			}
			i++
			right, err := atom(depth)
			if err != nil {
				return nil, err
			}
			left = bson.M{key: bson.A{left, right}}
		}
	}
	out, err := expression(0)
	if err != nil {
		return nil, err
	}
	if i != len(tokens) {
		return nil, errAdvancedFilter
	}
	return bson.M{"$or": bson.A{out}}, nil
}

// boardAdvancedFilter reads the board's custom fields for a parse: by name,
// a dropdown's options by name, as WeKan's
// buildAdvancedFilterResolversFromCustomFields.
func (s *Server) boardAdvancedFilter(ctx context.Context, boardID string) advancedFilter {
	f := advancedFilter{fields: map[string]filterField{}}
	cur, err := s.db.Collection("customFields").Find(ctx, bson.M{"boardIds": eq(boardID)})
	if err != nil {
		return f
	}
	var defs []bson.M
	_ = cur.All(ctx, &defs)
	for _, d := range defs {
		def := filterField{ID: strOf(d["_id"]), Type: strOf(d["type"]), Options: map[string]string{}}
		for id, name := range dropdownItems(d) {
			if _, taken := def.Options[name]; !taken {
				def.Options[name] = id
			}
		}
		f.fields[strOf(d["name"])] = def
	}
	return f
}

// cardMatchesAdvancedFilter is WeKan's: the card's current state, by the
// database's own query engine, as the sidebar does.
func (s *Server) cardMatchesAdvancedFilter(ctx context.Context, boardID, cardID, filter string) bool {
	if strings.TrimSpace(filter) == "" || !validID(cardID) {
		return false
	}
	sel, err := s.boardAdvancedFilter(ctx, boardID).selector(filter)
	if err != nil {
		return false
	}
	q := bson.M{"$and": bson.A{sel, bson.M{"_id": eq(cardID), "boardId": eq(boardID)}}}
	n, err := s.db.Collection("cards").CountDocuments(ctx, q)
	return err == nil && n > 0
}

// filteredCardIDs are the board's cards an advanced filter shows; ok is false
// for a filter that does not parse.
func (s *Server) filteredCardIDs(ctx context.Context, boardID, filter string) (map[string]bool, bool) {
	sel, err := s.boardAdvancedFilter(ctx, boardID).selector(filter)
	if err != nil {
		return nil, false
	}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"$and": bson.A{sel, bson.M{"boardId": eq(boardID)}}})
	if err != nil {
		return nil, false
	}
	var rows []struct {
		ID string `bson:"_id"`
	}
	_ = cur.All(ctx, &rows)
	out := map[string]bool{}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out, true
}
