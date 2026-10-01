package handover

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/monitor"
)

// The read-side collaborators the aggregator needs. They are satisfied by the
// concrete runtime coordinators without this package owning them.
type (
	cycleLookup interface {
		Current(string) (model.TransitCycle, bool)
	}
	planReader interface {
		Active(string) []model.ConvoyPlan
		Get(uuid.UUID) (model.ConvoyPlan, bool)
	}
	leaseReader interface {
		Snapshot() []model.Lease
	}
	levelReader interface {
		WindowSamples(string) []model.LevelSample
	}
	queueReader interface {
		List(string) []admission.QueueEntry
	}
	signalReader interface {
		States() []model.SignalState
	}
	safetyReader interface {
		Status(string) model.EmergencyRecord
	}
)

// ErrBookNotFound is returned when a handover book does not exist.
var ErrBookNotFound = errors.New("handover book not found")

// Service generates, amends and confirms per-chamber watch handover books.
type Service struct {
	store    BookStore
	events   journal.Store
	cycles   cycleLookup
	commands *gate.Commands
	gates    *gate.State
	plans    planReader
	leases   leaseReader
	levels   levelReader
	queue    queueReader
	signals  signalReader
	safety   safetyReader
	now      func() time.Time
}

// NewService wires the aggregator to its read-side collaborators.
func NewService(
	store BookStore,
	eventStore journal.Store,
	cycles cycleLookup,
	commands *gate.Commands,
	gates *gate.State,
	plans planReader,
	leases leaseReader,
	levels levelReader,
	queue queueReader,
	signals signalReader,
	safety safetyReader,
	now func() time.Time,
) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		store: store, events: eventStore, cycles: cycles, commands: commands,
		gates: gates, plans: plans, leases: leases, levels: levels,
		queue: queue, signals: signals, safety: safety, now: now,
	}
}

// Generate aggregates the watch containing at (or the current watch) for a
// chamber into a draft book. Regenerating an existing draft refreshes the
// snapshot while keeping the duty officer's remarks; a confirmed book is
// immutable.
func (s *Service) Generate(ctx context.Context, chamberID string, at time.Time) (model.HandoverBook, error) {
	if chamberID == "" {
		return model.HandoverBook{}, errors.New("chamber is required")
	}
	if at.IsZero() {
		at = s.now()
	}
	shift := model.ShiftAt(at)
	existing, err := s.findBook(ctx, chamberID, shift)
	if err != nil {
		return model.HandoverBook{}, err
	}
	if existing != nil && existing.Status == model.HandoverConfirmed {
		return model.HandoverBook{}, model.ErrHandoverSealed
	}

	snapshot, err := s.buildSnapshot(ctx, chamberID, shift)
	if err != nil {
		return model.HandoverBook{}, err
	}

	if existing != nil {
		notes, officer := existing.Notes, existing.OutgoingOfficer
		existing.Snapshot = snapshot
		if err := existing.AmendNotes(notes, officer, s.now()); err != nil {
			return model.HandoverBook{}, err
		}
		if err := s.store.SaveDraft(ctx, *existing); err != nil {
			return model.HandoverBook{}, err
		}
		return *existing, nil
	}

	book, err := model.NewHandoverBook(chamberID, snapshot, s.now())
	if err != nil {
		return model.HandoverBook{}, err
	}
	if err := s.store.SaveDraft(ctx, book); err != nil {
		return model.HandoverBook{}, err
	}
	return book, nil
}

func (s *Service) findBook(ctx context.Context, chamberID string, shift model.Shift) (*model.HandoverBook, error) {
	books, err := s.store.List(ctx, BookFilter{
		ChamberID: chamberID,
		From:      shift.Start,
		To:        shift.End,
	})
	if err != nil {
		return nil, err
	}
	for index := range books {
		if books[index].Shift.Start.Equal(shift.Start) {
			return &books[index], nil
		}
	}
	return nil, nil
}

