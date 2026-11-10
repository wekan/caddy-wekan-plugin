package web

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestExportExcelAndPDF(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Office (x)")
	get := func(url, token string) (int, string, []byte) {
		req, _ := http.NewRequest("GET", url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Content-Type"), data
	}
	sheet := func(data []byte) string {
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("an .xlsx is a ZIP: %v", err)
		}
		names := map[string]string{}
		for _, f := range z.File {
			r, _ := f.Open()
			x, _ := io.ReadAll(r)
			names[f.Name] = string(x)
		}
		for _, part := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels"} {
			if _, ok := names[part]; !ok {
				t.Errorf("the workbook has %s", part)
			}
		}
		return names["xl/worksheets/sheet1.xml"]
	}
	boardURL := w.srv.URL + "/api/boards/" + b.ID
	cardURL := boardURL + "/lists/" + b.List + "/cards/" + b.Card

	// With Card details - WeKan's default - the card in the card layout;
	// without it, WeKan's table with its header.
	code, typ, data := get(boardURL+"/exportExcel", a)
	if code != 200 || typ != xlsxType {
		t.Fatalf("excel: %d %s", code, typ)
	}
	if s := sheet(data); !strings.Contains(s, `<t xml:space="preserve">Swimlane: Default</t>`) || !strings.Contains(s, `<t xml:space="preserve">C</t>`) {
		t.Fatalf("the card layout and the card: %s", s)
	}
	code, _, data = get(boardURL+"/exportExcel?fields=description", a)
	if s := sheet(data); code != 200 || !strings.Contains(s, `<t xml:space="preserve">Title</t>`) || !strings.Contains(s, `<t xml:space="preserve">C</t>`) {
		t.Fatalf("WeKan's header and the card: %d %s", code, s)
	}
	code, typ, data = get(boardURL+"/exportPDF", a)
	if code != 200 || typ != "application/pdf" || !bytes.HasPrefix(data, []byte("%PDF-1.4")) || !bytes.HasSuffix(data, []byte("%%EOF\n")) || len(data) < 200 {
		t.Fatalf("pdf: %d %s %q", code, typ, data[:min(len(data), 20)])
	}
	if !bytes.Contains(data, []byte(`(Office \(x\)) Tj`)) || !bytes.Contains(data, []byte("(    - C) Tj")) {
		t.Fatalf("the board title, escaped, and its card: %s", data)
	}
	if code, typ, _ = get(cardURL+"/exportExcel", a); code != 200 || typ != xlsxType {
		t.Fatalf("a card's excel: %d", code)
	}
	if code, typ, _ = get(cardURL+"/exportPDF", a); code != 200 || typ != "application/pdf" {
		t.Fatalf("a card's pdf: %d", code)
	}

	for _, url := range []string{boardURL + "/exportExcel", boardURL + "/exportPDF", cardURL + "/exportPDF"} {
		if code, _, _ = get(url, ""); code != 401 {
			t.Errorf("negative: no token, %s: %d", url, code)
		}
		if code, _, _ = get(url, bt); code != 403 {
			t.Errorf("negative: a non-member, %s: %d", url, code)
		}
	}
	// WeKan's missing board: plain text, with a token or without, and no file.
	for _, path := range []string{"/exportExcel", "/exportPDF", "/lists/" + b.List + "/cards/" + b.Card + "/exportPDF"} {
		for _, token := range []string{a, ""} {
			req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/nosuchboard00001"+path, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			res, err := testClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 404 || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" || string(data) != "Board not found" || res.Header.Get("Content-Disposition") != "" {
				t.Errorf("negative: a missing board's %s: %d %q %q", path, res.StatusCode, res.Header.Get("Content-Type"), data)
			}
		}
	}
	if code, _, _ = get(boardURL+"/lists/"+b.List+"/cards/nosuchcard000001/exportPDF", a); code != 404 {
		t.Errorf("negative: a card not on the board: %d", code)
	}

	// An assigned-only member: not the whole board, not a card not theirs -
	// only a card assigned to them.
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	for _, url := range []string{boardURL + "/exportExcel", boardURL + "/exportPDF", cardURL + "/exportExcel", cardURL + "/exportPDF"} {
		if code, _, _ = get(url, bt); code != 403 {
			t.Errorf("negative: assigned-only, %s: %d", url, code)
		}
	}
	_, _ = w.db.Collection("cards").UpdateOne(context.Background(), bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"assignees": bson.A{bob.ID}}})
	if code, _, _ = get(cardURL+"/exportPDF", bt); code != 200 {
		t.Errorf("their own card: %d", code)
	}
	if code, _, _ = get(boardURL+"/exportPDF?authToken="+a, ""); code != 200 {
		t.Errorf("a download link: %d", code)
	}
}

func TestPDFTextEscapes(t *testing.T) {
	if got := pdfText("a(b)c\\d\tä€\x01"); got != "a\\(b\\)c\\\\d    \xe4?" {
		t.Fatalf("%q", got)
	}
}

// A card's PDF has its checklists, ticked or not, and its comments with
// their authors - anonymized when Admin Panel says so.
func TestCardPDFDetails(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "annreal", "pw")
	a := login(t, w, "annreal")
	b := newRESTBoard(t, w, a, "PDF")
	ctx := context.Background()
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "pdfchecklist0001", "cardId": b.Card, "title": "Steps", "sort": 0.0})
	_, _ = w.db.Collection("checklistItems").InsertMany(ctx, []any{
		bson.M{"_id": "pdfitem000000001", "checklistId": "pdfchecklist0001", "cardId": b.Card, "title": "Done one", "isFinished": true, "sort": 0.0},
		bson.M{"_id": "pdfitem000000002", "checklistId": "pdfchecklist0001", "cardId": b.Card, "title": "Open one", "isFinished": false, "sort": 1.0}})
	_, _ = w.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": "pdfcomment000001", "boardId": b.ID, "cardId": b.Card, "userId": ann.ID,
		"text": "Looks good", "createdAt": time.Now()})
	pdf := func() string {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/lists/"+b.List+"/cards/"+b.Card+"/exportPDF", nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return string(data)
	}
	got := pdf()
	for _, want := range []string{"(Steps) Tj", "(    [x] Done one) Tj", "(    [ ] Open one) Tj", "(annreal: Looks good) Tj"} {
		if !strings.Contains(got, want) {
			t.Errorf("the card's PDF has %q", want)
		}
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"anonymizeExportUsers": true}})
	if got := pdf(); strings.Contains(got, "annreal") {
		t.Error("negative: anonymized, the commenter is not named")
	}
}
