package web

import (
	"encoding/json"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A date whose milliseconds end in zero is written with all three digits, as
// toISOString writes it - the export's "…53.67Z" was another date to WeKan.
func TestJSDates(t *testing.T) {
	at := time.Date(2026, 11, 12, 23, 41, 53, 670_000_000, time.UTC)
	got := jsDates(map[string]any{"a": at, "b": []map[string]any{{"c": bson.NewDateTimeFromTime(at.Add(330 * time.Millisecond))}},
		"d": bson.D{{Key: "e", Value: &at}}, "f": "x", "g": []byte("y"), "h": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	raw, _ := json.Marshal(got)
	want := `{"a":"2026-11-12T23:41:53.670Z","b":[{"c":"2026-11-12T23:41:54.000Z"}],"d":{"e":"2026-11-12T23:41:53.670Z"},"f":"x","g":"eQ==","h":"2026-01-01T00:00:00.000Z"}`
	if string(raw) != want {
		t.Fatalf("%s", raw)
	}
	// Negative: what encoding/json alone writes, which this replaces.
	if plainRaw, _ := json.Marshal(at); string(plainRaw) != `"2026-11-12T23:41:53.67Z"` {
		t.Fatalf("encoding/json writes %s", plainRaw)
	}
}
