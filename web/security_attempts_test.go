package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The attacks Weddy refused without a word are refused, recorded under
// WeKan's Hall of Fame names and cost the sender the account, as WeKan's
// specs of PrototypeBleed, RepointBleed, BackgroundBleed and HookBleed ask -
// and the ordinary calls beside them are not recorded (negative).
func TestRefusedAttacksAreRecorded(t *testing.T) {
	realLookup := lookupHost
	t.Cleanup(func() { lookupHost = realLookup })
	lookupHost = func(_ context.Context, host string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	sink := newWebhookSink(t)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	w := newTestWeKan(t)
	ctx := context.Background()
	owner := w.seedUser(t, "owner", "pw")
	victim := newRESTBoard(t, w, login(t, w, "owner"), "Victim")
	_ = owner
	secret := "att" + randomID()[:12]
	_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": secret, "name": "secret.txt", "meta": bson.M{"boardId": victim.ID, "cardId": victim.Card}})

	n := 0
	// attacker: a fresh board admin of their own board, as WeKan's specs use,
	// since an attempt disables the account.
	attacker := func() (u *User, rb restBoard, call func(method, args string) map[string]any) {
		n++
		name := "attacker" + string(rune('a'+n))
		u = w.seedUser(t, name, "pw")
		rb = newRESTBoard(t, w, login(t, w, name), "Mine "+name)
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		call = func(method, args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(args))
			req.Header.Set("Content-Type", "application/json")
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(res.Body).Decode(&out)
			return out
		}
		return
	}
	disabled := func(u *User) bool {
		var d struct {
			LoginDisabled bool `bson:"loginDisabled"`
		}
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&d)
		return d.LoginDisabled
	}
	count := func(bleed string) int {
		cur, _ := w.db.Collection("eventlog").Find(ctx, bson.M{"bleed": bleed})
		var rows []struct {
			Count int `bson:"count"`
		}
		_ = cur.All(ctx, &rows)
		total := 0
		for _, r := range rows {
			total += r.Count
		}
		return total
	}
	expectRecorded := func(t *testing.T, bleed string, before int, u *User) {
		t.Helper()
		if got := count(bleed); got <= before {
			t.Errorf("%s not recorded (%d rows' count, %d before)", bleed, got, before)
		}
		if !disabled(u) {
			t.Errorf("%s did not disable the sender's account", bleed)
		}
	}
	expectQuiet := func(t *testing.T, u *User, what string) {
		t.Helper()
		if disabled(u) {
			t.Errorf("negative: %s disabled the account", what)
		}
	}

	t.Run("PrototypeBleed", func(t *testing.T) {
		u, rb, call := attacker()
		var list struct {
			ID string `bson:"_id"`
		}
		_ = w.db.Collection("lists").FindOne(ctx, bson.M{"boardId": rb.ID}).Decode(&list)
		if out := call("setListCollapsedState", `["`+rb.ID+`","`+list.ID+`",true]`); out["error"] != nil {
			t.Fatalf("negative: an ordinary collapse: %v", out)
		}
		expectQuiet(t, u, "an ordinary collapse")
		before := count("PrototypeBleed")
		for _, args := range []string{`["__proto__","polluted",true]`, `["` + rb.ID + `","constructor",true]`, `["` + rb.ID + `","` + list.ID + `:$where",true]`, `["a.b","x",true]`} {
			// The first is refused as invalid-map-key and disables the
			// account, so the rest are refused as signed out.
			if out := call("setListCollapsedState", args); out["error"] == nil {
				t.Errorf("%s was accepted: %v", args, out)
			}
		}
		expectRecorded(t, "PrototypeBleed", before, u)
		var polluted bson.M
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&polluted)
		if p, _ := polluted["profile"].(bson.M); p != nil {
			if cl, _ := p["collapsedLists"].(bson.M); cl["__proto__"] != nil || cl[rb.ID].(bson.M)["constructor"] != nil {
				t.Errorf("an unsafe key was written: %v", cl)
			}
		}
	})

	t.Run("RepointBleed", func(t *testing.T) {
		for _, attempt := range []func(rb restBoard) (string, string){
			func(rb restBoard) (string, string) {
				return "/triggers/update", `[{"_id":"t1"},{"$set":{"boardId":"*"}}]`
			},
			func(rb restBoard) (string, string) {
				return "/triggers/update", `[{"_id":"t1"},{"$unset":{"boardId":1}}]`
			},
			func(rb restBoard) (string, string) {
				return "/integrations/update", `[{"_id":"h1"},{"$set":{"boardId":"` + victim.ID + `"}}]`
			},
			func(rb restBoard) (string, string) {
				return "/rules/update", `[{"_id":"r1"},{"$rename":{"title":"boardId"}}]`
			},
			func(rb restBoard) (string, string) {
				return "/customFields/update", `[{"_id":"f1"},{"$push":{"boardIds":"` + victim.ID + `"}}]`
			},
			func(rb restBoard) (string, string) {
				return "/customFields/insert", `[{"boardIds":["` + rb.ID + `","` + victim.ID + `"],"name":"Planted","type":"text"}]`
			},
		} {
			u, rb, call := attacker()
			method, args := attempt(rb)
			before := count("RepointBleed")
			if out := call(method, args); out["error"] == nil {
				t.Errorf("%s %s was accepted: %v", method, args, out)
			}
			t.Run(method, func(t *testing.T) { expectRecorded(t, "RepointBleed", before, u) })
		}
		// Negative: a denied write that moves nothing is not an attack.
		u, rb, call := attacker()
		before := count("RepointBleed")
		call("/integrations/update", `[{"_id":"h1"},{"$set":{"title":"Renamed"}}]`)
		call("/customFields/update", `[{"_id":"f1"},{"$set":{"name":"Renamed"}}]`)
		call("/customFields/insert", `[{"boardIds":["`+rb.ID+`"],"name":"Mine","type":"text"}]`)
		call("/triggers/update", `[{"_id":"t1"},{"$set":{"listName":"x"}}]`)
		if count("RepointBleed") != before {
			t.Error("negative: an ordinary denied write was recorded as RepointBleed")
		}
		expectQuiet(t, u, "an ordinary denied write")
	})

	t.Run("BackgroundBleed", func(t *testing.T) {
		u, rb, call := attacker()
		own := "att" + randomID()[:12]
		_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": own, "name": "mine.png", "meta": bson.M{"boardId": rb.ID}})
		before := count("BackgroundBleed")
		call("/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"title":"Renamed"}}]`)
		call("/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"backgroundImageId":"`+own+`"}}]`)
		if count("BackgroundBleed") != before {
			t.Error("negative: a board's own image, or a rename, was recorded as BackgroundBleed")
		}
		expectQuiet(t, u, "a board's own image")
		if out := call("/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"backgroundImageId":"`+secret+`"}}]`); out["error"] == nil {
			t.Fatalf("another board's attachment became a background: %v", out)
		}
		expectRecorded(t, "BackgroundBleed", before, u)
		var b bson.M
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
		if b["backgroundImageId"] == secret {
			t.Error("the background was written")
		}
	})

	t.Run("HookBleed", func(t *testing.T) {
		u, rb, call := attacker()
		hook := "hook" + randomID()[:12]
		_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": hook, "boardId": rb.ID, "url": sink.srv.URL + "/chat",
			"type": "outgoing-webhooks", "enabled": true, "activities": bson.A{"all"}, "userId": u.ID})
		// Negative: the card-opened notice for the board's own card is sent,
		// built from the card, and costs nothing.
		before := count("HookBleed")
		if out := call("outgoingWebhooks", `[{"_id":"`+hook+`"},"CardSelected",{"cardId":"`+rb.Card+`","text":"ignored"}]`); out["error"] != nil {
			t.Fatalf("CardSelected: %v", out)
		}
		deadline := time.Now().Add(10 * time.Second)
		for len(sink.bodies("/chat")) == 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if got := sink.bodies("/chat"); len(got) != 1 || strings.Contains(toJSON(got[0]), "ignored") {
			t.Errorf("the card-opened notice: %v", got)
		}
		if count("HookBleed") != before {
			t.Error("negative: the card-opened notice was recorded as HookBleed")
		}
		expectQuiet(t, u, "the card-opened notice")
		// The forged message, through the hook named by url, and a card of another board.
		call("outgoingWebhooks", `[{"url":"`+sink.srv.URL+`/chat","boardId":"`+rb.ID+`","type":"bidirectional-webhooks"},"act-anything",{"text":"Your account is suspended, log in at https://phish.example"}]`)
		expectRecorded(t, "HookBleed", before, u)
		time.Sleep(200 * time.Millisecond)
		for _, b := range sink.bodies("/chat") {
			if strings.Contains(toJSON(b), "phish.example") {
				t.Error("the forged text was sent")
			}
		}
		var row bson.M
		_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "HookBleed"}).Decode(&row)
		if row["source"] != "outgoingWebhooks" {
			t.Errorf("source: %v", row["source"])
		}
		// Somebody who is not a member of the hook's board learns nothing and sends nothing.
		_, _, other := attacker()
		if out := other("outgoingWebhooks", `[{"_id":"`+hook+`"},"CardSelected",{"cardId":"`+rb.Card+`"}]`); out["result"] != nil {
			t.Errorf("a non-member: %v", out)
		}
	})
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestUnsafeMapKey(t *testing.T) {
	for _, k := range []string{"__proto__", "constructor", "prototype", "a.b", "$where", strings.Repeat("x", 257)} {
		if !unsafeMapKey(k) {
			t.Errorf("%q is safe", k)
		}
	}
	for _, k := range []string{"", "abc", "Ab_9-x", "list:lane"} {
		if unsafeMapKey(k) {
			t.Errorf("negative: %q is unsafe", k)
		}
	}
}

