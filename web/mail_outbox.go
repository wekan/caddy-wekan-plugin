package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"net/textproto"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The notification e-mail outbox, as Node.js WeKan's server/lib/emailOutbox.js:
// a notification is a durable job in notificationEmailJobs - the same
// collection and the same fields, so either server delivers what the other
// queued - and a background pass sends what is due, one e-mail per recipient
// and group, retrying a failure with WeKan's backoff
// (server/lib/emailRetryPolicy.js) until it is sent or failed for good.

const (
	maxEmailAttempts = 12
	maxDigestBytes   = 4 << 20
	maxJobBytes      = 15 << 20
)

// emailDelay is EMAIL_NOTIFICATION_TIMEOUT: how long an immediate
// notification waits for others of its recipient, 30 s unless set.
func emailDelay() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("EMAIL_NOTIFICATION_TIMEOUT")); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 30 * time.Second
}

// emailJobID is WeKan's idFor: sha256 of JSON.stringify([userId, eventId]).
func emailJobID(userID, eventID string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode([]string{userID, eventID})
	sum := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

type emailJob struct {
	UserID, EventID, Subject, HTML, Language string
	CardID, BoardID                          string
	Text, GroupKey                           *string
	DeliverAt                                *time.Time
}

// enqueueEmail stores a job once: the same event for the same recipient is
// one job, whoever queues it again.
func (s *Server) enqueueEmail(ctx context.Context, j emailJob) error {
	if j.UserID == "" || j.EventID == "" || j.Language == "" || strings.ContainsAny(j.Subject, "\r\n") || len(j.Subject) > 10000 ||
		(j.GroupKey != nil && (*j.GroupKey == "" || len(*j.GroupKey) > 300)) {
		return errors.New("invalid-email-job")
	}
	now := time.Now().UTC()
	due := now.Add(emailDelay())
	if j.DeliverAt != nil {
		due = *j.DeliverAt
		if due.Before(now) {
			due = now
		}
	}
	nullable := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	doc := bson.M{"_id": emailJobID(j.UserID, j.EventID), "userId": j.UserID, "eventId": j.EventID, "subject": j.Subject, // inserted, not a filter
		"html": j.HTML, "language": j.Language, "cardId": nullable(j.CardID), "boardId": nullable(j.BoardID), // inserted, not a filter
		"state": "pending", "attempts": 0, "cycleAttempts": 0, "createdAt": now, "nextAttemptAt": due}
	if j.Text != nil {
		doc["text"] = *j.Text
	}
	if j.GroupKey != nil {
		doc["groupKey"] = *j.GroupKey
	}
	if raw, err := bson.Marshal(doc); err != nil || len(raw) > maxJobBytes {
		return errors.New("email-job-too-large")
	}
	if _, err := s.db.Collection("notificationEmailJobs").InsertOne(ctx, doc); err != nil {
		// A duplicate event is the same job: never overwrite the first copy.
		var existing bson.M
		if s.db.Collection("notificationEmailJobs").FindOne(ctx, bson.M{"_id": doc["_id"]}).Decode(&existing) != nil {
			return err
		}
	}
	return nil
}

// runEmailOutbox sends what is due, a second at a time, until stop.
func (s *Server) runEmailOutbox(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			s.drainEmail(ctx)
			cancel()
		}
	}
}

