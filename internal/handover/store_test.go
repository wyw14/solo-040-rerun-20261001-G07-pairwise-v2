package handover

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-96/internal/model"
)

func sampleBook(t *testing.T, chamber string, status model.HandoverStatus, shiftStart time.Time) model.HandoverBook {
	t.Helper()
	shift := model.ShiftAt(shiftStart)
	snapshot := model.HandoverSnapshot{TakenAt: shiftStart, Shift: shift}
	book, err := model.NewHandoverBook(chamber, snapshot, shiftStart)
	if err != nil {
		t.Fatalf("new book: %v", err)
	}
	if status == model.HandoverConfirmed {
		if err := book.Confirm("out", "in", shift.End); err != nil {
			t.Fatalf("confirm: %v", err)
		}
	}
	return book
}

func runForStores(t *testing.T, test func(*testing.T, BookStore)) {
	t.Helper()
	stores := map[string]func() (BookStore, func()){
		"memory": func() (BookStore, func()) { return NewMemoryBookStore(), func() {} },
		"file": func() (BookStore, func()) {
			dir, err := os.MkdirTemp("", "handover-*")
			if err != nil {
				t.Fatalf("mkdir temp: %v", err)
			}
			store, err := NewFileBookStore(dir)
			if err != nil {
				t.Fatalf("file store: %v", err)
			}
			return store, func() { _ = os.RemoveAll(dir) }
		},
	}
	for name, factory := range stores {
		t.Run(name, func(t *testing.T) {
			store, cleanup := factory()
			defer cleanup()
			test(t, store)
		})
	}
}

func TestDraftCanBeRefreshed(t *testing.T) {
	runForStores(t, func(t *testing.T, store BookStore) {
		ctx := context.Background()
		start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
		book := sampleBook(t, "chamber-1", model.HandoverDraft, start)
		if err := store.SaveDraft(ctx, book); err != nil {
			t.Fatalf("first save: %v", err)
		}
		if err := book.AmendNotes("补充", "out", start.Add(time.Minute)); err != nil {
			t.Fatalf("amend: %v", err)
		}
		if err := store.SaveDraft(ctx, book); err != nil {
			t.Fatalf("draft refresh should be allowed, got %v", err)
		}
		got, ok, err := store.Get(ctx, book.ID)
		if err != nil || !ok {
			t.Fatalf("get = (%v,%v,%v)", got, ok, err)
		}
		if got.Notes != "补充" {
			t.Fatalf("notes = %q, want refreshed draft", got.Notes)
		}
	})
}

func TestConfirmIsWriteOnce(t *testing.T) {
	runForStores(t, func(t *testing.T, store BookStore) {
		ctx := context.Background()
		start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
		book := sampleBook(t, "chamber-1", model.HandoverConfirmed, start)
		if err := store.Confirm(ctx, book); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		// A second seal write for the same id must be rejected.
		if err := store.Confirm(ctx, book); err != model.ErrHandoverSealed {
			t.Fatalf("second confirm err = %v, want ErrHandoverSealed", err)
		}
		// The draft path cannot resurrect an editable copy of a sealed book.
		draft := book
		draft.Status = model.HandoverDraft
		draft.Seal = nil
		if err := store.SaveDraft(ctx, draft); err != model.ErrHandoverSealed {
			t.Fatalf("save draft over sealed err = %v, want ErrHandoverSealed", err)
		}
		got, ok, err := store.Get(ctx, book.ID)
		if err != nil || !ok {
			t.Fatalf("get sealed = (%v,%v,%v)", got, ok, err)
		}
		if got.Status != model.HandoverConfirmed {
			t.Fatalf("stored status = %q, want confirmed", got.Status)
		}
	})
}

func TestListFiltersByChamberTimeAndStatus(t *testing.T) {
	runForStores(t, func(t *testing.T, store BookStore) {
		ctx := context.Background()
		day := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
		night := time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC)
		nextDay := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
		mustStore := func(book model.HandoverBook) {
			t.Helper()
			var err error
			if book.Status == model.HandoverConfirmed {
				err = store.Confirm(ctx, book)
			} else {
				err = store.SaveDraft(ctx, book)
			}
			if err != nil {
				t.Fatalf("store book: %v", err)
			}
		}
		mustStore(sampleBook(t, "chamber-1", model.HandoverDraft, day))
		mustStore(sampleBook(t, "chamber-1", model.HandoverConfirmed, night))
		mustStore(sampleBook(t, "chamber-2", model.HandoverConfirmed, day))
		mustStore(sampleBook(t, "chamber-1", model.HandoverDraft, nextDay))

		assertCount := func(filter BookFilter, want int) {
			t.Helper()
			books, err := store.List(ctx, filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(books) != want {
				t.Fatalf("filter %+v returned %d books, want %d", filter, len(books), want)
			}
		}
		assertCount(BookFilter{}, 4)
		assertCount(BookFilter{ChamberID: "chamber-1"}, 3)
		assertCount(BookFilter{ChamberID: "chamber-1", Status: model.HandoverConfirmed}, 1)
		// [2026-09-29T20:00, 2026-09-30T20:00) covers only the night and next day.
		assertCount(BookFilter{
			From: time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC),
		}, 2)

		books, _ := store.List(ctx, BookFilter{})
		if !books[0].Shift.Start.After(books[len(books)-1].Shift.Start) {
			t.Fatal("list should be ordered newest shift first")
		}
	})
}

func TestFileStoreCreatesSealedRecordOnce(t *testing.T) {
	dir, err := os.MkdirTemp("", "handover-sealed-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store, err := NewFileBookStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	draft := sampleBook(t, "chamber-1", model.HandoverDraft, start)
	if err := store.SaveDraft(ctx, draft); err != nil {
		t.Fatal(err)
	}
	sealed := draft
	if err := sealed.Confirm("out", "in", start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Confirm(ctx, sealed); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "handovers", "chamber-1", "*.sealed.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("sealed files = %v (err=%v), want exactly 1", matches, err)
	}
	drafts, err := filepath.Glob(filepath.Join(dir, "handovers", "chamber-1", "*.draft.json"))
	if err != nil || len(drafts) != 0 {
		t.Fatalf("draft files after confirm = %v (err=%v), want none", drafts, err)
	}
}