func TestModifierTouches(t *testing.T) {
	for _, c := range []struct {
		mod  string
		want bool
	}{
		{`{"$set":{"boardId":"x"}}`, true},
		{`{"$unset":{"boardId":1}}`, true},
		{`{"$set":{"boardId.x":1}}`, true},
		{`{"$rename":{"title":"boardId"}}`, true},
		{`{"boardId":"x"}`, true},
		{`{"$set":{"title":"x","boardIdx":1}}`, false},
		{`{"$rename":{"boardIdx":"title"}}`, false},
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(c.mod), &m)
		if modifierTouches(m, "boardId") != c.want {
			t.Errorf("%s: want %v", c.mod, c.want)
		}
	}
}

// Negative, over the whole package: a database path built from a variable
// either has a guard of these, or its value comes from a fixed list - the
// ones listed here, with why. A new one of a request's value fails until it
// is guarded or listed.
func TestNoUnguardedMapKeyPath(t *testing.T) {
	fixed := map[string]string{
		"api_board_views.go":           "boardViewSettings.<view>: isKnownBoardView, and showField's two names",
		"notification_delivery.go":     "notificationDelivery.<channel>: one of the channels normalizeChannelSettings knows",
		"server.go":                    "poker.<field>: pokerFields only",
		"admin_attachment_settings.go": "storageConfig.<provider>: only in error messages",
		"bots.go":                      "bot.<kind>: a webhook event name, not a database path",
		"import_attachments.go":        "import.<source>.attachment: a Problems source, not a database path",
	}
	files, _ := filepath.Glob("*.go")
	built := regexp.MustCompile(`"[A-Za-z_]+\.[A-Za-z_.]*"\s*\+\s*[A-Za-z]`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "securitylog.go" || f == "security_attempts.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if !built.MatchString(line) || strings.Contains(line, `"actors.`) {
				continue
			}
			if strings.Contains(line, "profile.collapsedLists.") {
				continue // checked below
			}
			if _, ok := fixed[f]; !ok {
				t.Errorf("%s builds a database path from a value: %s - guard it with refuseMapKeys, or list why it is fixed", f, strings.TrimSpace(line))
			}
		}
	}
	src, _ := os.ReadFile("methods.go")
	fn := string(src)
	i := strings.Index(fn, "func (s *Server) methodSetListCollapsedState(")
	j := strings.Index(fn[i:], "profile.collapsedLists.")
	if i < 0 || j < 0 || !strings.Contains(fn[i:i+j], "s.refuseMapKeys(") {
		t.Error("setListCollapsedState writes profile.collapsedLists.<id> without refuseMapKeys first")
	}
	if !strings.Contains(fn, "s.noteDeniedCollectionWrite(r, u, r.PathValue(\"name\"), denied)") {
		t.Error("the denied collection writes are no longer looked at for RepointBleed and BackgroundBleed")
	}
}

// ImportBleed: a logged-out importBoard is refused with WeKan's own error,
// before anything is created; a method WeKan names no other way keeps
// not-authorized (negative).
func TestLoggedOutMethodErrors(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	call := func(method, args string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := w.client(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	code, out := call("importBoard", `[{"title":"ImportBleed","lists":[],"cards":[]},{},"wekan",null]`)
	if code != 401 || out["error"] != "error-notAuthorized" {
		t.Errorf("importBoard logged out: %d %v", code, out)
	}
	if n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{"title": "ImportBleed"}); n != 0 {
		t.Error("a logged-out import created a board")
	}
	for method, want := range map[string]string{"setListCollapsedState": "not-logged-in", "cards.pokerVote": "not-authorized", "no-such-method": "not-authorized"} {
		if _, out := call(method, `[]`); out["error"] != want {
			t.Errorf("%s logged out: %v, want %s", method, out["error"], want)
		}
	}
}
