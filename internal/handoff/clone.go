package handoff

import "github.com/google/uuid"

// cloneBook returns a deep copy so callers cannot mutate stored snapshots.
func cloneBook(book HandoverBook) HandoverBook {
	if book.Snapshot != nil {
		book.Snapshot = cloneSnapshot(book.Snapshot)
	}
	return book
}

func cloneSnapshot(s *Snapshot) *Snapshot {
	if s == nil {
		return nil
	}
	copyEvent := func(e EventRef) EventRef {
		if e.Payload != nil {
			e.Payload = append([]byte(nil), e.Payload...)
		}
		return e
	}
	c := *s
	c.Cycles = append([]CycleSummary(nil), s.Cycles...)
	for i := range c.Cycles {
		c.Cycles[i].Events = append([]EventRef(nil), s.Cycles[i].Events...)
		for j := range c.Cycles[i].Events {
			c.Cycles[i].Events[j] = copyEvent(s.Cycles[i].Events[j])
		}
		if s.Cycles[i].CompletedAt != nil {
			t := *s.Cycles[i].CompletedAt
			c.Cycles[i].CompletedAt = &t
		}
	}
	c.Unfinished = append([]PlanSummary(nil), s.Unfinished...)
	for i := range c.Unfinished {
		c.Unfinished[i].VesselIDs = append([]uuid.UUID(nil), s.Unfinished[i].VesselIDs...)
		c.Unfinished[i].Vessels = append([]string(nil), s.Unfinished[i].Vessels...)
		c.Unfinished[i].Events = append([]EventRef(nil), s.Unfinished[i].Events...)
		for j := range c.Unfinished[i].Events {
			c.Unfinished[i].Events[j] = copyEvent(s.Unfinished[i].Events[j])
		}
		if s.Unfinished[i].CycleID != nil {
			id := *s.Unfinished[i].CycleID
			c.Unfinished[i].CycleID = &id
		}
	}
	c.PendingCmds = append([]CommandSummary(nil), s.PendingCmds...)
	for i := range c.PendingCmds {
		if s.PendingCmds[i].Event != nil {
			event := copyEvent(*s.PendingCmds[i].Event)
			c.PendingCmds[i].Event = &event
		}
	}
	c.Leases = append([]LeaseSummary(nil), s.Leases...)
	for i := range c.Leases {
		if s.Leases[i].Event != nil {
			event := copyEvent(*s.Leases[i].Event)
			c.Leases[i].Event = &event
		}
	}
	c.Incidents = append([]IncidentItem(nil), s.Incidents...)
	for i := range c.Incidents {
		c.Incidents[i].Event = copyEvent(s.Incidents[i].Event)
	}
	c.Chambers = append([]ChamberState(nil), s.Chambers...)
	for i := range c.Chambers {
		c.Chambers[i].Gates = cloneStringMap(s.Chambers[i].Gates)
		if s.Chambers[i].CycleID != nil {
			id := *s.Chambers[i].CycleID
			c.Chambers[i].CycleID = &id
		}
	}
	return &c
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
