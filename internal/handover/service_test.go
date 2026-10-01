package handover

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

type fakeCycles struct{ current model.TransitCycle }

func (f fakeCycles) Current(string) (model.TransitCycle, bool) { return f.current, true }

type fakePlans struct {
	active []model.ConvoyPlan
	byID   map[uuid.UUID]model.ConvoyPlan
}

func (f fakePlans) Active(string) []model.ConvoyPlan { return f.active }
func (f fakePlans) Get(id uuid.UUID) (model.ConvoyPlan, bool) {
	plan, ok := f.byID[id]
	return plan, ok
}

type fakeLeases struct{ leases []model.Lease }

func (f fakeLeases) Snapshot() []model.Lease { return f.leases }

type fakeLevels struct{ samples []model.LevelSample }

func (f fakeLevels) WindowSamples(string) []model.LevelSample { return f.samples }

type fakeQueue struct{ entries []admission.QueueEntry }

func (f fakeQueue) List(string) []admission.QueueEntry { return f.entries }

type fakeSignals struct{ states []model.SignalState }

func (f fakeSignals) States() []model.SignalState { return f.states }

type fakeSafety struct{ record model.EmergencyRecord }

func (f fakeSafety) Status(string) model.EmergencyRecord { return f.record }

func TestGenerateAggregatesShift(t *testing.T) {
	ctx := context.Background()
	chamber := "chamber-1"
	day10 := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	day1030 := day10.Add(30 * time.Minute)
	day11 := day10.Add(time.Hour)
	night22 := time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC)

	clock := day10
	now := func() time.Time { return clock }
	store := journal.NewMemoryStore()
	recorder := journal.NewRecorder(store, now)

	vessel, err := model.NewVessel("River Star", "RS-1", 80)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := model.NewConvoyPlan(chamber, model.DirectionUpstream, []model.Vessel{vessel}, day10)
	if err != nil {
		t.Fatal(err)
	}
	cycleValue, err := model.NewTransitCycle(chamber, plan.ID, model.DirectionUpstream, 1, day10)
	if err != nil {
		t.Fatal(err)
	}

	// In-shift events: cycle created, advanced, emergency started.
	if _, err := recorder.Record(ctx, chamber, model.EventCycleCreated, cycleValue.ID, 1, cycleValue); err != nil {
		t.Fatal(err)
	}
	clock = day1030
	if err := cycleValue.Advance(model.StageAdmitting, "gate opened", day1030); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Record(ctx, chamber, model.EventCycleAdvanced, cycleValue.ID, 1, cycleValue); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Record(ctx, chamber, model.EventEmergencyStarted, cycleValue.ID, 1, map[string]any{"reason": "drift"}); err != nil {
		t.Fatal(err)
	}
	// Out-of-shift event must be excluded from this book's references.
	clock = night22
	if _, err := recorder.Record(ctx, chamber, model.EventPlanCancelled, plan.ID, 0, map[string]any{"note": "late"}); err != nil {
		t.Fatal(err)
	}

	commands := gate.NewCommands()
	queued, err := commands.Issue(cycleValue, model.DeviceFillValve, "set_opening", 0.4, day1030)
	if err != nil {
		t.Fatal(err)
	}
	gates := gate.NewState(chamber)
	if err := gates.Observe(chamber, model.DirectionUpstream, model.GateOpening, day1030); err != nil {
		t.Fatal(err)
	}

	lease := model.Lease{VesselID: vessel.ID, Owner: "tow-7", FencingToken: 3, ExpiresAt: day10.Add(2 * time.Hour)}
	service := NewService(
		NewMemoryBookStore(), store,
		fakeCycles{current: cycleValue}, commands, gates,
		fakePlans{active: []model.ConvoyPlan{plan}, byID: map[uuid.UUID]model.ConvoyPlan{plan.ID: plan}},
		fakeLeases{leases: []model.Lease{lease}},
		fakeLevels{samples: []model.LevelSample{{Meters: 12.5, ObservedAt: day1030}}},
		fakeQueue{entries: []admission.QueueEntry{{Vessel: vessel, PlanID: plan.ID}}},
		fakeSignals{states: []model.SignalState{{VesselID: vessel.ID, CycleID: cycleValue.ID, Aspect: model.SignalProceed, UpdatedAt: day1030}}},
		fakeSafety{record: model.EmergencyRecord{ChamberID: chamber, Status: model.EmergencyDraining, RequestedAt: day1030}},
		now,
	)

	clock = day11
	book, err := service.Generate(ctx, chamber, day11)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if book.Status != model.HandoverDraft {
		t.Fatalf("status = %q, want draft", book.Status)
	}
	snap := book.Snapshot

	if len(snap.Transits) != 1 {
		t.Fatalf("transits = %d, want 1", len(snap.Transits))
	}
	transit := snap.Transits[0]
	if transit.CycleID != cycleValue.ID || transit.Direction != model.DirectionUpstream ||
		transit.LastStage != model.StageAdmitting || transit.VesselCount != 1 {
		t.Fatalf("unexpected transit summary: %+v", transit)
	}
	if len(snap.UnfinishedPlans) != 1 || snap.UnfinishedPlans[0].ID != plan.ID {
		t.Fatalf("unfinished plans = %+v", snap.UnfinishedPlans)
	}
	if len(snap.PendingCommands) != 1 || snap.PendingCommands[0].ID != queued.ID {
		t.Fatalf("pending commands = %+v", snap.PendingCommands)
	}
	if len(snap.Leases) != 1 || snap.Leases[0].FencingToken != 3 {
		t.Fatalf("leases = %+v", snap.Leases)
	}
	if len(snap.ReviewItems) != 1 || snap.ReviewItems[0].Type != model.EventEmergencyStarted {
		t.Fatalf("review items = %+v", snap.ReviewItems)
	}
	if len(snap.EventRefs) != 3 {
		t.Fatalf("event refs = %d, want 3 in-shift events", len(snap.EventRefs))
	}
	state := snap.Chamber
	if state.Cycle == nil || state.Cycle.ID != cycleValue.ID {
		t.Fatalf("frozen chamber cycle = %+v", state.Cycle)
	}
	if state.LevelM != 12.5 || state.Gates[model.DirectionUpstream] != model.GateOpening {
		t.Fatalf("frozen chamber state = %+v", state)
	}
	if len(state.QueuedVessels) != 1 || len(state.Signals) != 1 ||
		state.Emergency.Status != model.EmergencyDraining {
		t.Fatalf("frozen chamber details incomplete: %+v", state)
	}
}

