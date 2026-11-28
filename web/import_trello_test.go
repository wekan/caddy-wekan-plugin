package web

import (
	"io"
	"testing"
)

func TestTrelloArchiveResolvesAttachmentsByID(t *testing.T) {
	raw := makeZip(map[string][]byte{
		"board.json": []byte(`{"name":"Board","lists":[{"id":"list","name":"List"}],
			"cards":[{"id":"first","idList":"list","attachments":[{"id":"a","fileName":"same.txt"}]},
			{"id":"second","idList":"list","attachments":[{"id":"b","fileName":"same.txt"}]}]}`),
		"attachments/b_same.txt": []byte("second"),
		"attachments/a_same.txt": []byte("first"),
	})
	doc, attachment, err := trelloImportArchive(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range docsOf(doc, "attachments") {
		reader := attachment(row)
		if reader == nil {
			t.Fatalf("attachment %q missing", row["_id"])
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || string(data) != row["cardId"] {
			t.Fatalf("attachment %q = %q, %v", row["_id"], data, err)
		}
	}
}

func TestTrelloArchiveRequiresBoardDocument(t *testing.T) {
	for _, name := range []string{"missing.json", "board.json"} {
		_, _, err := trelloImportArchive(makeZip(map[string][]byte{name: []byte(`{"name":"Incomplete board"}`)}))
		if err == nil {
			t.Fatalf("accepted archive with %q", name)
		}
	}
}
