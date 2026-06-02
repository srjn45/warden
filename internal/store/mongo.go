package store

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type MongoStore struct {
	client *mongo.Client
	active *mongo.Collection
	closed *mongo.Collection
}

func NewMongoStore(ctx context.Context, uri, db string) (*MongoStore, error) {
	cl, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := cl.Ping(ctx, nil); err != nil {
		return nil, err
	}
	d := cl.Database(db)
	return &MongoStore{client: cl, active: d.Collection("sessions"), closed: d.Collection("closed")}, nil
}

func (m *MongoStore) Insert(ctx context.Context, s *Session) error {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	if s.Events == nil {
		s.Events = []Event{}
	}
	_, err := m.active.InsertOne(ctx, s)
	if mongo.IsDuplicateKeyError(err) {
		return ErrExists
	}
	return err
}

func (m *MongoStore) Get(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := m.active.FindOne(ctx, bson.M{"_id": id}).Decode(&s)
	if err == mongo.ErrNoDocuments {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *MongoStore) List(ctx context.Context) ([]*Session, error) {
	cur, err := m.active.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*Session
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *MongoStore) setUpdated(set bson.M) bson.M {
	set["updated_at"] = time.Now().UTC()
	return set
}

func (m *MongoStore) UpdateStatus(ctx context.Context, id string, status Status) error {
	res, err := m.active.UpdateByID(ctx, id, bson.M{"$set": m.setUpdated(bson.M{"status": status})})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateStatusIf sets status to next only when the stored status still equals
// expected (a compare-and-swap on a filtered UpdateOne). It returns true when
// the swap matched a document, false when it did not (status already changed,
// or the doc was archived/deleted) — the latter is not an error.
func (m *MongoStore) UpdateStatusIf(ctx context.Context, id string, expected, next Status) (bool, error) {
	res, err := m.active.UpdateOne(ctx,
		bson.M{"_id": id, "status": expected},
		bson.M{"$set": m.setUpdated(bson.M{"status": next})},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (m *MongoStore) UpdateType(ctx context.Context, id string, t Type) error {
	res, err := m.active.UpdateByID(ctx, id, bson.M{"$set": m.setUpdated(bson.M{"type": t})})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *MongoStore) UpdateSubject(ctx context.Context, id, subject string) error {
	res, err := m.active.UpdateByID(ctx, id, bson.M{"$set": m.setUpdated(bson.M{"subject": subject})})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *MongoStore) AppendEvent(ctx context.Context, id string, ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	res, err := m.active.UpdateByID(ctx, id, bson.M{
		"$push": bson.M{"events": ev},
		"$set":  bson.M{"updated_at": time.Now().UTC()},
	})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendEventStatus appends ev and optionally sets status in one UpdateByID, so
// the two writes share a round-trip and apply atomically. An empty status only
// appends the event (and bumps updated_at).
func (m *MongoStore) AppendEventStatus(ctx context.Context, id string, ev Event, status Status) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	set := bson.M{"updated_at": time.Now().UTC()}
	if status != "" {
		set["status"] = status
	}
	res, err := m.active.UpdateByID(ctx, id, bson.M{
		"$push": bson.M{"events": ev},
		"$set":  set,
	})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *MongoStore) UpdatePane(ctx context.Context, id, excerpt string) error {
	res, err := m.active.UpdateByID(ctx, id, bson.M{"$set": m.setUpdated(bson.M{"last_pane_excerpt": excerpt})})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *MongoStore) Archive(ctx context.Context, id string) error {
	s, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if _, err := m.closed.InsertOne(ctx, s); err != nil {
		return err
	}
	_, err = m.active.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

func (m *MongoStore) Delete(ctx context.Context, id string) error {
	res, err := m.active.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *MongoStore) Ping(ctx context.Context) error { return m.client.Ping(ctx, nil) }
func (m *MongoStore) Close(ctx context.Context) error { return m.client.Disconnect(ctx) }
