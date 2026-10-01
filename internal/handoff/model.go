package handoff

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status of a shift handover book.
type Status string

const (
	// StatusOpen is a live draft: its snapshot is regenerated on every save.
	StatusOpen Status = "open"
	// StatusConfirmed is sealed: neither the snapshot nor operator notes may change.
	StatusConfirmed Status = "confirmed"
)

func (s Status) Valid() bool { return s == StatusOpen || s == StatusConfirmed }

// EventRef points at the original journal event captured in a handover snapshot.
// The reference survives later state changes; the event itself is immutable.
type EventRef struct {
	ID         uuid.UUID       `json:"id"`
	ChamberID  string          `json:"chamber_id"`
	Sequence   uint64          `json:"sequence"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// CycleSummary describes one lock transit (闸次) seen during the shift window.
type CycleSummary struct {
	CycleID     uuid.UUID  `json:"cycle_id"`
	PlanID      uuid.UUID  `json:"plan_id"`
	Direction   string     `json:"direction"`
	Stage       string     `json:"stage"`
	Generation  uint64     `json:"generation"`
	StartedAt   time.Time  `json:"started_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Events      []EventRef `json:"events,omitempty"`
}

// PlanSummary describes an unfinished convoy plan carried into the next shift.
type PlanSummary struct {
	PlanID     uuid.UUID   `json:"plan_id"`
	Direction  string      `json:"direction"`
	VesselIDs  []uuid.UUID `json:"vessel_ids"`
	Vessels    []string    `json:"vessel_names"`
	CreatedAt  time.Time   `json:"created_at"`
	Queued     bool        `json:"queued"`
	CycleID    *uuid.UUID  `json:"cycle_id,omitempty"`
	CycleStage string      `json:"cycle_stage,omitempty"`
	Events     []EventRef  `json:"events,omitempty"`
}

// CommandSummary describes a device command that had not settled at handover.
type CommandSummary struct {
	CommandID  uuid.UUID `json:"command_id"`
	CycleID    uuid.UUID `json:"cycle_id"`
	Device     string    `json:"device"`
	Action     string    `json:"action"`
	Target     float64   `json:"target,omitempty"`
	State      string    `json:"state"`
	Generation uint64    `json:"generation"`
	Epoch      uint64    `json:"epoch"`
	IssuedAt   time.Time `json:"issued_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Event      *EventRef `json:"event,omitempty"`
}

// LeaseSummary describes an active vessel lease at handover.
type LeaseSummary struct {
	VesselID     uuid.UUID `json:"vessel_id"`
	Owner        string    `json:"owner"`
	FencingToken uint64    `json:"fencing_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Event        *EventRef `json:"event,omitempty"`
}

// IncidentItem is an event the next shift should review (事件复盘项).
type IncidentItem struct {
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	Event    EventRef `json:"event"`
}

// ChamberState is the point-in-time chamber view embedded in the snapshot.
type ChamberState struct {
	CycleID   *uuid.UUID        `json:"cycle_id,omitempty"`
	Stage     string            `json:"stage,omitempty"`
	Gates     map[string]string `json:"gates"`
	QueueSize int               `json:"queue_size"`
	Emergency string            `json:"emergency"`
	LevelM    float64           `json:"level_m"`
	Pending   int               `json:"pending_commands"`
	Sequence  uint64            `json:"journal_sequence"`
}

// Snapshot is the frozen-at-generation summary carried by a handover book.
type Snapshot struct {
	CapturedAt  time.Time        `json:"captured_at"`
	WindowStart time.Time        `json:"window_start"`
	WindowEnd   time.Time        `json:"window_end"`
	CycleCount  int              `json:"cycle_count"`
	Cycles      []CycleSummary   `json:"cycles"`
	Unfinished  []PlanSummary    `json:"unfinished_plans"`
	PendingCmds []CommandSummary `json:"pending_commands"`
	Leases      []LeaseSummary   `json:"leases"`
	Incidents   []IncidentItem   `json:"incident_reviews"`
	Chambers    []ChamberState   `json:"chambers"`
}

// HandoverBook is the per-chamber shift book (闸室交接簿).
type HandoverBook struct {
	ID          uuid.UUID  `json:"id"`
	ChamberID   string     `json:"chamber_id"`
	ShiftLabel  string     `json:"shift_label"`
	WindowStart time.Time  `json:"window_start"`
	WindowEnd   time.Time  `json:"window_end"`
	Status      Status     `json:"status"`
	Snapshot    *Snapshot  `json:"snapshot,omitempty"`
	Operator    string     `json:"operator,omitempty"`
	Notes       string     `json:"notes,omitempty"`
	ConfirmedBy string     `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Open reports whether the book still accepts note/snapshot changes.
func (b HandoverBook) Open() bool { return b.Status == StatusOpen }

func newBook(chamberID, shiftLabel string, start, end, now time.Time) (HandoverBook, error) {
	if chamberID == "" {
		return HandoverBook{}, errors.New("chamber id is required")
	}
	if end.Before(start) {
		return HandoverBook{}, errors.New("shift window end must not precede start")
	}
	return HandoverBook{
		ID:          uuid.New(),
		ChamberID:   chamberID,
		ShiftLabel:  shiftLabel,
		WindowStart: start.UTC(),
		WindowEnd:   end.UTC(),
		Status:      StatusOpen,
		CreatedAt:   now.UTC(),
		UpdatedAt:   now.UTC(),
	}, nil
}
