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
	boards, err := trelloImportArchive(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range docsOf(boards[0].doc, "attachments") {
		reader := boards[0].files(row)
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
		_, err := trelloImportArchive(makeZip(map[string][]byte{name: []byte(`{"name":"Incomplete board"}`)}))
		if err == nil {
			t.Fatalf("accepted archive with %q", name)
		}
	}
}

func TestTrelloArchiveKeepsNestedBoardFilesLocal(t *testing.T) {
	const board = `{"name":"Board","lists":[{"id":"list","name":"List"}],
		"cards":[{"id":"card","idList":"list","attachments":[{"id":"file","fileName":"same.txt"}]}]}`
	raw := makeZip(map[string][]byte{
		"outer/board.json":                      []byte(board),
		"outer/board/board.json":                []byte(board),
		"outer/attachments/file_same.txt":       []byte("outer"),
		"outer/board/attachments/file_same.txt": []byte("inner"),
	})
	boards, err := trelloImportArchive(raw)
	if err != nil || len(boards) != 2 {
		t.Fatalf("expected two boards: %v", err)
	}
	for index, want := range []string{"outer", "inner"} {
		row := docsOf(boards[index].doc, "attachments")[0]
		reader := boards[index].files(row)
		if reader == nil {
			t.Fatalf("attachment missing from %s", want)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || string(data) != want {
			t.Fatalf("%s attachment = %q, %v", want, data, err)
		}
	}
}
