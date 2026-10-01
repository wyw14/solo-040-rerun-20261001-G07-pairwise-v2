package handoff

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

// buildSnapshot derives the point-in-time summary. It only reads live state
// and the durable journal; every referenced event is stored by id/sequence so
// the snapshot stays meaningful after the live state moves on.
func (s *Service) buildSnapshot(ctx context.Context, book HandoverBook) (*Snapshot, error) {
	chamberID := book.ChamberID
	events, err := s.src.Store.Events(ctx, chamberID, 0)
	if err != nil {
		return nil, err
	}
	windowEvents := make([]model.Event, 0, len(events))
	var lastSequence uint64
	for _, event := range events {
		if event.Sequence > lastSequence {
			lastSequence = event.Sequence
		}
		if !event.OccurredAt.Before(book.WindowStart) && !event.OccurredAt.After(book.WindowEnd) {
			windowEvents = append(windowEvents, event)
		}
	}

	incidentItems, err := s.collectIncidents(ctx, chamberID, book.WindowStart, book.WindowEnd)
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{
		CapturedAt:  s.now().UTC(),
		WindowStart: book.WindowStart,
		WindowEnd:   book.WindowEnd,
		Cycles:      make([]CycleSummary, 0),
		Unfinished:  make([]PlanSummary, 0),
		PendingCmds: make([]CommandSummary, 0),
		Leases:      make([]LeaseSummary, 0),
		Incidents:   make([]IncidentItem, 0),
		Chambers:    []ChamberState{s.collectChamberState(chamberID, lastSequence)},
	}
	snapshot.Cycles = s.collectCycles(chamberID, windowEvents)
	snapshot.Unfinished = s.collectUnfinishedPlans(chamberID, windowEvents)
	snapshot.PendingCmds = s.collectPendingCommands(chamberID, events)
	snapshot.Leases = s.collectLeases(chamberID, windowEvents)
	snapshot.Incidents = incidentItems
	snapshot.CycleCount = len(snapshot.Cycles)
	return snapshot, nil
}

func refFromEvent(event model.Event) EventRef {
	return EventRef{
		ID:         event.ID,
		ChamberID:  event.ChamberID,
		Sequence:   event.Sequence,
		Type:       string(event.Type),
		OccurredAt: event.OccurredAt,
		Payload:    append([]byte(nil), event.Payload...),
	}
}

// collectCycles summarizes cycles created during the window plus any cycle
// still active for the chamber (a transit can straddle a shift boundary).
func (s *Service) collectCycles(chamberID string, window []model.Event) []CycleSummary {
	byID := make(map[uuid.UUID]*CycleSummary)
	order := make([]uuid.UUID, 0)

	for _, event := range window {
		if event.Type != model.EventCycleCreated && event.Type != model.EventCycleAdvanced {
			continue
		}
		var cycleValue model.TransitCycle
		if err := json.Unmarshal(event.Payload, &cycleValue); err != nil || cycleValue.ID == uuid.Nil {
			continue
		}
		summary, ok := byID[cycleValue.ID]
		if !ok {
			summary = &CycleSummary{
				CycleID:    cycleValue.ID,
				PlanID:     cycleValue.PlanID,
				Direction:  string(cycleValue.Direction),
				Generation: cycleValue.Generation,
				StartedAt:  cycleValue.CreatedAt,
				UpdatedAt:  cycleValue.UpdatedAt,
			}
			byID[cycleValue.ID] = summary
			order = append(order, cycleValue.ID)
		}
		if event.Type == model.EventCycleCreated {
			summary.StartedAt = event.OccurredAt
		}
		summary.Stage = string(cycleValue.Stage)
		summary.UpdatedAt = event.OccurredAt
		if cycleValue.Stage.Terminal() {
			completed := event.OccurredAt
			summary.CompletedAt = &completed
		}
		summary.Events = append(summary.Events, refFromEvent(event))
	}

	// A transit started by the previous shift is still in flight: include it
	// using its latest known state even if it has no created-event in window.
	if current, ok := s.src.Cycles.Current(chamberID); ok && !current.Stage.Terminal() {
		if _, exists := byID[current.ID]; !exists {
			summary := &CycleSummary{
				CycleID: current.ID, PlanID: current.PlanID,
				Direction: string(current.Direction), Stage: string(current.Stage),
				Generation: current.Generation, StartedAt: current.CreatedAt,
				UpdatedAt: current.UpdatedAt,
			}
			byID[current.ID] = summary
			order = append(order, current.ID)
		}
	}

	result := make([]CycleSummary, 0, len(order))
	for _, id := range order {
		summary := *byID[id]
		sort.Slice(summary.Events, func(i, j int) bool { return summary.Events[i].Sequence < summary.Events[j].Sequence })
		result = append(result, summary)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StartedAt.Before(result[j].StartedAt) })
	return result
}