func TestDraftLifecycleAndImmutableConfirm(t *testing.T) {
	ctx := context.Background()
	chamber := "chamber-2"
	day19 := time.Date(2026, 9, 29, 19, 0, 0, 0, time.UTC)
	now := func() time.Time { return day19 }
	store := journal.NewMemoryStore()

	service := NewService(
		NewMemoryBookStore(), store,
		fakeCycles{}, gate.NewCommands(), gate.NewState(chamber),
		fakePlans{byID: map[uuid.UUID]model.ConvoyPlan{}},
		fakeLeases{}, fakeLevels{}, fakeQueue{}, fakeSignals{}, fakeSafety{}, now,
	)

	book, err := service.Generate(ctx, chamber, day19)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	book, err = service.AmendNotes(ctx, book.ID, "夜间注意二号闸阀门", "zhao")
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if book.Notes == "" || book.OutgoingOfficer != "zhao" {
		t.Fatalf("amended book = %+v", book)
	}

	// Regenerating the same shift keeps the remarks but refreshes the snapshot.
	regenerated, err := service.Generate(ctx, chamber, day19.Add(time.Minute))
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if regenerated.ID != book.ID || regenerated.Notes != book.Notes {
		t.Fatal("regeneration should preserve identity and notes")
	}

	confirmed, err := service.Confirm(ctx, book.ID, "zhao", "sun")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if confirmed.Status != model.HandoverConfirmed || confirmed.IncomingOfficer != "sun" {
		t.Fatalf("confirmed book = %+v", confirmed)
	}
	intact, err := confirmed.VerifySeal()
	if err != nil || !intact {
		t.Fatalf("seal = (%v,%v)", intact, err)
	}

	if _, err := service.Generate(ctx, chamber, day19.Add(2*time.Minute)); err != model.ErrHandoverSealed {
		t.Fatalf("regenerate confirmed err = %v, want ErrHandoverSealed", err)
	}
	if _, err := service.AmendNotes(ctx, book.ID, "改不动", "zhao"); err != model.ErrHandoverSealed {
		t.Fatalf("amend confirmed err = %v, want ErrHandoverSealed", err)
	}
	if _, err := service.Confirm(ctx, book.ID, "zhao", "sun"); err != model.ErrHandoverSealed {
		t.Fatalf("double confirm err = %v, want ErrHandoverSealed", err)
	}

	// The sealed record is still retrievable unchanged.
	got, ok, err := service.Get(ctx, book.ID)
	if err != nil || !ok || got.Status != model.HandoverConfirmed {
		t.Fatalf("get confirmed = (%+v,%v,%v)", got, ok, err)
	}
}