func (s *Service) buildSnapshot(ctx context.Context, chamberID string, shift model.Shift) (model.HandoverSnapshot, error) {
	events, err := s.events.Events(ctx, chamberID, 0)
	if err != nil {
		return model.HandoverSnapshot{}, err
	}
	inShift := make([]model.Event, 0, len(events))
	for _, event := range events {
		if shift.Contains(event.OccurredAt) {
			inShift = append(inShift, event)
		}
	}
	sort.Slice(inShift, func(i, j int) bool { return inShift[i].Sequence < inShift[j].Sequence })

	transits := s.summarizeTransits(inShift)
	reviews := s.reviewItems(inShift)
	refs := make([]model.EventRef, 0, len(inShift))
	for _, event := range inShift {
		refs = append(refs, model.EventRef{
			ID: event.ID, ChamberID: event.ChamberID, Sequence: event.Sequence,
			Type: event.Type, OccurredAt: event.OccurredAt,
		})
	}

	snapshot := model.HandoverSnapshot{
		TakenAt:         s.now().UTC(),
		Shift:           shift,
		Transits:        transits,
		UnfinishedPlans: nonNilPlans(s.plans.Active(chamberID)),
		PendingCommands: nonNilCommands(s.commands.Pending(chamberID)),
		Leases:          s.chamberLeases(chamberID),
		ReviewItems:     reviews,
		EventRefs:       refs,
		Chamber:         s.chamberState(chamberID),
	}
	return snapshot, nil
}

func nonNilPlans(plans []model.ConvoyPlan) []model.ConvoyPlan {
	if plans == nil {
		return []model.ConvoyPlan{}
	}
	return plans
}

func nonNilCommands(commands []model.DeviceCommand) []model.DeviceCommand {
	if commands == nil {
		return []model.DeviceCommand{}
	}
	return commands
}

func (s *Service) summarizeTransits(events []model.Event) []model.ShiftTransit {
	byCycle := make(map[uuid.UUID]model.ShiftTransit)
	var order []uuid.UUID
	for _, event := range events {
		switch event.Type {
		case model.EventCycleCreated, model.EventCycleAdvanced:
			var cycleValue model.TransitCycle
			if err := event.Decode(&cycleValue); err != nil {
				continue
			}
			summary, ok := byCycle[cycleValue.ID]
			if !ok {
				summary = model.ShiftTransit{
					CycleID: cycleValue.ID, PlanID: cycleValue.PlanID,
					Direction: cycleValue.Direction, Generation: cycleValue.Generation,
					StartedAt: cycleValue.CreatedAt,
				}
				if plan, found := s.plans.Get(cycleValue.PlanID); found {
					summary.VesselCount = len(plan.Vessels)
				}
				order = append(order, cycleValue.ID)
			}
			if event.Type == model.EventCycleCreated {
				summary.StartedAt = cycleValue.CreatedAt
			}
			summary.LastStage = cycleValue.Stage
			summary.LastUpdatedAt = cycleValue.UpdatedAt
			summary.Terminal = cycleValue.Stage.Terminal()
			byCycle[cycleValue.ID] = summary
		}
	}
	result := make([]model.ShiftTransit, 0, len(order))
	for _, id := range order {
		result = append(result, byCycle[id])
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StartedAt.Before(result[j].StartedAt) })
	return result
}

func (s *Service) reviewItems(events []model.Event) []model.ReviewItem {
	result := make([]model.ReviewItem, 0)
	for _, event := range events {
		severity, summary, include := monitor.IncidentMeaning(event.Type)
		if !include {
			continue
		}
		result = append(result, model.ReviewItem{
			EventID: event.ID, Sequence: event.Sequence, Type: event.Type,
			Severity: severity, Summary: summary, OccurredAt: event.OccurredAt,
		})
	}
	return result
}

