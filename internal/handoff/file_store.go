package handoff

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
)

// FileBookStore keeps one append-only book log per chamber, mirroring the
// journal layout. A confirmed book is written once and every later revision
// is rejected, so sealed handovers are tamper-evident on disk as well.
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

func safeID(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, `/\\.`) {
		return "", fmt.Errorf("invalid id %q", value)
	}
	return value, nil
}

func (s *FileBookStore) bookPath(chamberID string) (string, error) {
	name, err := safeID(chamberID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, "handovers", name+".jsonl"), nil
}

type storedRevision struct {
	Book HandoverBook
	Line int
}

func (s *FileBookStore) Create(ctx context.Context, book HandoverBook) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if book.ID == uuid.Nil {
		return errors.New("book id is required")
	}
	path, err := s.bookPath(book.ChamberID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	revisions, err := readBooks(path)
	if err != nil {
		return err
	}
	for _, revision := range revisions {
		if revision.Book.ID == book.ID {
			return errors.New("handover book already exists")
		}
	}
	return appendBook(path, book)
}

func (s *FileBookStore) Update(ctx context.Context, book HandoverBook) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.bookPath(book.ChamberID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	revisions, err := readBooks(path)
	if err != nil {
		return err
	}
	var found bool
	for _, revision := range revisions {
		if revision.Book.ID != book.ID {
			continue
		}
		found = true
		if !revision.Book.Open() {
			return ErrBookSealed
		}
	}
	if !found {
		return errors.New("handover book not found")
	}
	return appendBook(path, book)
}

func (s *FileBookStore) Get(ctx context.Context, id uuid.UUID) (HandoverBook, bool, error) {
	if err := ctx.Err(); err != nil {
		return HandoverBook{}, false, err
	}
	chambers, err := s.chamberIDs()
	if err != nil {
		return HandoverBook{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, chamberID := range chambers {
		path, err := s.bookPath(chamberID)
		if err != nil {
			return HandoverBook{}, false, err
		}
		revisions, err := readBooks(path)
		if err != nil {
			return HandoverBook{}, false, err
		}
		if latest, ok := latestRevision(revisions, id); ok {
			return cloneBook(latest), true, nil
		}
	}
	return HandoverBook{}, false, nil
}

func (s *FileBookStore) List(ctx context.Context, chamberID string, from, to time.Time) ([]HandoverBook, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	chamberIDs := []string{chamberID}
	if chamberID == "" {
		var err error
		chamberIDs, err = s.chamberIDs()
		if err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]HandoverBook, 0)
	for _, id := range chamberIDs {
		path, err := s.bookPath(id)
		if err != nil {
			return nil, err
		}
		revisions, err := readBooks(path)
		if err != nil {
			return nil, err
		}
		latest := latestPerBook(revisions)
		for _, book := range latest {
			if !windowOverlaps(book.WindowStart, book.WindowEnd, from, to) {
				continue
			}
			result = append(result, cloneBook(book))
		}
	}
	sortBooks(result)
	return result, nil
}

func (s *FileBookStore) Chambers(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chamberIDsLocked()
}

func (s *FileBookStore) chamberIDs() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chamberIDsLocked()
}

func (s *FileBookStore) chamberIDsLocked() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "handovers"))
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			result = append(result, strings.TrimSuffix(entry.Name(), ".jsonl"))
		}
	}
	sort.Strings(result)
	return result, nil
}

func appendBook(path string, book HandoverBook) error {
	encoded, err := json.Marshal(book)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func readBooks(path string) ([]storedRevision, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var result []storedRevision
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var book HandoverBook
		if err := json.Unmarshal(scanner.Bytes(), &book); err != nil {
			return nil, fmt.Errorf("%s: corrupted revision on line %d: %w", path, line, err)
		}
		result = append(result, storedRevision{Book: book, Line: line})
	}
	return result, scanner.Err()
}

func latestRevision(revisions []storedRevision, id uuid.UUID) (HandoverBook, bool) {
	for i := len(revisions) - 1; i >= 0; i-- {
		if revisions[i].Book.ID == id {
			return revisions[i].Book, true
		}
	}
	return HandoverBook{}, false
}

func latestPerBook(revisions []storedRevision) []HandoverBook {
	byID := make(map[uuid.UUID]int)
	for i, revision := range revisions {
		byID[revision.Book.ID] = i
	}
	result := make([]HandoverBook, 0, len(byID))
	for _, index := range byID {
		result = append(result, revisions[index].Book)
	}
	return result
}

// NewRuntimeBookStore selects the durable file store or the in-memory one.
func NewRuntimeBookStore(dataDirectory string) (BookStore, error) {
	if dataDirectory == "" {
		return NewMemoryBookStore(), nil
	}
	return NewFileBookStore(dataDirectory)
}
