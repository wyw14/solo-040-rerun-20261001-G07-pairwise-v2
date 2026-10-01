package handover

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

// BookFilter narrows a listing by chamber, watch window and status.
type BookFilter struct {
	ChamberID string
	From      time.Time
	To        time.Time
	Status    model.HandoverStatus
}

// BookStore persists watch handover books. Drafts may be replaced; a confirmed
// book is write-once and can never be overwritten.
type BookStore interface {
	SaveDraft(context.Context, model.HandoverBook) error
	Confirm(context.Context, model.HandoverBook) error
	Get(context.Context, uuid.UUID) (model.HandoverBook, bool, error)
	List(context.Context, BookFilter) ([]model.HandoverBook, error)
}

func matchesFilter(book model.HandoverBook, filter BookFilter) bool {
	if filter.ChamberID != "" && book.ChamberID != filter.ChamberID {
		return false
	}
	if filter.Status != "" && book.Status != filter.Status {
		return false
	}
	start := book.Shift.Start.UTC()
	if !filter.From.IsZero() && start.Before(filter.From.UTC()) {
		return false
	}
	if !filter.To.IsZero() && !start.Before(filter.To.UTC()) {
		return false
	}
	return true
}

// MemoryBookStore is the non-durable implementation used without a data dir.
type MemoryBookStore struct {
	mu    sync.RWMutex
	books map[uuid.UUID]model.HandoverBook
}

func NewMemoryBookStore() *MemoryBookStore {
	return &MemoryBookStore{books: make(map[uuid.UUID]model.HandoverBook)}
}

