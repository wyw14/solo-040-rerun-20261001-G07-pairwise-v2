package monitor

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

type Incident struct {
	ID         uuid.UUID       `json:"id"`
	ChamberID  string          `json:"chamber_id"`
	Severity   string          `json:"severity"`
	Summary    string          `json:"summary"`
	EventType  model.EventType `json:"event_type"`
	Sequence   uint64          `json:"sequence"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// IncidentReview derives operator-review items from persisted safety events.
type IncidentReview struct {
	store journal.Store
}

func NewIncidentReview(store journal.Store) *IncidentReview { return &IncidentReview{store: store} }

func (r *IncidentReview) List(ctx context.Context) ([]Incident, error) {
	return r.Between(ctx, time.Time{}, time.Time{})
}

// Between returns incidents whose occurrence falls within [from, to].
// A zero bound is treated as open-ended.
func (r *IncidentReview) Between(ctx context.Context, from, to time.Time) ([]Incident, error) {
	chambers, err := r.store.Chambers(ctx)
	if err != nil {
		return nil, err
	}
	var result []Incident
	for _, chamberID := range chambers {
		events, err := r.store.Events(ctx, chamberID, 0)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			if !from.IsZero() && event.OccurredAt.Before(from) {
				continue
			}
			if !to.IsZero() && event.OccurredAt.After(to) {
				continue
			}
			severity, summary, include := incidentMeaning(event.Type)
			if !include {
				continue
			}
			result = append(result, Incident{
				ID: event.ID, ChamberID: chamberID, Severity: severity,
				Summary: summary, EventType: event.Type, Sequence: event.Sequence,
				OccurredAt: event.OccurredAt, Payload: event.Payload,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OccurredAt.After(result[j].OccurredAt) })
	return result, nil
}

func incidentMeaning(kind model.EventType) (string, string, bool) {
	switch kind {
	case model.EventEmergencyStarted:
		return "critical", "Emergency stop requested", true
	case model.EventEmergencySettled:
		return "notice", "Emergency equipment reached stable state", true
	case model.EventReplayWarning:
		return "warning", "Recovery sequence requires review", true
	case model.EventPlanCancelled:
		return "notice", "Convoy plan cancelled and compensated", true
	default:
		return "", "", false
	}
}
