package model

import (
	"testing"
	"time"
)

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func TestShiftAtBoundaries(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		kind ShiftKind
	}{
		{"day start", utc(2026, 9, 29, 8, 0), ShiftDay},
		{"day middle", utc(2026, 9, 29, 13, 30), ShiftDay},
		{"just before night", utc(2026, 9, 29, 19, 59), ShiftDay},
		{"night start", utc(2026, 9, 29, 20, 0), ShiftNight},
		{"small hours belong to previous night", utc(2026, 9, 29, 3, 0), ShiftNight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shift := ShiftAt(tc.at)
			if shift.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", shift.Kind, tc.kind)
			}
			if !shift.Contains(tc.at) {
				t.Fatalf("instant %s not contained in shift %s..%s", tc.at, shift.Start, shift.End)
			}
			if shift.End.Sub(shift.Start) != 12*time.Hour {
				t.Fatalf("shift length = %s, want 12h", shift.End.Sub(shift.Start))
			}
		})
	}
	night := ShiftAt(utc(2026, 9, 29, 3, 0))
	wantStart := utc(2026, 9, 28, 20, 0)
	if !night.Start.Equal(wantStart) {
		t.Fatalf("early-morning night start = %s, want %s", night.Start, wantStart)
	}
}

func TestConfirmSealsAndDetectsTampering(t *testing.T) {
	snapshot := HandoverSnapshot{
		TakenAt: utc(2026, 9, 29, 10, 0),
		Shift:   ShiftAt(utc(2026, 9, 29, 10, 0)),
	}
	book, err := NewHandoverBook("chamber-1", snapshot, utc(2026, 9, 29, 19, 55))
	if err != nil {
		t.Fatalf("new book: %v", err)
	}
	if book.Status != HandoverDraft {
		t.Fatalf("new book status = %q, want draft", book.Status)
	}
	if err := book.AmendNotes("二号阀门偏慢", "zhang", utc(2026, 9, 29, 19, 56)); err != nil {
		t.Fatalf("amend: %v", err)
	}
	if err := book.Confirm("zhang", "li", utc(2026, 9, 29, 20, 0)); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	intact, err := book.VerifySeal()
	if err != nil || !intact {
		t.Fatalf("seal verify = (%v,%v), want (true,nil)", intact, err)
	}
	if book.ConfirmedAt == nil || book.Seal == nil || book.Seal.Algorithm != "SHA-256" {
		t.Fatalf("confirmed book missing seal metadata: %+v", book)
	}

	if err := book.Confirm("zhang", "li", utc(2026, 9, 29, 20, 1)); err != ErrHandoverSealed {
		t.Fatalf("second confirm err = %v, want ErrHandoverSealed", err)
	}
	if err := book.AmendNotes("tampered", "zhang", utc(2026, 9, 29, 20, 2)); err != ErrHandoverSealed {
		t.Fatalf("amend after confirm err = %v, want ErrHandoverSealed", err)
	}

	// Direct field mutation simulates tampering with the persisted record.
	book.Notes = "forged remark"
	intact, err = book.VerifySeal()
	if err != nil {
		t.Fatalf("verify after tamper: %v", err)
	}
	if intact {
		t.Fatal("seal still valid after tampering with notes")
	}
}
