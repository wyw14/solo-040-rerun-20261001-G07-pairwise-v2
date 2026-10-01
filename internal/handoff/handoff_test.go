package handoff_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/cycle"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/handoff"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/lease"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/monitor"
	"github.com/wyw14/cry-96/internal/safety"
	"github.com/wyw14/cry-96/internal/schedule"
	"github.com/wyw14/cry-96/internal/signal"
)

type graph struct {
	ctx          context.Context
	now          time.Time
	clock        func() time.Time
	store        journal.Store
	recorder     *journal.Recorder
	gateState    *gate.State
	gateCommands *gate.Commands
	gates        *gate.Controller
	admission    *admission.Service
	planner      *schedule.Planner
	hydraulics   *hydraulics.Controller
	cycles       *cycle.Coordinator
	cycleService *cycle.Service
	leases       *lease.Service
	safety       *safety.Service
	incidents    *monitor.IncidentReview
}

func newGraph(t *testing.T, chamberID string) *graph {
	t.Helper()
	base := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	g := &graph{ctx: context.Background(), now: base}
	g.clock = func() time.Time { return g.now }

	store := journal.NewMemoryStore()
	g.store = store
	g.recorder = journal.NewRecorder(store, g.clock)

	g.gateState = gate.NewState(chamberID)
	g.gateCommands = gate.NewCommands()
	g.gates = gate.NewController(g.gateState, g.gateCommands, g.recorder, g.clock)

	signalStore := signal.NewStore()
	queue := admission.NewQueue()
	admissionController := admission.NewController(queue, g.gateState, signalStore, g.clock)
	g.admission = admission.NewService(admissionController, g.recorder, g.clock)

	leaseRegistry := lease.NewRegistry()
	g.leases = lease.NewService(leaseRegistry, admissionController, g.recorder, g.clock)

	g.planner = schedule.NewPlanner(g.admission, g.recorder, g.clock)

	ramp := hydraulics.NewRamp(time.Hour, g.clock)
	g.hydraulics = hydraulics.NewController(g.gateCommands, g.recorder, ramp, g.clock)

	g.cycles = cycle.NewCoordinator(g.gates, g.hydraulics, g.recorder, g.clock)
	g.cycleService = cycle.NewService(g.cycles, g.admission)

	barrier := safety.NewBarrier()
	safetyCoordinator := safety.NewCoordinator(g.gateCommands, barrier, g.recorder, g.clock)
	g.safety = safety.NewService(safetyCoordinator, g.cycles)

	g.incidents = monitor.NewIncidentReview(store)
	return g
}

func (g *graph) sources() handoff.Sources {
	return handoff.Sources{
		Store: g.store, Cycles: g.cycles, Gates: g.gateState,
		Commands: g.gateCommands, Planner: g.planner, Queue: g.admission,
		Leases: g.leases, Safety: g.safety, Hydraulics: g.hydraulics,
		Incidents: g.incidents,
	}
}

func (g *graph) advance(d time.Duration) { g.now = g.now.Add(d) }

const chamber = "chamber-1"