// drainEmail is one pass: legacy profile buffers queued, then the recipients
// with something due, the longest waiting first.
func (s *Server) drainEmail(ctx context.Context) {
	if _, err := s.mailSettings(ctx, "localhost"); err != nil {
		return // no mail server: the jobs wait for one
	}
	s.migrateEmailBuffers(ctx)
	cur, err := s.db.Collection("notificationEmailJobs").Find(ctx, bson.M{"state": "pending", "nextAttemptAt": bson.M{"$lte": time.Now().UTC()}},
		options.Find().SetProjection(bson.M{"userId": 1}).SetSort(bson.D{{Key: "nextAttemptAt", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(1000))
	if err != nil {
		return
	}
	var due []struct {
		UserID string `bson:"userId"`
	}
	_ = cur.All(ctx, &due)
	seen := []string{}
	for _, d := range due {
		if d.UserID != "" && !slices.Contains(seen, d.UserID) && len(seen) < 100 {
			seen = append(seen, d.UserID)
		}
	}
	// Four recipients progress while another's SMTP server is slow, as
	// WeKan's drainBatch; each recipient's lease keeps out other senders, and
	// a pass stops when every send slot is taken.
	var mu sync.Mutex
	next, full := 0, false
	var wg sync.WaitGroup
	for range min(4, len(seen)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if full || next >= len(seen) {
					mu.Unlock()
					return
				}
				id := seen[next]
				next++
				mu.Unlock()
				if errors.Is(s.drainEmailRecipient(ctx, id), errEmailCapacityBusy) {
					mu.Lock()
					full = true
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
}

// errEmailCapacityBusy: all of WeKan's send slots are someone's.
var errEmailCapacityBusy = errors.New("email-capacity-busy")

// emailSendCapacity is WeKan's EMAIL_SEND_CAPACITY: the SMTP sessions all
// servers sharing the database have at once.
const emailSendCapacity = 4

// drainEmailRecipient delivers one recipient's due mail under its lease
// (notificationEmailLeases) and a send slot (notificationEmailSendSlots),
// claimed before any attempt is counted, so waiting for capacity costs none.
func (s *Server) drainEmailRecipient(ctx context.Context, userID string) error {
	return s.withLease(ctx, "notificationEmailLeases", userID, func() error {
		for slot := range emailSendCapacity {
			err := s.withLease(ctx, "notificationEmailSendSlots", "slot-"+strconv.Itoa(slot), func() error {
				s.drainEmailUser(ctx, userID)
				return nil
			})
			if !errors.Is(err, errLeaseBusy) {
				return err
			}
		}
		return errEmailCapacityBusy
	})
}

// migrateEmailBuffers is WeKan's migrateLegacyEmailBuffer: an older
// profile.emailBuffer's texts become jobs, removed once stored.
func (s *Server) migrateEmailBuffers(ctx context.Context) {
	cur, err := s.db.Collection("users").Find(ctx, bson.M{"profile.emailBuffer.0": bson.M{"$exists": true}},
		options.Find().SetProjection(bson.M{"profile.emailBuffer": 1, "profile.language": 1}).SetLimit(100))
	if err != nil {
		return
	}
	var users []struct {
		ID      string `bson:"_id"`
		Profile struct {
			Buffer   []string `bson:"emailBuffer"`
			Language string   `bson:"language"`
		} `bson:"profile"`
	}
	_ = cur.All(ctx, &users)
	for _, u := range users {
		lang := u.Profile.Language
		if lang == "" {
			lang = "en"
		}
		ok := true
		for _, text := range u.Profile.Buffer {
			if s.enqueueEmail(ctx, emailJob{UserID: u.ID, EventID: "legacy:" + emailJobID(u.ID, text), Subject: "WeKan", HTML: text, Language: lang}) != nil {
				ok = false
			}
		}
		if ok {
			// One plain value a pull: FerretDB's $pull matches no $in.
			for _, text := range u.Profile.Buffer {
				_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(u.ID)}, bson.M{"$pull": bson.M{"profile.emailBuffer": text}})
			}
		}
	}
}

// payloadUnset clears a finished job's content: the record of it stays.
var payloadUnset = bson.M{"html": "", "text": "", "subject": "", "language": "", "cardId": "", "nextAttemptAt": "", "lastFailure": ""}

func (s *Server) drainEmailUser(ctx context.Context, userID string) {
	jobs := s.db.Collection("notificationEmailJobs")
	// The administrator's controls (mail_recovery.go): what was cancelled is
	// gone, and a paused recipient's mail waits - out of the next pass's way.
	var control struct {
		Paused       bool      `bson:"paused"`
		CancelBefore time.Time `bson:"cancelBefore"`
	}
	_ = s.db.Collection("notificationEmailControls").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&control)
	if !control.CancelBefore.IsZero() {
		_, _ = jobs.UpdateMany(ctx, bson.M{"userId": eq(userID), "state": bson.M{"$in": bson.A{"pending", "failed"}}, "createdAt": bson.M{"$lte": control.CancelBefore}},
			bson.M{"$set": bson.M{"state": "cancelled", "finishedAt": time.Now().UTC()}, "$unset": emailPayloadFields})
	}
	if control.Paused {
		_, _ = jobs.UpdateMany(ctx, bson.M{"userId": eq(userID), "state": "pending"}, bson.M{"$max": bson.M{"nextAttemptAt": time.Now().UTC().Add(time.Minute)}})
		return
	}
	due := bson.M{"userId": eq(userID), "state": "pending", "nextAttemptAt": bson.M{"$lte": time.Now().UTC()}}
	if !control.CancelBefore.IsZero() {
		due["createdAt"] = bson.M{"$gt": control.CancelBefore}
	}
	cur, err := jobs.Find(ctx, due,
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(100))
	if err != nil {
		return
	}
	var all []bson.M
	_ = cur.All(ctx, &all)
	// One e-mail carries one group: the first job's, or the built-in
	// "everything due" when it has none. Other groups go in the next pass.
	var batch []bson.M
	size := 0
	key, keyed := "", false
	for i, j := range all {
		k, has := j["groupKey"].(string)
		if i == 0 {
			key, keyed = k, has
		} else if has != keyed || k != key {
			continue
		}
		n := len(strOf(j["html"]))
		if len(batch) > 0 && size+n > maxDigestBytes {
			break
		}
		batch, size = append(batch, j), size+n
	}
	if len(batch) == 0 {
		return
	}
	attemptID := randomID()
	reserved := []bson.M{}
	for _, j := range batch {
		prev := intOf(j["cycleAttempts"])
		if _, ok := j["cycleAttempts"]; !ok {
			prev = intOf(j["attempts"])
		}
		sel := bson.M{"_id": eq(strOf(j["_id"])), "userId": eq(userID), "state": "pending", "cycleAttempts": j["cycleAttempts"]}
		if prev >= maxEmailAttempts {
			_, _ = jobs.UpdateOne(ctx, sel, bson.M{"$set": bson.M{"state": "failed", "lastFailure": "retry-limit", "failedAt": time.Now().UTC()},
				"$unset": bson.M{"nextAttemptAt": ""}})
			continue
		}
		// The claim: only one pass may move this attempt count on.
		res, err := jobs.UpdateOne(ctx, sel, bson.M{"$set": bson.M{"cycleAttempts": prev + 1, "attemptId": attemptID, "lastAttemptAt": time.Now().UTC()}})
		if err != nil || res.ModifiedCount != 1 {
			continue
		}
		j["cycleAttempts"] = prev + 1
		reserved = append(reserved, j)
	}
	if len(reserved) == 0 {
		return
	}
	// finish ends this attempt's jobs, one by one.
	finish := func(js []bson.M, state string) {
		for _, j := range js {
			_, _ = jobs.UpdateOne(ctx, bson.M{"_id": eq(strOf(j["_id"])), "userId": eq(userID), "state": "pending", "attemptId": attemptID},
				bson.M{"$set": bson.M{"state": state, "finishedAt": time.Now().UTC()}, "$unset": payloadUnset})
		}
	}
	var u User
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&u) != nil {
		finish(reserved, "cancelled")
		return
	}
	// canReceive: still an active member of the board, still wanting mail.
	permitted, cancelled := []bson.M{}, []bson.M{}
	for _, j := range reserved {
		board := strOf(j["boardId"])
		ok := true
		if board != "" {
			var b Board
			ok = validID(board) && s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(board)}).Decode(&b) == nil
			if ok {
				m := b.member(userID)
				ok = m != nil && m.IsActive && s.emailNotificationsOn(ctx, userID, board)
			}
		}
		if ok {
			permitted = append(permitted, j)
		} else {
			cancelled = append(cancelled, j)
		}
	}
	finish(cancelled, "cancelled")
	if len(permitted) == 0 {
		return
	}
	phase := "preparation"
	var sendErr error
	if u.LoginDisabled || len(u.Emails) == 0 || u.Emails[0].Address == "" {
		sendErr = errRecipientUnavailable
	} else {
		htmls, texts := []string{}, []string{}
		allText := true
		for _, j := range permitted {
			htmls = append(htmls, strOf(j["html"]))
			if t, ok := j["text"].(string); ok {
				texts = append(texts, t)
			} else {
				allText = false
			}
		}
		m := mailMessage{To: []string{strings.ToLower(u.Emails[0].Address)}, Subject: strOf(permitted[0]["subject"]),
			HTML: strings.Join(htmls, "<br/>\n\n")}
		if allText {
			m.Text = strings.Join(texts, "\n\n")
		}
		phase = "smtp"
		sendErr = s.sendMail(ctx, "localhost", m)
	}
	if sendErr == nil {
		phase = "acknowledgement"
		finish(permitted, "sent")
		return
	}
	now := time.Now().UTC()
	for _, j := range permitted {
		attempts := intOf(j["attempts"]) + 1
		d := emailRetryDecision(sendErr, phase, intOf(j["cycleAttempts"]), now, rand.Float64())
		set := bson.M{"attempts": attempts, "state": d.state, "lastFailure": d.reason}
		unset := bson.M{"failedAt": ""}
		if d.state == "failed" {
			set["failedAt"] = now
			unset = bson.M{"nextAttemptAt": ""}
		} else {
			set["nextAttemptAt"] = d.next
		}
		_, _ = jobs.UpdateOne(ctx, bson.M{"_id": eq(strOf(j["_id"])), "userId": eq(userID), "state": "pending", "attemptId": attemptID},
			bson.M{"$set": set, "$unset": unset})
	}
	s.log.Warn("notification mail not sent", "reason", emailRetryDecision(sendErr, phase, 1, now, 0).reason)
}

