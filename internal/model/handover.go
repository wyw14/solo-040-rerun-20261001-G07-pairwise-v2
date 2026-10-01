package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ShiftKind labels the two twelve-hour watches covered by a handover book.
type ShiftKind string

const (
	ShiftDay   ShiftKind = "day"
	ShiftNight ShiftKind = "night"
)

// DayShiftStartHour is the local-time hour at which the day watch begins (UTC).
const DayShiftStartHour = 8

// Shift is the half-open interval [Start, End) covered by one watch.
type Shift struct {
	Kind  ShiftKind `json:"kind"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Contains reports whether an instant belongs to the watch window.
func (s Shift) Contains(instant time.Time) bool {
	when := instant.UTC()
	return (when.Equal(s.Start) || when.After(s.Start)) && when.Before(s.End)
}

// ShiftAt derives the watch that owns an instant. Day watch runs
// [08:00, 20:00) UTC; night watch covers the remaining twelve hours.
func ShiftAt(instant time.Time) Shift {
	when := instant.UTC()
	startOfDay := time.Date(when.Year(), when.Month(), when.Day(), 0, 0, 0, 0, time.UTC)
	dayStart := startOfDay.Add(DayShiftStartHour * time.Hour)
	nightStart := dayStart.Add(12 * time.Hour)
	if !when.Before(dayStart) && when.Before(nightStart) {
		return Shift{Kind: ShiftDay, Start: dayStart, End: nightStart}
	}
	if when.Before(dayStart) {
		// Early-morning hours belong to the previous evening's night watch.
		nightStart = dayStart.Add(-12 * time.Hour)
	}
	return Shift{Kind: ShiftNight, Start: nightStart, End: nightStart.Add(12 * time.Hour)}
}

// HandoverStatus marks whether a book is still editable or sealed.
type HandoverStatus string

const (
	HandoverDraft     HandoverStatus = "draft"
	HandoverConfirmed HandoverStatus = "confirmed"
)

// ErrHandoverSealed is returned when a confirmed book is modified.
var ErrHandoverSealed = errors.New("handover book is confirmed and cannot be modified")

// EventRef is an immutable pointer back to a journal event captured at handover.
type EventRef struct {
	ID         uuid.UUID `json:"id"`
	ChamberID  string    `json:"chamber_id"`
	Sequence   uint64    `json:"sequence"`
	Type       EventType `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ReviewItem is a journal event flagged for follow-up during the watch.
type ReviewItem struct {
	EventID    uuid.UUID `json:"event_id"`
	Sequence   uint64    `json:"sequence"`
	Type       EventType `json:"type"`
	Severity   string    `json:"severity"`
	Summary    string    `json:"summary"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ShiftTransit summarises one lockage that began during the watch.
type ShiftTransit struct {
	CycleID       uuid.UUID    `json:"cycle_id"`
	PlanID        uuid.UUID    `json:"plan_id"`
	Direction     Direction    `json:"direction"`
	Generation    uint64       `json:"generation"`
	StartedAt     time.Time    `json:"started_at"`
	LastStage     TransitStage `json:"last_stage"`
	LastUpdatedAt time.Time    `json:"last_updated_at"`
	Terminal      bool         `json:"terminal"`
	VesselCount   int          `json:"vessel_count"`
}

// HandoverChamberState freezes the operational state of one chamber.
type HandoverChamberState struct {
	Cycle         *TransitCycle              `json:"cycle,omitempty"`
	Gates         map[Direction]GatePosition `json:"gates"`
	LevelM        float64                    `json:"level_m"`
	QueuedVessels []Vessel                   `json:"queued_vessels"`
	Signals       []SignalState              `json:"signals"`
	Emergency     EmergencyRecord            `json:"emergency"`
}

func (s HandoverChamberState) clone() HandoverChamberState {
	if s.Cycle != nil {
		cycle := s.Cycle.Clone()
		s.Cycle = &cycle
	}
	if s.Gates != nil {
		gates := make(map[Direction]GatePosition, len(s.Gates))
		for direction, position := range s.Gates {
			gates[direction] = position
		}
		s.Gates = gates
	}
	queued := make([]Vessel, len(s.QueuedVessels))
	copy(queued, s.QueuedVessels)
	s.QueuedVessels = queued
	signals := make([]SignalState, len(s.Signals))
	copy(signals, s.Signals)
	s.Signals = signals
	return s
}

// HandoverSnapshot is the point-in-time aggregate frozen when the book is
// generated. It is never recomputed after confirmation.
type HandoverSnapshot struct {
	TakenAt         time.Time            `json:"taken_at"`
	Shift           Shift                `json:"shift"`
	Transits        []ShiftTransit       `json:"transits"`
	UnfinishedPlans []ConvoyPlan         `json:"unfinished_plans"`
	PendingCommands []DeviceCommand      `json:"pending_commands"`
	Leases          []Lease              `json:"leases"`
	ReviewItems     []ReviewItem         `json:"review_items"`
	EventRefs       []EventRef           `json:"event_refs"`
	Chamber         HandoverChamberState `json:"chamber"`
}

func (s HandoverSnapshot) clone() HandoverSnapshot {
	transits := make([]ShiftTransit, len(s.Transits))
	copy(transits, s.Transits)
	s.Transits = transits
	if s.UnfinishedPlans != nil {
		plans := make([]ConvoyPlan, len(s.UnfinishedPlans))
		for index, plan := range s.UnfinishedPlans {
			plans[index] = plan.Clone()
		}
		s.UnfinishedPlans = plans
	}
	if s.PendingCommands != nil {
		commands := make([]DeviceCommand, len(s.PendingCommands))
		copy(commands, s.PendingCommands)
		s.PendingCommands = commands
	}
	leases := make([]Lease, len(s.Leases))
	copy(leases, s.Leases)
	s.Leases = leases
	reviews := make([]ReviewItem, len(s.ReviewItems))
	copy(reviews, s.ReviewItems)
	s.ReviewItems = reviews
	refs := make([]EventRef, len(s.EventRefs))
	copy(refs, s.EventRefs)
	s.EventRefs = refs
	s.Chamber = s.Chamber.clone()
	return s
}

// HandoverSeal binds the confirmed content to a digest so later tampering of
// the persisted record is detectable.
type HandoverSeal struct {
	Algorithm string    `json:"algorithm"`
	Digest    string    `json:"digest"`
	SealedAt  time.Time `json:"sealed_at"`
}

// HandoverBook is the per-chamber watch handover record.
type HandoverBook struct {
	ID              uuid.UUID        `json:"id"`
	ChamberID       string           `json:"chamber_id"`
	Shift           Shift            `json:"shift"`
	Status          HandoverStatus   `json:"status"`
	Snapshot        HandoverSnapshot `json:"snapshot"`
	Notes           string           `json:"notes,omitempty"`
	OutgoingOfficer string           `json:"outgoing_officer,omitempty"`
	IncomingOfficer string           `json:"incoming_officer,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	ConfirmedAt     *time.Time       `json:"confirmed_at,omitempty"`
	Seal            *HandoverSeal    `json:"seal,omitempty"`
}

// NewHandoverBook freezes an aggregate into an editable draft.
func NewHandoverBook(chamberID string, snapshot HandoverSnapshot, now time.Time) (HandoverBook, error) {
	if chamberID == "" {
		return HandoverBook{}, errors.New("handover book requires a chamber")
	}
	if snapshot.TakenAt.IsZero() {
		return HandoverBook{}, errors.New("handover snapshot requires a taken-at timestamp")
	}
	stamped := now.UTC()
	return HandoverBook{
		ID:        uuid.New(),
		ChamberID: chamberID,
		Shift:     snapshot.Shift,
		Status:    HandoverDraft,
		Snapshot:  snapshot.clone(),
		CreatedAt: stamped,
		UpdatedAt: stamped,
	}, nil
}

// AmendNotes lets the duty officer add remarks while the book is a draft.
func (b *HandoverBook) AmendNotes(notes, officer string, now time.Time) error {
	if b.Status == HandoverConfirmed {
		return ErrHandoverSealed
	}
	b.Notes = notes
	if officer != "" {
		b.OutgoingOfficer = officer
	}
	b.UpdatedAt = now.UTC()
	return nil
}

// Confirm freezes the book and computes its seal. It cannot be reopened.
func (b *HandoverBook) Confirm(outgoingOfficer, incomingOfficer string, now time.Time) error {
	if b.Status == HandoverConfirmed {
		return ErrHandoverSealed
	}
	stamped := now.UTC()
	b.OutgoingOfficer = outgoingOfficer
	b.IncomingOfficer = incomingOfficer
	digest, err := b.fingerprint()
	if err != nil {
		return err
	}
	b.Seal = &HandoverSeal{Algorithm: "SHA-256", Digest: digest, SealedAt: stamped}
	b.Status = HandoverConfirmed
	confirmed := stamped
	b.ConfirmedAt = &confirmed
	b.UpdatedAt = stamped
	return nil
}

// VerifySeal recomputes the digest and reports whether the sealed content is intact.
func (b HandoverBook) VerifySeal() (bool, error) {
	if b.Seal == nil {
		return false, errors.New("handover book is not sealed")
	}
	digest, err := b.fingerprint()
	if err != nil {
		return false, err
	}
	return digest == b.Seal.Digest, nil
}

// fingerprint covers the frozen snapshot and the officers' remarks, but never
// the seal itself or mutable bookkeeping timestamps.
func (b *HandoverBook) fingerprint() (string, error) {
	payload := struct {
		ChamberID       string           `json:"chamber_id"`
		Shift           Shift            `json:"shift"`
		Snapshot        HandoverSnapshot `json:"snapshot"`
		Notes           string           `json:"notes"`
		OutgoingOfficer string           `json:"outgoing_officer"`
		IncomingOfficer string           `json:"incoming_officer"`
	}{
		ChamberID:       b.ChamberID,
		Shift:           b.Shift,
		Snapshot:        b.Snapshot,
		Notes:           b.Notes,
		OutgoingOfficer: b.OutgoingOfficer,
		IncomingOfficer: b.IncomingOfficer,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
