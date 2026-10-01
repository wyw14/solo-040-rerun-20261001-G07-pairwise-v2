package handoff

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrBookSealed is returned when an attempt is made to mutate a confirmed book.
var ErrBookSealed = errors.New("handover book is confirmed and cannot be modified")

// BookStore persists handover books. Implementations must preserve every
// confirmed revision: updating a confirmed book is rejected at the storage
// boundary so later state changes can never rewrite a sealed record.
type BookStore interface {
	Create(ctx context.Context, book HandoverBook) error
	Update(ctx context.Context, book HandoverBook) error
	Get(ctx context.Context, id uuid.UUID) (HandoverBook, bool, error)
	// List returns books whose shift window overlaps [from, to]. A zero
	// chamberID lists every chamber; zero times remove that bound.
	List(ctx context.Context, chamberID string, from, to time.Time) ([]HandoverBook, error)
	Chambers(ctx context.Context) ([]string, error)
}

// MemoryBookStore keeps books in memory, ordered by creation per chamber.
type MemoryBookStore struct {
	mu    sync.RWMutex
	byID  map[uuid.UUID]HandoverBook
	order []uuid.UUID
}

func NewMemoryBookStore() *MemoryBookStore {
	return &MemoryBookStore{byID: make(map[uuid.UUID]HandoverBook)}
}

func (s *MemoryBookStore) Create(ctx context.Context, book HandoverBook) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if book.ID == uuid.Nil {
		return errors.New("book id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[book.ID]; exists {
		return errors.New("handover book already exists")
	}
	s.byID[book.ID] = cloneBook(book)
	s.order = append(s.order, book.ID)
	return nil
}

func (s *MemoryBookStore) Update(ctx context.Context, book HandoverBook) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[book.ID]
	if !ok {
		return errors.New("handover book not found")
	}
	if !existing.Open() {
		return ErrBookSealed
	}
	s.byID[book.ID] = cloneBook(book)
	return nil
}

func (s *MemoryBookStore) Get(ctx context.Context, id uuid.UUID) (HandoverBook, bool, error) {
	if err := ctx.Err(); err != nil {
		return HandoverBook{}, false, err
	}
	s.mu.RLock()
	book, ok := s.byID[id]
	s.mu.RUnlock()
	return cloneBook(book), ok, nil
}

func (s *MemoryBookStore) List(ctx context.Context, chamberID string, from, to time.Time) ([]HandoverBook, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]HandoverBook, 0)
	for _, id := range s.order {
		book := s.byID[id]
		if chamberID != "" && book.ChamberID != chamberID {
			continue
		}
		if !windowOverlaps(book.WindowStart, book.WindowEnd, from, to) {
			continue
		}
		result = append(result, cloneBook(book))
	}
	sortBooks(result)
	return result, nil
}

func (s *MemoryBookStore) Chambers(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	seen := make(map[string]struct{}, len(s.order))
	for _, book := range s.byID {
		seen[book.ChamberID] = struct{}{}
	}
	s.mu.RUnlock()
	result := make([]string, 0, len(seen))
	for chamberID := range seen {
		result = append(result, chamberID)
	}
	sort.Strings(result)
	return result, nil
}

func windowOverlaps(start, end, from, to time.Time) bool {
	if !from.IsZero() && end.Before(from) {
		return false
	}
	if !to.IsZero() && start.After(to) {
		return false
	}
	return true
}

func sortBooks(books []HandoverBook) {
	sort.Slice(books, func(i, j int) bool {
		if books[i].WindowStart.Equal(books[j].WindowStart) {
			if books[i].ChamberID == books[j].ChamberID {
				return books[i].CreatedAt.Before(books[j].CreatedAt)
			}
			return books[i].ChamberID < books[j].ChamberID
		}
		return books[i].WindowStart.Before(books[j].WindowStart)
	})
}
