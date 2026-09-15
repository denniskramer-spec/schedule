package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mongoStore keeps tasks in a MongoDB server, for a board shared between
// machines. It is only used when asked for with -mongo or MONGODB_URI; see
// openData in db.go.
type mongoStore struct {
	client *mongo.Client
	tasks  *mongo.Collection
	prefs  *mongo.Collection
	label  string
}

const (
	localMongoURI = "mongodb://localhost:27017"
	defaultDB     = "schedule"
)

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), opTimeout)
}

// mongoURI is the connection string asked for: -mongo flag, then MONGODB_URI.
// Empty means keep tasks in a file.
func mongoURI() string {
	if v := argValue("mongo"); v != "" {
		return v
	}
	return os.Getenv("MONGODB_URI")
}

// dbNameFromURI pulls the database out of a connection string, so a URI like
// mongodb+srv://host/mydata still lands in the right place. Empty if absent.
func dbNameFromURI(uri string) string {
	s := uri
	for _, p := range []string{"mongodb+srv://", "mongodb://"} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			s = rest
			break
		}
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	i := strings.IndexByte(s, '/')
	if i < 0 {
		return ""
	}
	return s[i+1:]
}

// dbName is the database to use: MONGODB_DB, then the URI, then "schedule".
func dbName(uri string) string {
	if v := os.Getenv("MONGODB_DB"); v != "" {
		return v
	}
	if v := dbNameFromURI(uri); v != "" {
		return v
	}
	return defaultDB
}

// hostLabel strips any credentials so the URI is safe to print.
func hostLabel(uri string) string {
	s := uri
	for _, p := range []string{"mongodb+srv://", "mongodb://"} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			s = rest
			break
		}
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:] // drop user:password
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// openMongo connects, verifies the server is actually reachable, and makes
// sure the indexes the board relies on exist.
func openMongo(uri string, wait time.Duration) (*mongoStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	opts := options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(wait).
		SetAppName("Schedule")

	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, err
	}
	// Connect is lazy; this is what actually proves the server is there.
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(context.Background())
		return nil, err
	}

	name := dbName(uri)
	db := client.Database(name)
	st := &mongoStore{
		client: client,
		tasks:  db.Collection("tasks"),
		prefs:  db.Collection("prefs"),
		label:  "MongoDB · " + name + " @ " + hostLabel(uri),
	}

	// The board reads a day, a week or a month at a time, always by date;
	// carrying and the end-of-day prompt ask which tasks run out on a day.
	_, err = st.tasks.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "date", Value: 1}}},
		{Keys: bson.D{{Key: "end", Value: 1}}},
	})
	if err != nil {
		client.Disconnect(context.Background())
		return nil, err
	}

	if n, err := st.migrate(); err != nil {
		client.Disconnect(context.Background())
		return nil, err
	} else if n > 0 {
		log.Printf("brought %d task(s) forward from an older layout", n)
	}
	return st, nil
}

// waitForMongo keeps trying to reach MongoDB until it answers or the time is
// up, and returns the last error in that case.
func waitForMongo(uri string, limit time.Duration) (*mongoStore, error) {
	deadline := time.Now().Add(limit)
	for {
		log.Print("MongoDB is not answering yet; trying again shortly")
		time.Sleep(5 * time.Second)
		db, err := openMongo(uri, opTimeout)
		if err == nil || time.Now().After(deadline) {
			return db, err
		}
	}
}