// collectUnfinishedPlans lists active (non-cancelled) plans for the chamber.
func (s *Service) collectUnfinishedPlans(chamberID string, window []model.Event) []PlanSummary {
	plans := s.src.Planner.Active(chamberID)
	queue := s.src.Queue.List(chamberID)
	current, hasCurrent := s.src.Cycles.Current(chamberID)

	planEvents := make(map[uuid.UUID][]EventRef)
	for _, event := range window {
		if event.Type != model.EventPlanCreated && event.Type != model.EventPlanCancelled {
			continue
		}
		var plan model.ConvoyPlan
		if err := json.Unmarshal(event.Payload, &plan); err != nil || plan.ID == uuid.Nil {
			continue
		}
		planEvents[plan.ID] = append(planEvents[plan.ID], refFromEvent(event))
	}

	result := make([]PlanSummary, 0, len(plans))
	for _, plan := range plans {
		summary := PlanSummary{
			PlanID: plan.ID, Direction: string(plan.Direction),
			VesselIDs: make([]uuid.UUID, 0, len(plan.Vessels)),
			Vessels:   make([]string, 0, len(plan.Vessels)),
			CreatedAt: plan.CreatedAt,
			Events:    planEvents[plan.ID],
		}
		for _, vessel := range plan.Vessels {
			summary.VesselIDs = append(summary.VesselIDs, vessel.ID)
			summary.Vessels = append(summary.Vessels, vessel.Name)
		}
		for _, entry := range queue {
			if entry.PlanID == plan.ID {
				summary.Queued = true
				break
			}
		}
		if hasCurrent && current.PlanID == plan.ID {
			id := current.ID
			summary.CycleID = &id
			summary.CycleStage = string(current.Stage)
		}
		sort.Slice(summary.Events, func(i, j int) bool { return summary.Events[i].Sequence < summary.Events[j].Sequence })
		result = append(result, summary)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

// collectPendingCommands lists commands that had not reached a terminal state
// when the snapshot was taken, with the original queued event reference.
func (s *Service) collectPendingCommands(chamberID string, all []model.Event) []CommandSummary {
	pending := s.src.Commands.Pending(chamberID)
	queuedEvent := make(map[uuid.UUID]model.Event)
	for _, event := range all {
		if event.Type != model.EventCommandQueued {
			continue
		}
		var command model.DeviceCommand
		if err := json.Unmarshal(event.Payload, &command); err != nil || command.ID == uuid.Nil {
			continue
		}
		queuedEvent[command.ID] = event
	}
	result := make([]CommandSummary, 0, len(pending))
	for _, command := range pending {
		summary := CommandSummary{
			CommandID: command.ID, CycleID: command.CycleID,
			Device: string(command.Device), Action: command.Action,
			Target: command.Target, State: string(command.State),
			Generation: command.Generation, Epoch: command.Epoch,
			IssuedAt: command.CreatedAt, UpdatedAt: command.UpdatedAt,
		}
		if event, ok := queuedEvent[command.ID]; ok {
			ref := refFromEvent(event)
			summary.Event = &ref
		}
		result = append(result, summary)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IssuedAt.Before(result[j].IssuedAt) })
	return result
}

// collectLeases lists active leases for vessels involved with the chamber,
// referencing the latest lease.changed event seen during the window.
func (s *Service) collectLeases(chamberID string, window []model.Event) []LeaseSummary {
	relevant := make(map[uuid.UUID]struct{})
	for _, entry := range s.src.Queue.List(chamberID) {
		relevant[entry.Vessel.ID] = struct{}{}
	}
	for _, plan := range s.src.Planner.Active(chamberID) {
		for _, vessel := range plan.Vessels {
			relevant[vessel.ID] = struct{}{}
		}
	}
	if len(relevant) == 0 {
		return []LeaseSummary{}
	}
	latestEvent := make(map[uuid.UUID]model.Event)
	for _, event := range window {
		if event.Type != model.EventLeaseChanged {
			continue
		}
		var payload struct {
			VesselID uuid.UUID `json:"vessel_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.VesselID == uuid.Nil {
			continue
		}
		latestEvent[payload.VesselID] = event
	}
	now := s.now()
	result := make([]LeaseSummary, 0, len(relevant))
	for _, leaseValue := range s.src.Leases.Active(now) {
		if _, ok := relevant[leaseValue.VesselID]; !ok {
			continue
		}
		summary := LeaseSummary{
			VesselID: leaseValue.VesselID, Owner: leaseValue.Owner,
			FencingToken: leaseValue.FencingToken, ExpiresAt: leaseValue.ExpiresAt,
		}
		if event, ok := latestEvent[leaseValue.VesselID]; ok {
			ref := refFromEvent(event)
			summary.Event = &ref
		}
		result = append(result, summary)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ExpiresAt.Before(result[j].ExpiresAt) })
	return result
}

// collectIncidents maps review-worthy events from the shift window.
func (s *Service) collectIncidents(ctx context.Context, chamberID string, from, to time.Time) ([]IncidentItem, error) {
	incidents, err := s.src.Incidents.Between(ctx, from, to)
	if err != nil {
		return nil, err
	}
	result := make([]IncidentItem, 0, len(incidents))
	for _, incident := range incidents {
		if incident.ChamberID != chamberID {
			continue
		}
		result = append(result, IncidentItem{
			Severity: incident.Severity,
			Summary:  incident.Summary,
			Event: EventRef{
				ID:         incident.ID,
				ChamberID:  incident.ChamberID,
				Sequence:   incident.Sequence,
				Type:       string(incident.EventType),
				OccurredAt: incident.OccurredAt,
				Payload:    append([]byte(nil), incident.Payload...),
			},
		})
	}
	return result, nil
}

func (s *Service) collectChamberState(chamberID string, sequence uint64) ChamberState {
	state := ChamberState{
		Gates:    map[string]string{},
		Sequence: sequence,
	}
	for direction, position := range s.src.Gates.Snapshot(chamberID) {
		state.Gates[string(direction)] = string(position)
	}
	state.QueueSize = len(s.src.Queue.List(chamberID))
	state.Emergency = string(s.src.Safety.Status(chamberID).Status)
	state.Pending = len(s.src.Commands.Pending(chamberID))
	if samples := s.src.Hydraulics.WindowSamples(chamberID); len(samples) > 0 {
		state.LevelM = samples[len(samples)-1].Meters
	}
	if current, ok := s.src.Cycles.Current(chamberID); ok {
		id := current.ID
		state.CycleID = &id
		state.Stage = string(current.Stage)
	}
	return state
}
