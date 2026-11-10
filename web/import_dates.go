package web

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// jsDateTime is what V8's new Date(text) reads from an ISO date-time - the
// shape WeKan's CSV imports hand it: YYYY-MM-DDTHH:mm[:ss[.fff...]] (a space
// for the T reads the same)
// with Z, z, +HH:MM or +HHMM, or no zone at all, which is read at UTC as on
// WeKan's server. As V8 does, a day past the end of its month rolls over
// (2026-02-30 is 2 March), 24:00 is the next midnight, a fraction is cut to
// milliseconds, and anything else - a month 13, a minute 60, a zone +25:00,
// text after the zone - is no date.
var jsDateTimeShape = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})[Tt ](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?([Zz]|[+-]\d{2}:?\d{2})?$`)

// jsDateDay is V8's date-only form: that day at UTC midnight, a day past the
// end of its month rolled over.
var jsDateDay = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)

// jsDateV8 is new Date(text) for the ISO shapes - a day, or a date-time with
// T or a space before its time.
func jsDateV8(text string) (time.Time, bool) {
	if m := jsDateDay.FindStringSubmatch(text); m != nil {
		n := func(s string) int { v, _ := strconv.Atoi(s); return v }
		if n(m[2]) < 1 || n(m[2]) > 12 || n(m[3]) < 1 || n(m[3]) > 31 {
			return time.Time{}, false
		}
		return time.Date(n(m[1]), time.Month(n(m[2])), n(m[3]), 0, 0, 0, 0, time.UTC), true
	}
	return jsDateTime(text)
}

func jsDateTime(text string) (time.Time, bool) {
	m := jsDateTimeShape.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	year, month, day, hour, minute, second := n(m[1]), n(m[2]), n(m[3]), n(m[4]), n(m[5]), n(m[6])
	ms := 0
	if m[7] != "" {
		f := (m[7] + "00")[:3]
		ms = n(f)
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || minute > 59 || second > 59 {
		return time.Time{}, false
	}
	if hour > 24 || (hour == 24 && (minute != 0 || second != 0 || ms != 0)) {
		return time.Time{}, false
	}
	offset := 0
	if z := m[8]; z != "" && z != "Z" && z != "z" {
		digits := z[1:]
		if len(digits) == 5 {
			digits = digits[:2] + digits[3:]
		}
		hh, mm := n(digits[:2]), n(digits[2:])
		if hh > 23 || mm > 59 {
			return time.Time{}, false
		}
		offset = hh*60 + mm
		if z[0] == '-' {
			offset = -offset
		}
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, ms*int(time.Millisecond), time.UTC)
	return t.Add(-time.Duration(offset) * time.Minute), true
}

// jsISO is Date.prototype.toISOString: a year outside 0000-9999 as six
// digits with its sign, as JavaScript writes it.
func jsISO(t time.Time) string {
	t = t.UTC()
	rest := t.Format("-01-02T15:04:05.000Z")
	switch y := t.Year(); {
	case y < 0:
		return "-" + fmt.Sprintf("%06d", -y) + rest
	case y > 9999:
		return "+" + fmt.Sprintf("%06d", y) + rest
	default:
		return fmt.Sprintf("%04d", y) + rest
	}
}

// jsSort is Array.prototype.sort with a comparator, as V8 sorts an array of
// under 64 elements - which matters when the comparator is not a consistent
// order, as one that returns NaN for a missing createAt is not: under 8, a
// binary insertion sort from the second element; from 8, the natural run at
// the start first (a descending one, which any answer but "less" ends,
// reversed), then binary insertion of the rest. A longer array is sorted
// stably, which is V8's result for any consistent comparator.
// TestJSSort checks it against V8 itself.
func jsSort(items []any, cmp func(a, b any) float64) {
	n := len(items)
	if n >= 64 {
		sort.SliceStable(items, func(i, j int) bool { return cmp(items[i], items[j]) < 0 })
		return
	}
	run := 1
	if n >= 8 {
		run = 2
		descending := cmp(items[1], items[0]) < 0
		prev := items[1]
		for i := 2; i < n; i++ {
			order := cmp(items[i], prev)
			if descending && !(order < 0) || !descending && order < 0 {
				break
			}
			prev = items[i]
			run++
		}
		if descending {
			for i, j := 0, run-1; i < j; i, j = i+1, j-1 {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	for start := run; start < n; start++ {
		pivot := items[start]
		left, right := 0, start
		for left < right {
			mid := left + (right-left)>>1
			if cmp(pivot, items[mid]) < 0 {
				right = mid
			} else {
				left = mid + 1
			}
		}
		copy(items[left+1:start+1], items[left:start])
		items[left] = pivot
	}
}