// chamberLeases keeps only leases held by vessels queued or planned in this
// chamber, so a watch inherits the ownerships relevant to its lockage.
func (s *Service) chamberLeases(chamberID string) []model.Lease {
	vesselIDs := make(map[uuid.UUID]struct{})
	for _, entry := range s.queue.List(chamberID) {
		vesselIDs[entry.Vessel.ID] = struct{}{}
	}
	for _, plan := range s.plans.Active(chamberID) {
		for _, vessel := range plan.Vessels {
			vesselIDs[vessel.ID] = struct{}{}
		}
	}
	result := make([]model.Lease, 0)
	for _, lease := range s.leases.Snapshot() {
		if _, ok := vesselIDs[lease.VesselID]; ok {
			result = append(result, lease)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].VesselID.String() < result[j].VesselID.String()
	})
	return result
}

func (s *Service) chamberState(chamberID string) model.HandoverChamberState {
	state := model.HandoverChamberState{
		Gates:         s.gates.Snapshot(chamberID),
		QueuedVessels: []model.Vessel{},
		Signals:       []model.SignalState{},
	}
	if cycleValue, ok := s.cycles.Current(chamberID); ok {
		cycle := cycleValue
		state.Cycle = &cycle
	}
	if samples := s.levels.WindowSamples(chamberID); len(samples) > 0 {
		state.LevelM = samples[len(samples)-1].Meters
	}
	for _, entry := range s.queue.List(chamberID) {
		state.QueuedVessels = append(state.QueuedVessels, entry.Vessel)
	}
	relevant := make(map[uuid.UUID]struct{})
	for _, vessel := range state.QueuedVessels {
		relevant[vessel.ID] = struct{}{}
	}
	for _, plan := range s.plans.Active(chamberID) {
		for _, vessel := range plan.Vessels {
			relevant[vessel.ID] = struct{}{}
		}
	}
	for _, signalState := range s.signals.States() {
		if state.Cycle != nil && signalState.CycleID == state.Cycle.ID {
			state.Signals = append(state.Signals, signalState)
			continue
		}
		if _, ok := relevant[signalState.VesselID]; ok {
			state.Signals = append(state.Signals, signalState)
		}
	}
	state.Emergency = s.safety.Status(chamberID)
	return state
}

// AmendNotes stores the outgoing duty officer's remarks on a draft.
func (s *Service) AmendNotes(ctx context.Context, bookID uuid.UUID, notes, officer string) (model.HandoverBook, error) {
	book, ok, err := s.store.Get(ctx, bookID)
	if err != nil {
		return model.HandoverBook{}, err
	}
	if !ok {
		return model.HandoverBook{}, ErrBookNotFound
	}
	if err := book.AmendNotes(notes, officer, s.now()); err != nil {
		return model.HandoverBook{}, err
	}
	if err := s.store.SaveDraft(ctx, book); err != nil {
		return model.HandoverBook{}, err
	}
	return book, nil
}

// Confirm seals a draft once both officers accept the handover.
func (s *Service) Confirm(ctx context.Context, bookID uuid.UUID, outgoingOfficer, incomingOfficer string) (model.HandoverBook, error) {
	book, ok, err := s.store.Get(ctx, bookID)
	if err != nil {
		return model.HandoverBook{}, err
	}
	if !ok {
		return model.HandoverBook{}, ErrBookNotFound
	}
	if err := book.Confirm(outgoingOfficer, incomingOfficer, s.now()); err != nil {
		return model.HandoverBook{}, err
	}
	if err := s.store.Confirm(ctx, book); err != nil {
		return model.HandoverBook{}, err
	}
	return book, nil
}

// Get returns a single book.
func (s *Service) Get(ctx context.Context, bookID uuid.UUID) (model.HandoverBook, bool, error) {
	return s.store.Get(ctx, bookID)
}

// List returns books matching the filter, newest watch first.
func (s *Service) List(ctx context.Context, filter BookFilter) ([]model.HandoverBook, error) {
	return s.store.List(ctx, filter)
}
