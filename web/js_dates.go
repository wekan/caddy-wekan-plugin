package web

import (
	"reflect"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// jsDates is a document with its dates as Node.js WeKan's JSON writes them,
// Date.prototype.toISOString - always three digits of milliseconds - where
// encoding/json writes a time.Time as RFC 3339 with its trailing zeros cut
// ("…53.67Z", "…53Z"), which a reader comparing strings sees as another
// date. Maps and slices are walked; other values are as they are.
func jsDates(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return jsISO(x)
	case *time.Time:
		if x == nil {
			return nil
		}
		return jsISO(*x)
	case bson.DateTime:
		return jsISO(x.Time())
	case bson.D:
		m := make(map[string]any, len(x))
		for _, e := range x {
			m[e.Key] = jsDates(e.Value)
		}
		return m
	case string, bool, int, int32, int64, float64:
		return v
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v
		}
		m := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			m[it.Key().String()] = jsDates(it.Value().Interface())
		}
		return m
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return v // bytes stay bytes
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = jsDates(rv.Index(i).Interface())
		}
		return out
	}
	return v
}