func (s *MemoryBookStore) SaveDraft(_ context.Context, book model.HandoverBook) error {
	if book.Status != model.HandoverDraft {
		return errors.New("only draft books can be saved as drafts")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.books[book.ID]; ok && existing.Status == model.HandoverConfirmed {
		return model.ErrHandoverSealed
	}
	s.books[book.ID] = book
	return nil
}

func (s *MemoryBookStore) Confirm(_ context.Context, book model.HandoverBook) error {
	if book.Status != model.HandoverConfirmed || book.Seal == nil {
		return errors.New("book must be confirmed and sealed before persistence")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.books[book.ID]; ok && existing.Status == model.HandoverConfirmed {
		return model.ErrHandoverSealed
	}
	s.books[book.ID] = book
	return nil
}

func (s *MemoryBookStore) Get(_ context.Context, id uuid.UUID) (model.HandoverBook, bool, error) {
	s.mu.RLock()
	book, ok := s.books[id]
	s.mu.RUnlock()
	return book, ok, nil
}

func (s *MemoryBookStore) List(_ context.Context, filter BookFilter) ([]model.HandoverBook, error) {
	s.mu.RLock()
	result := make([]model.HandoverBook, 0, len(s.books))
	for _, book := range s.books {
		if matchesFilter(book, filter) {
			result = append(result, book)
		}
	}
	s.mu.RUnlock()
	sortBooks(result)
	return result, nil
}

func sortBooks(books []model.HandoverBook) {
	sort.Slice(books, func(i, j int) bool {
		if books[i].Shift.Start.Equal(books[j].Shift.Start) {
			if books[i].ChamberID == books[j].ChamberID {
				return books[i].ID.String() < books[j].ID.String()
			}
			return books[i].ChamberID < books[j].ChamberID
		}
		return books[i].Shift.Start.After(books[j].Shift.Start)
	})
}

// FileBookStore keeps one draft and one sealed record per book on disk.
// Sealed records are created with O_EXCL so a confirmed handover is immutable.
type FileBookStore struct {
	root string
	mu   sync.Mutex
}

func NewFileBookStore(root string) (*FileBookStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("handover store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "handovers"), 0o755); err != nil {
		return nil, err
	}
	return &FileBookStore{root: abs}, nil
}

func safeID(chamberID string) (string, error) {
	if chamberID == "" || strings.ContainsAny(chamberID, `/\\.`) {
		return "", fmt.Errorf("invalid chamber id %q", chamberID)
	}
	return chamberID, nil
}

func (s *FileBookStore) chamberDir(chamberID string) (string, error) {
	name, err := safeID(chamberID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, "handovers", name), nil
}

func (s *FileBookStore) draftPath(book model.HandoverBook) (string, error) {
	dir, err := s.chamberDir(book.ChamberID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, book.ID.String()+".draft.json"), nil
}

func (s *FileBookStore) sealedPath(book model.HandoverBook) (string, error) {
	dir, err := s.chamberDir(book.ChamberID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, book.ID.String()+".sealed.json"), nil
}

func writeAtomic(path string, value model.HandoverBook) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".new"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (s *FileBookStore) SaveDraft(ctx context.Context, book model.HandoverBook) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if book.Status != model.HandoverDraft {
		return errors.New("only draft books can be saved as drafts")
	}
	path, err := s.draftPath(book)
	if err != nil {
		return err
	}
	sealed, err := s.sealedPath(book)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, statErr := os.Stat(sealed); statErr == nil {
		return model.ErrHandoverSealed
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, book)
}

func (s *FileBookStore) Confirm(ctx context.Context, book model.HandoverBook) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if book.Status != model.HandoverConfirmed || book.Seal == nil {
		return errors.New("book must be confirmed and sealed before persistence")
	}
	draftPath, err := s.draftPath(book)
	if err != nil {
		return err
	}
	sealedPath, err := s.sealedPath(book)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(book, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(sealedPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(sealedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return model.ErrHandoverSealed
		}
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Remove(draftPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readBook(path string) (model.HandoverBook, error) {
	file, err := os.Open(path)
	if err != nil {
		return model.HandoverBook{}, err
	}
	defer file.Close()
	var book model.HandoverBook
	if err := json.NewDecoder(bufio.NewReader(file)).Decode(&book); err != nil {
		return model.HandoverBook{}, err
	}
	return book, nil
}

func (s *FileBookStore) Get(ctx context.Context, id uuid.UUID) (model.HandoverBook, bool, error) {
	if err := ctx.Err(); err != nil {
		return model.HandoverBook{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root := filepath.Join(s.root, "handovers")
	chambers, err := listChamberDirs(root)
	if err != nil {
		return model.HandoverBook{}, false, err
	}
	for _, chamber := range chambers {
		dir := filepath.Join(root, chamber)
		sealed := filepath.Join(dir, id.String()+".sealed.json")
		if book, err := readBook(sealed); err == nil {
			return book, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return model.HandoverBook{}, false, err
		}
		draft := filepath.Join(dir, id.String()+".draft.json")
		if book, err := readBook(draft); err == nil {
			return book, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return model.HandoverBook{}, false, err
		}
	}
	return model.HandoverBook{}, false, nil
}

func listChamberDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var chambers []string
	for _, entry := range entries {
		if entry.IsDir() {
			chambers = append(chambers, entry.Name())
		}
	}
	sort.Strings(chambers)
	return chambers, nil
}

func (s *FileBookStore) List(ctx context.Context, filter BookFilter) ([]model.HandoverBook, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root := filepath.Join(s.root, "handovers")
	chambers, err := listChamberDirs(root)
	if err != nil {
		return nil, err
	}
	var result []model.HandoverBook = make([]model.HandoverBook, 0)
	for _, chamber := range chambers {
		entries, err := os.ReadDir(filepath.Join(root, chamber))
		if err != nil {
			return nil, err
		}
		// Prefer the sealed record when a stale draft and a seal coexist.
		drafts := make(map[uuid.UUID]string)
		sealed := make(map[uuid.UUID]string)
		for _, entry := range entries {
			name := entry.Name()
			switch {
			case strings.HasSuffix(name, ".sealed.json"):
				if id, err := uuid.Parse(strings.TrimSuffix(name, ".sealed.json")); err == nil {
					sealed[id] = filepath.Join(root, chamber, name)
				}
			case strings.HasSuffix(name, ".draft.json"):
				if id, err := uuid.Parse(strings.TrimSuffix(name, ".draft.json")); err == nil {
					drafts[id] = filepath.Join(root, chamber, name)
				}
			}
		}
		load := func(path string) error {
			book, err := readBook(path)
			if err != nil {
				return err
			}
			if matchesFilter(book, filter) {
				result = append(result, book)
			}
			return nil
		}
		for _, path := range sealed {
			if err := load(path); err != nil {
				return nil, err
			}
		}
		for id, path := range drafts {
			if _, isSealed := sealed[id]; isSealed {
				continue
			}
			if err := load(path); err != nil {
				return nil, err
			}
		}
	}
	sortBooks(result)
	return result, nil
}

// NewRuntimeBookStore selects the durable file store or an in-memory one.
func NewRuntimeBookStore(dataDirectory string) (BookStore, error) {
	if dataDirectory == "" {
		return NewMemoryBookStore(), nil
	}
	return NewFileBookStore(dataDirectory)
}
