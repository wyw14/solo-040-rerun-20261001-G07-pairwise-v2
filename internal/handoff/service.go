package handoff

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/cycle"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/lease"
	"github.com/wyw14/cry-96/internal/monitor"
	"github.com/wyw14/cry-96/internal/safety"
	"github.com/wyw14/cry-96/internal/schedule"
)

// Sources bundles the read-only collaborators used to build a snapshot.
// Nothing here is mutated: the handover service only observes current state
// and reads the durable journal.
type Sources struct {
	Store      journal.Store
	Cycles     *cycle.Coordinator
	Gates      *gate.State
	Commands   *gate.Commands
	Planner    *schedule.Planner
	Queue      *admission.Service
	Leases     *lease.Service
	Safety     *safety.Service
	Hydraulics *hydraulics.Controller
	Incidents  *monitor.IncidentReview
}

func (s Sources) validate() error {
	switch {
	case s.Store == nil:
		return errors.New("journal store is required")
	case s.Cycles == nil, s.Gates == nil, s.Commands == nil:
		return errors.New("cycle, gate state and command sources are required")
	case s.Planner == nil, s.Queue == nil, s.Leases == nil:
		return errors.New("planner, queue and lease sources are required")
	case s.Safety == nil, s.Hydraulics == nil, s.Incidents == nil:
		return errors.New("safety, hydraulics and incident sources are required")
	}
	return nil
}

// Service generates, persists and seals shift handover books.
type Service struct {
	store BookStore
	src   Sources
	now   func() time.Time
}

func NewService(store BookStore, src Sources, now func() time.Time) (*Service, error) {
	if err := src.validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, src: src, now: now}, nil
}

// ShiftWindow returns the three-shift window containing t (08/16/00
// boundaries) together with a Chinese shift label used by the book.
func ShiftWindow(t time.Time) (time.Time, time.Time, string) {
	t = t.Local()
	hour := t.Hour()
	var start time.Time
	var span string
	switch {
	case hour >= 8 && hour < 16:
		start = time.Date(t.Year(), t.Month(), t.Day(), 8, 0, 0, 0, t.Location())
		span = "早班 08:00-16:00"
	case hour >= 16:
		start = time.Date(t.Year(), t.Month(), t.Day(), 16, 0, 0, 0, t.Location())
		span = "晚班 16:00-24:00"
	default:
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		span = "夜班 00:00-08:00"
	}
	end := start.Add(8 * time.Hour)
	label := start.Format("2006-01-02") + " " + span
	return start, end, label
}

// OpenInput carries optional explicit window parameters.
type OpenInput struct {
	ChamberID  string
	ShiftLabel string
	Start      time.Time
	End        time.Time
	Operator   string
}

// Open returns the open book for the chamber and shift window, creating one
// (and generating its first snapshot) when none exists yet. A window already
// sealed by a confirmed book cannot be reopened.
func (s *Service) Open(ctx context.Context, input OpenInput) (HandoverBook, error) {
	if input.ChamberID == "" {
		return HandoverBook{}, errors.New("chamber id is required")
	}
	start, end := input.Start, input.End
	label := input.ShiftLabel
	if start.IsZero() || end.IsZero() {
		autoStart, autoEnd, autoLabel := ShiftWindow(s.now())
		if start.IsZero() {
			start = autoStart
		}
		if end.IsZero() {
			end = autoEnd
		}
		if label == "" {
			label = autoLabel
		}
	}
	existing, err := s.store.List(ctx, input.ChamberID, start, end)
	if err != nil {
		return HandoverBook{}, err
	}
	for _, book := range existing {
		if book.WindowStart.Equal(start.UTC()) && book.WindowEnd.Equal(end.UTC()) {
			if !book.Open() {
				return HandoverBook{}, fmt.Errorf("shift %s is already sealed by confirmed handover %s", label, book.ID)
			}
			return s.Refresh(ctx, book.ID)
		}
	}
	book, err := newBook(input.ChamberID, label, start, end, s.now())
	if err != nil {
		return HandoverBook{}, err
	}
	book.Operator = input.Operator
	if err := s.store.Create(ctx, book); err != nil {
		return HandoverBook{}, err
	}
	return s.Refresh(ctx, book.ID)
}

// Refresh regenerates the snapshot of an open book from current state.
func (s *Service) Refresh(ctx context.Context, id uuid.UUID) (HandoverBook, error) {
	book, ok, err := s.store.Get(ctx, id)
	if err != nil {
		return HandoverBook{}, err
	}
	if !ok {
		return HandoverBook{}, errors.New("handover book not found")
	}
	if !book.Open() {
		return book, nil
	}
	snapshot, err := s.buildSnapshot(ctx, book)
	if err != nil {
		return HandoverBook{}, err
	}
	book.Snapshot = snapshot
	book.UpdatedAt = s.now().UTC()
	if err := s.store.Update(ctx, book); err != nil {
		return HandoverBook{}, err
	}
	return book, nil
}

// SaveNotes stores the duty operator's supplementary remarks on an open book.
func (s *Service) SaveNotes(ctx context.Context, id uuid.UUID, operator, notes string) (HandoverBook, error) {
	book, ok, err := s.store.Get(ctx, id)
	if err != nil {
		return HandoverBook{}, err
	}
	if !ok {
		return HandoverBook{}, errors.New("handover book not found")
	}
	if !book.Open() {
		return HandoverBook{}, ErrBookSealed
	}
	book.Operator = operator
	book.Notes = notes
	book.UpdatedAt = s.now().UTC()
	if err := s.store.Update(ctx, book); err != nil {
		return HandoverBook{}, err
	}
	return book, nil
}

// ConfirmInput carries the outgoing operator's final remarks.
type ConfirmInput struct {
	ConfirmedBy string
	Operator    string
	Notes       string
}

// Confirm takes a final snapshot and seals the book. The sealed snapshot and
// remarks are never updated again, even when live state changes afterwards.
func (s *Service) Confirm(ctx context.Context, id uuid.UUID, input ConfirmInput) (HandoverBook, error) {
	book, ok, err := s.store.Get(ctx, id)
	if err != nil {
		return HandoverBook{}, err
	}
	if !ok {
		return HandoverBook{}, errors.New("handover book not found")
	}
	if !book.Open() {
		return HandoverBook{}, ErrBookSealed
	}
	snapshot, err := s.buildSnapshot(ctx, book)
	if err != nil {
		return HandoverBook{}, err
	}
	now := s.now().UTC()
	book.Snapshot = snapshot
	if input.Operator != "" {
		book.Operator = input.Operator
	}
	if input.Notes != "" || book.Notes == "" {
		book.Notes = input.Notes
	}
	if input.ConfirmedBy == "" {
		return HandoverBook{}, errors.New("confirmed_by is required")
	}
	book.ConfirmedBy = input.ConfirmedBy
	book.ConfirmedAt = &now
	book.Status = StatusConfirmed
	book.UpdatedAt = now
	if err := s.store.Update(ctx, book); err != nil {
		return HandoverBook{}, err
	}
	return book, nil
}

// Get fetches a single book.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (HandoverBook, bool, error) {
	return s.store.Get(ctx, id)
}

// List returns books filtered by chamber and time window.
func (s *Service) List(ctx context.Context, chamberID string, from, to time.Time) ([]HandoverBook, error) {
	return s.store.List(ctx, chamberID, from, to)
}