// migrate brings documents written by older versions up to the current shape:
// the one comment a task used to hold becomes the first entry in its history,
// and a task with no last day gets one equal to its first, which is what a
// one-day task has always meant. Both are safe to run on every start, because
// a document that has already been converted no longer matches.
func (s *mongoStore) migrate() (int, error) {
	ctx, cancel := opCtx()
	defer cancel()

	cur, err := s.tasks.Find(ctx, bson.M{"$or": bson.A{
		bson.M{"end": bson.M{"$in": bson.A{"", nil}}},
		bson.M{
			"note": bson.M{"$nin": bson.A{"", nil}},
			"log":  bson.M{"$exists": false},
		},
	}})
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)

	var models []mongo.WriteModel
	for cur.Next(ctx) {
		var t Task
		if err := cur.Decode(&t); err != nil {
			return 0, err
		}
		set, unset := bson.M{}, bson.M{}
		if t.End == "" {
			set["end"] = t.Date
		}
		if t.Note != "" && t.Log == nil {
			// The old noteAt was whatever the browser's toLocaleString()
			// produced, so it is only usable if it is in our own format.
			at := t.NoteAt
			if _, err := time.Parse(stamp, at); err != nil {
				at = t.Date
			}
			set["log"] = []Entry{{
				Kind: "note", Date: t.Date, At: at, Text: t.Note, Status: t.Status,
			}}
			unset["note"], unset["noteAt"] = "", ""
		}
		if len(set) == 0 && len(unset) == 0 {
			continue
		}
		update := bson.M{}
		if len(set) > 0 {
			update["$set"] = set
		}
		if len(unset) > 0 {
			update["$unset"] = unset
		}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": t.ID}).
			SetUpdate(update))
	}
	if err := cur.Err(); err != nil {
		return 0, err
	}
	if len(models) == 0 {
		return 0, nil
	}
	res, err := s.tasks.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return 0, err
	}
	return int(res.ModifiedCount), nil
}

func (s *mongoStore) CarryForward(today, at string) (int, error) {
	ctx, cancel := opCtx()
	defer cancel()

	cur, err := s.tasks.Find(ctx, bson.M{
		"end":    bson.M{"$lt": today},
		"status": bson.M{"$in": bson.A{"todo", "doing"}},
	})
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)

	var models []mongo.WriteModel
	for cur.Next(ctx) {
		var t Task
		if err := cur.Decode(&t); err != nil {
			return 0, err
		}
		set := bson.M{"end": today}
		if t.Date == t.End {
			set["date"] = today
		}
		e := Entry{Kind: "carry", Date: today, At: at, From: t.End, Status: t.Status}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": t.ID}).
			SetUpdate(bson.M{
				"$set": set,
				"$push": bson.M{"log": bson.M{
					"$each": bson.A{e}, "$slice": -maxLogLen,
				}},
			}))
	}
	if err := cur.Err(); err != nil {
		return 0, err
	}
	if len(models) == 0 {
		return 0, nil
	}
	res, err := s.tasks.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return 0, err
	}
	return int(res.ModifiedCount), nil
}

// AddRepeats extends every repeating task up to horizon; see repeat.go.
func (s *mongoStore) AddRepeats(today, horizon, now string) (int, error) {
	all, err := s.ListTasks()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, head := range all {
		if head.Repeat == "" {
			continue
		}
		inst, through := newInstances(head, today, horizon, now)
		for _, t := range inst {
			err := s.InsertTask(t)
			if errors.Is(err, errDuplicate) {
				continue
			}
			if err != nil {
				return n, err
			}
			n++
		}
		if through != head.RepeatedThrough {
			head.RepeatedThrough = through
			if err := s.ReplaceTask(head.ID, head); err != nil && !errors.Is(err, errNoRow) {
				return n, err
			}
		}
	}
	return n, nil
}

// Fingerprint hashes the whole board, so another machine's writes to the
// same server show up as a change.
func (s *mongoStore) Fingerprint() (string, error) {
	tasks, err := s.ListTasks()
	if err != nil {
		return "", err
	}
	p, err := s.GetPrefs()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(fileData{Version: 1, Prefs: p, AlertDay: s.GetAlertDay(), Tasks: tasks})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *mongoStore) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.client.Disconnect(ctx)
}

func (s *mongoStore) Label() string { return s.label }

