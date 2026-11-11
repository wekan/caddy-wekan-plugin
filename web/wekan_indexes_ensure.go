package web

// WeKan's indexes on Weddy's database (#6892): the table cmd/sync-indexes
// copies from WeKan's ensureIndex calls (wekan_indexes.go), created at
// startup in the background, one at a time - an index already there, under
// WeKan's name or another, or one the database refuses, is left as it is,
// as WeKan's ensureIndex leaves it. Weddy's queries are WeKan's, so a
// database no WeKan ever started on scanned whole collections without them.

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type wekanIndex struct {
	coll string
	keys bson.D
	opts *options.IndexOptionsBuilder
}

// ensureWekanIndexes creates the indexes, and answers how many it could not.
func (s *Server) ensureWekanIndexes(ctx context.Context) int {
	failed := 0
	for _, ix := range wekanIndexes {
		model := mongo.IndexModel{Keys: ix.keys}
		if ix.opts != nil {
			model.Options = ix.opts
		}
		c, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if _, err := s.db.Collection(ix.coll).Indexes().CreateOne(c, model); err != nil {
			failed++
		}
		cancel()
	}
	return failed
}
