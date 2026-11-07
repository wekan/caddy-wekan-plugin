package web

// Activities, as Node.js WeKan records them in the activities collection - what
// its activity feed, notifications and rules read. Recording never fails the
// change it records.

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func (s *Server) activity(ctx context.Context, userID, activityType string, fields bson.M) {
	now := time.Now().UTC()
	doc := bson.M{"_id": randomID(), "userId": userID, "activityType": activityType, "createdAt": now, "modifiedAt": now} // inserted, not a filter
	for k, v := range fields {
		doc[k] = v
	}
	if _, err := s.db.Collection("activities").InsertOne(ctx, doc); err != nil {
		s.log.Warn("recording an activity", "type", activityType, "error", err)
	}
}

// titleOfID is the title of a document, "" when there is none.
func (s *Server) titleOfID(ctx context.Context, coll, id string) string {
	var d struct {
		Title string `bson:"title"`
	}
	if !validID(id) {
		return ""
	}
	_ = s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&d)
	return d.Title
}
