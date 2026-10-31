package audit

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/migrations"
)

func ev(action string) Event {
	return Event{Action: action, Outcome: OutcomeSuccess, Actor: "alice", ActorID: 1}
}

func TestDisabledRecorderWritesNothing(t *testing.T) {
	s := &MemoryStore{}
	r, err := NewRecorder(false, s)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Record(context.Background(), ev("x")); err != nil {
		t.Fatal(err)
	}
	if len(s.recs) != 0 {
		t.Fatal("disabled recorder wrote records")
	}
}

func TestEnabledRequiresStore(t *testing.T) {
	if _, err := NewRecorder(true, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestChainAndTamperDetection(t *testing.T) {
	ctx := context.Background()
	s := &MemoryStore{}
	r, _ := NewRecorder(true, s)
	for _, a := range []string{"a", "b", "c"} {
		if err := r.Record(ctx, ev(a)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := Verify(ctx, s); err != nil || n != 3 {
		t.Fatalf("verify n=%d err=%v", n, err)
	}
	if s.recs[0].PrevHash != GenesisHash || s.recs[1].PrevHash != s.recs[0].Hash {
		t.Fatal("chain not linked")
	}

	s.recs[1].Actor = "mallory"
	var broken *BrokenChainError
	if _, err := Verify(ctx, s); !errors.As(err, &broken) || broken.Seq != 2 {
		t.Fatalf("tamper not detected: %v", err)
	}
	s.recs[1].Actor = "alice"

	s.recs = append(s.recs[:1], s.recs[2:]...)
	if _, err := Verify(ctx, s); !errors.As(err, &broken) {
		t.Fatal("deletion not detected")
	}
}

func TestSensitiveDetailRedacted(t *testing.T) {
	s := &MemoryStore{}
	r, _ := NewRecorder(true, s)
	e := ev("x")
	e.Detail = map[string]string{"newPassword": "hunter2", "privateKeyPEM": "-----", "username": "bob"}
	_ = r.Record(context.Background(), e)
	d := s.recs[0].Detail
	if d["newPassword"] != "[redacted]" || d["privateKeyPEM"] != "[redacted]" || d["username"] != "bob" {
		t.Fatalf("detail = %v", d)
	}
}

func openSQLite(t *testing.T) store.Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := store.SQL{DB: db}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := store.Migrate(context.Background(), s, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSQLStoreAppendOnlyAndConcurrent(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	s := SQLStore{DB: db, Retries: 50}
	r, _ := NewRecorder(true, s)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			e := ev("concurrent")
			e.Detail = map[string]string{"k": "v"}
			if err := r.Record(ctx, e); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n, err := Verify(ctx, s); err != nil || n != 20 {
		t.Fatalf("verify n=%d err=%v", n, err)
	}
	newest, err := s.ListBefore(ctx, 0, 3)
	if err != nil || len(newest) != 3 || newest[0].Seq != 20 || newest[2].Seq != 18 {
		t.Fatalf("newest first: %v %+v", err, newest)
	}
	if older, _ := s.ListBefore(ctx, 18, 100); len(older) != 17 || older[0].Seq != 17 {
		t.Fatalf("paging back: %d", len(older))
	}
	if _, err := db.Exec(ctx, "UPDATE v2_audit_log SET actor = 'x' WHERE seq = 1"); err == nil {
		t.Fatal("update allowed")
	}
	// Only the oldest record may go: deleting from the middle or the end is refused.
	for _, seq := range []int{2, 20} {
		if _, err := db.Exec(ctx, "DELETE FROM v2_audit_log WHERE seq = ?", seq); err == nil {
			t.Fatalf("delete of seq %d allowed", seq)
		}
	}
	if _, err := store.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}
}

func TestSQLStoreRollsOver(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	s := SQLStore{DB: db, MaxRecords: 5}
	r, _ := NewRecorder(true, s)
	for range 12 {
		if err := r.Record(ctx, ev("x")); err != nil {
			t.Fatal(err)
		}
	}
	var n, lo, hi int64
	if err := store.QueryRow(ctx, db, "SELECT COUNT(*), MIN(seq), MAX(seq) FROM v2_audit_log", nil, &n, &lo, &hi); err != nil || n != 5 || lo != 8 || hi != 12 {
		t.Fatalf("kept n=%d seq %d..%d err=%v", n, lo, hi, err)
	}
	if got, err := Verify(ctx, s); err != nil || got != 12 {
		t.Fatalf("truncated chain verify n=%d err=%v", got, err)
	}

	// Lowering the limit trims oldest first, at most trimBatch rows per append.
	for range trimBatch + 10 {
		if err := r.Record(ctx, ev("y")); err != nil {
			t.Fatal(err)
		}
	}
	s.MaxRecords = 2
	r, _ = NewRecorder(true, s)
	for range 2 {
		if err := r.Record(ctx, ev("z")); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.QueryRow(ctx, db, "SELECT COUNT(*), MIN(seq) FROM v2_audit_log", nil, &n, &lo); err != nil || n != 2 || lo != hi+trimBatch+10+1 {
		t.Fatalf("after lowering: n=%d min=%d err=%v", n, lo, err)
	}
	if _, err := Verify(ctx, s); err != nil {
		t.Fatal(err)
	}
}