// buildShiftActivity creates a plan, starts its transit, leaves a gate
// command unacknowledged, grants a lease and records a cancellation incident.
func buildShiftActivity(t *testing.T, g *graph) (model.ConvoyPlan, model.TransitCycle, model.DeviceCommand) {
	t.Helper()
	vessel, err := model.NewVessel("River Star", "RS-204", 86)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := g.planner.Create(g.ctx, chamber, model.DirectionUpstream, []model.Vessel{vessel})
	if err != nil {
		t.Fatal(err)
	}
	g.advance(time.Second)
	transit, err := g.cycleService.Start(g.ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	g.advance(time.Second)
	command, err := g.gates.Request(g.ctx, transit, model.DirectionUpstream, true)
	if err != nil {
		t.Fatal(err)
	}
	g.advance(time.Second)
	if _, err := g.leases.Assign(g.ctx, chamber, vessel.ID, "tug-7", time.Minute); err != nil {
		t.Fatal(err)
	}
	g.advance(time.Second)
	cancelled := plan
	cancelled.Cancelled = true
	if _, err := g.recorder.Record(g.ctx, chamber, model.EventPlanCancelled, plan.ID, 0, cancelled); err != nil {
		t.Fatal(err)
	}
	return plan, transit, command
}

func newTestService(t *testing.T, g *graph) *handoff.Service {
	t.Helper()
	service, err := handoff.NewService(handoff.NewMemoryBookStore(), g.sources(), g.clock)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestOpenGeneratesSnapshotAggregatingShift(t *testing.T) {
	g := newGraph(t, chamber)
	plan, transit, command := buildShiftActivity(t, g)

	service := newTestService(t, g)
	start := g.now.Add(-time.Hour)
	end := g.now.Add(time.Hour)
	book, err := service.Open(g.ctx, handoff.OpenInput{
		ChamberID: chamber, ShiftLabel: "测试班次", Start: start, End: end, Operator: "op-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if book.Status != handoff.StatusOpen || book.Snapshot == nil {
		t.Fatalf("expected open book with snapshot, got %+v", book)
	}
	snap := book.Snapshot
	if snap.CycleCount != 1 || len(snap.Cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", snap.CycleCount)
	}
	if snap.Cycles[0].CycleID != transit.ID {
		t.Fatalf("cycle summary points at %s, want %s", snap.Cycles[0].CycleID, transit.ID)
	}
	if len(snap.Cycles[0].Events) == 0 {
		t.Fatal("cycle summary must carry original event references")
	}
	if snap.Cycles[0].Events[0].Sequence == 0 || snap.Cycles[0].Events[0].ID == uuid.Nil {
		t.Fatal("event reference must keep journal id and sequence")
	}
	if len(snap.Unfinished) != 1 || snap.Unfinished[0].PlanID != plan.ID {
		t.Fatalf("expected the unfinished plan, got %+v", snap.Unfinished)
	}
	if len(snap.PendingCmds) != 1 || snap.PendingCmds[0].CommandID != command.ID {
		t.Fatalf("expected 1 pending command, got %+v", snap.PendingCmds)
	}
	if snap.PendingCmds[0].Event == nil || snap.PendingCmds[0].Event.Sequence == 0 {
		t.Fatal("pending command must reference its original queued event")
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("expected 1 active lease, got %+v", snap.Leases)
	}
	if len(snap.Incidents) != 1 || snap.Incidents[0].Event.Type != string(model.EventPlanCancelled) {
		t.Fatalf("expected 1 incident review item, got %+v", snap.Incidents)
	}
	state := snap.Chambers[0]
	if state.Stage != string(model.StagePlanned) || state.Pending != 1 {
		t.Fatalf("unexpected chamber state: %+v", state)
	}
	if state.Sequence == 0 {
		t.Fatal("chamber state must record journal sequence at capture time")
	}
}

func TestConfirmSealsBookAgainstLaterStateChanges(t *testing.T) {
	g := newGraph(t, chamber)
	_, _, command := buildShiftActivity(t, g)
	service := newTestService(t, g)
	start, end := g.now.Add(-time.Hour), g.now.Add(time.Hour)

	book, err := service.Open(g.ctx, handoff.OpenInput{
		ChamberID: chamber, ShiftLabel: "测试班次", Start: start, End: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SaveNotes(g.ctx, book.ID, "op-1", "下一班注意上游闸门命令"); err != nil {
		t.Fatal(err)
	}
	sealed, err := service.Confirm(g.ctx, book.ID, handoff.ConfirmInput{ConfirmedBy: "op-1"})
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Status != handoff.StatusConfirmed || sealed.ConfirmedAt == nil {
		t.Fatalf("expected sealed book, got %+v", sealed)
	}
	if sealed.Snapshot.PendingCmds[0].CommandID != command.ID {
		t.Fatal("sealed snapshot mismatch")
	}

	// Live state moves on: the pending device command settles after handover.
	ack := model.DeviceAck{
		CommandID: command.ID, CycleID: command.CycleID, ChamberID: command.ChamberID,
		Generation: command.Generation, Epoch: command.Epoch,
		Device: command.Device, Settled: true, ObservedAt: g.now,
	}
	if _, err := g.gateCommands.Settle(ack, g.now); err != nil {
		t.Fatal(err)
	}
	if pending := g.gateCommands.Pending(chamber); len(pending) != 0 {
		t.Fatalf("live command should be settled, got %d pending", len(pending))
	}

	// Refresh must not regenerate a sealed snapshot.
	reopened, err := service.Refresh(g.ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Snapshot.PendingCmds) != 1 || reopened.Snapshot.PendingCmds[0].CommandID != command.ID {
		t.Fatal("sealed snapshot was altered by later state change")
	}
	if reopened.Notes != "下一班注意上游闸门命令" {
		t.Fatal("sealed notes were altered")
	}
	if _, err := service.SaveNotes(g.ctx, book.ID, "op-2", "tamper"); err != handoff.ErrBookSealed {
		t.Fatalf("expected ErrBookSealed on note edit, got %v", err)
	}
	if _, err := service.Confirm(g.ctx, book.ID, handoff.ConfirmInput{ConfirmedBy: "op-2"}); err != handoff.ErrBookSealed {
		t.Fatalf("expected ErrBookSealed on re-confirm, got %v", err)
	}
	_, err = service.Open(g.ctx, handoff.OpenInput{ChamberID: chamber, ShiftLabel: "测试班次", Start: start, End: end})
	if err == nil {
		t.Fatal("reopening a sealed shift window must fail")
	}

	// Stored record survives unchanged.
	stored, ok, err := service.Get(g.ctx, book.ID)
	if err != nil || !ok {
		t.Fatalf("get sealed book: %v %v", ok, err)
	}
	if stored.ConfirmedBy != "op-1" || len(stored.Snapshot.PendingCmds) != 1 {
		t.Fatalf("stored sealed record changed: %+v", stored)
	}
}

func TestListFiltersByChamberAndTime(t *testing.T) {
	g := newGraph(t, chamber)
	buildShiftActivity(t, g)
	service := newTestService(t, g)
	windowStart := g.now.Add(-30 * time.Minute)
	windowEnd := g.now.Add(30 * time.Minute)
	if _, err := service.Open(g.ctx, handoff.OpenInput{
		ChamberID: chamber, ShiftLabel: "班次A", Start: windowStart, End: windowEnd,
	}); err != nil {
		t.Fatal(err)
	}

	otherChamber := "chamber-2"
	bookOther, err := service.Open(g.ctx, handoff.OpenInput{
		ChamberID: otherChamber, ShiftLabel: "班次B",
		Start: g.now.Add(-30 * time.Minute), End: g.now.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	all, err := service.List(g.ctx, "", time.Time{}, time.Time{})
	if err != nil || len(all) != 2 {
		t.Fatalf("expected 2 books across chambers, got %d (%v)", len(all), err)
	}
	one, err := service.List(g.ctx, chamber, time.Time{}, time.Time{})
	if err != nil || len(one) != 1 || one[0].ChamberID != chamber {
		t.Fatalf("chamber filter failed: %+v %v", one, err)
	}
	none, err := service.List(g.ctx, "", time.Time{}, g.now.Add(-2*time.Hour))
	if err != nil || len(none) != 0 {
		t.Fatalf("time filter should exclude all books, got %d (%v)", len(none), err)
	}
	future, err := service.List(g.ctx, "", g.now.Add(2*time.Hour), time.Time{})
	if err != nil || len(future) != 0 {
		t.Fatalf("future time filter should exclude all books, got %d (%v)", len(future), err)
	}
	if _, err := service.List(g.ctx, otherChamber, windowStart, windowEnd); err != nil {
		t.Fatal(err)
	}
	_ = bookOther
}

func TestRefreshUpdatesOpenSnapshotWithCurrentState(t *testing.T) {
	g := newGraph(t, chamber)
	buildShiftActivity(t, g)
	service := newTestService(t, g)
	book, err := service.Open(g.ctx, handoff.OpenInput{
		ChamberID: chamber, ShiftLabel: "测试班次",
		Start: g.now.Add(-time.Hour), End: g.now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := book.Snapshot.PendingCmds[0].State; got != string(model.CommandQueued) {
		t.Fatalf("unexpected initial command state %s", got)
	}

	// The device acknowledges execution while the book is still open.
	command := book.Snapshot.PendingCmds[0]
	if err := g.gateCommands.Accept(command.CommandID, g.now); err != nil {
		t.Fatal(err)
	}
	refreshed, err := service.Refresh(g.ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Snapshot.PendingCmds[0].State != string(model.CommandAccepted) {
		t.Fatalf("expected refreshed command state accepted, got %s", refreshed.Snapshot.PendingCmds[0].State)
	}
}

func TestFileStorePersistsAndRejectsSealedMutation(t *testing.T) {
	g := newGraph(t, chamber)
	buildShiftActivity(t, g)
	dir := t.TempDir()

	store, err := handoff.NewFileBookStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := handoff.NewService(store, g.sources(), g.clock)
	if err != nil {
		t.Fatal(err)
	}
	book, err := service.Open(g.ctx, handoff.OpenInput{
		ChamberID: chamber, ShiftLabel: "测试班次",
		Start: g.now.Add(-time.Hour), End: g.now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := service.Confirm(g.ctx, book.ID, handoff.ConfirmInput{ConfirmedBy: "op-1", Notes: "封存"})
	if err != nil {
		t.Fatal(err)
	}

	// Storage-level guard: a later revision of a sealed book is rejected.
	tampered := sealed
	tampered.Notes = "disk tamper"
	if err := store.Update(g.ctx, tampered); err != handoff.ErrBookSealed {
		t.Fatalf("expected ErrBookSealed from file store, got %v", err)
	}

	// Simulate a process restart: revisions replay to the sealed state.
	reopenedStore, err := handoff.NewFileBookStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := reopenedStore.Get(g.ctx, sealed.ID)
	if err != nil || !ok {
		t.Fatalf("reload sealed book: %v %v", ok, err)
	}
	if loaded.Status != handoff.StatusConfirmed || loaded.Notes != "封存" {
		t.Fatalf("reloaded book mismatch: %+v", loaded)
	}
	if len(loaded.Snapshot.PendingCmds) != 1 {
		t.Fatal("reloaded sealed snapshot lost pending commands")
	}
	listed, err := reopenedStore.List(g.ctx, chamber, g.now.Add(-2*time.Hour), g.now.Add(2*time.Hour))
	if err != nil || len(listed) != 1 || listed[0].Status != handoff.StatusConfirmed {
		t.Fatalf("reloaded list mismatch: %+v %v", listed, err)
	}

	// The on-disk log ends with the sealed revision and carries no tamper row.
	logPath := filepath.Join(dir, "data", "handovers", chamber+".jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "封存") || strings.Contains(string(data), "disk tamper") {
		t.Fatalf("on-disk log does not reflect tamper protection: %s", data)
	}
}

func TestShiftWindows(t *testing.T) {
	cases := []struct {
		hour      int
		wantStart int
		label     string
	}{
		{3, 0, "夜班"}, {10, 8, "早班"}, {20, 16, "晚班"},
	}
	for _, tc := range cases {
		at := time.Date(2026, 9, 30, tc.hour, 30, 0, 0, time.Local)
		start, end, label := handoff.ShiftWindow(at)
		if start.Hour() != tc.wantStart || end.Sub(start) != 8*time.Hour {
			t.Fatalf("hour %d window = %s~%s", tc.hour, start, end)
		}
		if !strings.Contains(label, tc.label) {
			t.Fatalf("hour %d label %q missing %q", tc.hour, label, tc.label)
		}
	}
}