var errRecipientUnavailable = errors.New("email-recipient-unavailable")

func intOf(v any) int {
	switch x := v.(type) {
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

type retryDecision struct {
	state, reason string
	next          time.Time
}

// emailRetryDecision is WeKan's: an SMTP 4xx or a network failure is tried
// again after 5 s, doubling, at most 48 min plus up to a quarter more; a 5xx,
// a refused login or a bad envelope fails for good, and so does the 12th try.
// No server reply text is kept - it can hold addresses or credentials.
func emailRetryDecision(err error, phase string, cycleAttempts int, now time.Time, random float64) retryDecision {
	reason, terminal := "delivery-failed", false
	var te *textproto.Error
	switch {
	case phase == "acknowledgement":
		reason = "acknowledgement-failed"
	case errors.Is(err, errRecipientUnavailable):
		reason = "recipient-unavailable"
	case phase != "smtp":
	case errors.As(err, &te) && te.Code >= 400 && te.Code < 500:
		reason = "smtp-temporary"
	case errors.As(err, &te) && te.Code >= 500 && te.Code < 600:
		reason, terminal = "smtp-rejected", true
		if te.Code == 535 || te.Code == 534 || te.Code == 530 {
			reason = "smtp-authentication"
		}
	case err != nil && (strings.Contains(err.Error(), "header") || strings.Contains(err.Error(), "address")):
		reason, terminal = "smtp-configuration", true
	}
	if terminal {
		return retryDecision{state: "failed", reason: reason}
	}
	if cycleAttempts >= maxEmailAttempts {
		return retryDecision{state: "failed", reason: "retry-limit"}
	}
	jitter := math.Max(0, math.Min(1, random))
	base := math.Min(2880000, 5000*math.Pow(2, math.Min(math.Max(0, float64(cycleAttempts-1)), 10)))
	return retryDecision{state: "pending", reason: reason, next: now.Add(time.Duration(math.Round(base*(1+0.25*jitter))) * time.Millisecond)}
}

// emailIndexes are WeKan's indexes of the outbox.
func (s *Server) emailIndexes(ctx context.Context) {
	_, _ = s.db.Collection("notificationEmailJobs").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "state", Value: 1}, {Key: "nextAttemptAt", Value: 1}, {Key: "_id", Value: 1}}},
		{Keys: bson.D{{Key: "userId", Value: 1}, {Key: "state", Value: 1}, {Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}},
	})
}