func (s *mongoStore) ListTasks() ([]Task, error) {
	ctx, cancel := opCtx()
	defer cancel()

	opts := options.Find().SetSort(bson.D{
		{Key: "date", Value: 1},
		{Key: "createdAt", Value: 1},
	})
	cur, err := s.tasks.Find(ctx, bson.D{}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	out := []Task{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Log == nil {
			out[i].Log = []Entry{}
		}
	}
	return out, nil
}

func (s *mongoStore) GetTask(id string) (Task, error) {
	ctx, cancel := opCtx()
	defer cancel()

	var t Task
	err := s.tasks.FindOne(ctx, bson.M{"_id": id}).Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return t, errNoRow
	}
	if t.Log == nil {
		t.Log = []Entry{}
	}
	return t, err
}

func (s *mongoStore) InsertTask(t Task) error {
	ctx, cancel := opCtx()
	defer cancel()

	t.Log = trimLog(t.Log)
	_, err := s.tasks.InsertOne(ctx, t)
	if mongo.IsDuplicateKeyError(err) {
		return errDuplicate
	}
	return err
}

func (s *mongoStore) ReplaceTask(id string, t Task) error {
	ctx, cancel := opCtx()
	defer cancel()

	t.ID = id
	t.Log = trimLog(t.Log)
	res, err := s.tasks.ReplaceOne(ctx, bson.M{"_id": id}, t)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return errNoRow
	}
	return nil
}

func (s *mongoStore) DeleteTask(id string) error {
	ctx, cancel := opCtx()
	defer cancel()

	res, err := s.tasks.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return errNoRow
	}
	return nil
}

func (s *mongoStore) DeleteAllTasks() error {
	ctx, cancel := opCtx()
	defer cancel()

	_, err := s.tasks.DeleteMany(ctx, bson.D{})
	return err
}

// prefs live in one document so reading them is a single lookup; the day the
// deadline last fired sits in another.
const (
	prefsID = "prefs"
	stateID = "state"
)

func (s *mongoStore) GetPrefs() (Prefs, error) {
	ctx, cancel := opCtx()
	defer cancel()

	p := defaultPrefs()
	p.Categories = nil // so a saved record's categories are not appended to
	err := s.prefs.FindOne(ctx, bson.M{"_id": prefsID}).Decode(&p)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return p, err
	}
	p.normalize()
	return p, nil
}

func (s *mongoStore) SetPrefs(p Prefs) error {
	ctx, cancel := opCtx()
	defer cancel()

	_, err := s.prefs.ReplaceOne(ctx, bson.M{"_id": prefsID}, bson.M{
		"ask": p.Ask, "carry": p.Carry, "deadline": p.Deadline,
		"cats": p.Categories, "dayStart": p.DayStart,
	}, options.Replace().SetUpsert(true))
	return err
}

func (s *mongoStore) GetAlertDay() string {
	ctx, cancel := opCtx()
	defer cancel()
	var st struct {
		AlertDay string `bson:"alertDay"`
	}
	s.prefs.FindOne(ctx, bson.M{"_id": stateID}).Decode(&st)
	return st.AlertDay
}

func (s *mongoStore) SetAlertDay(d string) error {
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.prefs.UpdateOne(ctx, bson.M{"_id": stateID},
		bson.M{"$set": bson.M{"alertDay": d}}, options.Update().SetUpsert(true))
	return err
}

func (s *mongoStore) ReplaceAll(tasks []Task, p Prefs, alertDay string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*opTimeout)
	defer cancel()
	if _, err := s.tasks.DeleteMany(ctx, bson.D{}); err != nil {
		return err
	}
	if len(tasks) > 0 {
		docs := make([]any, 0, len(tasks))
		for _, t := range tasks {
			t.Log = trimLog(t.Log)
			docs = append(docs, t)
		}
		if _, err := s.tasks.InsertMany(ctx, docs); err != nil {
			return err
		}
	}
	if err := s.SetPrefs(p); err != nil {
		return err
	}
	return s.SetAlertDay(alertDay)
}

func (s *mongoStore) SetCategory(ids []string, category int) error {
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.tasks.UpdateMany(ctx, bson.M{"_id": bson.M{"$in": ids}},
		bson.M{"$set": bson.M{"category": category}})
	return err
}
