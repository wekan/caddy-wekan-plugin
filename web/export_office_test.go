package web

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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

	code, typ, data := get(boardURL+"/exportExcel", a)
	if code != 200 || typ != xlsxType {
		t.Fatalf("excel: %d %s", code, typ)
	}
	if s := sheet(data); !strings.Contains(s, `<t xml:space="preserve">Title</t>`) || !strings.Contains(s, `<t xml:space="preserve">C</t>`) {
		t.Fatalf("WeKan's header and the card: %s", s)
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
