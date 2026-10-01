package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-96/internal/handoff"
)

type handoverOpenInput struct {
	ChamberID  string `json:"chamber_id"`
	ShiftLabel string `json:"shift_label"`
	Start      string `json:"window_start"`
	End        string `json:"window_end"`
	Operator   string `json:"operator"`
}

type handoverNotesInput struct {
	Operator string `json:"operator"`
	Notes    string `json:"notes"`
}

type handoverConfirmInput struct {
	ConfirmedBy string `json:"confirmed_by"`
	Operator    string `json:"operator"`
	Notes       string `json:"notes"`
}

// openHandoverHandler returns the open book for the current/explicit shift,
// creating and snapshotting it on first use.
func openHandoverHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input handoverOpenInput
		if r.ContentLength != 0 {
			if err := decodeJSON(r, &input); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		if input.ChamberID == "" {
			input.ChamberID = r.URL.Query().Get("chamber_id")
		}
		if input.Operator == "" {
			input.Operator = r.URL.Query().Get("operator")
		}
		if input.ChamberID == "" {
			writeError(w, http.StatusBadRequest, errors.New("chamber_id is required"))
			return
		}
		start, end, err := parseWindow(input.Start, input.End)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		book, err := runtime.Handovers.Open(r.Context(), handoff.OpenInput{
			ChamberID: input.ChamberID, ShiftLabel: input.ShiftLabel,
			Start: start, End: end, Operator: input.Operator,
		})
		if err != nil {
			writeHandoverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, book)
	}
}

func listHandoversHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		from, err := parseTimeQuery(query.Get("from"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		to, err := parseTimeQuery(query.Get("to"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		books, err := runtime.Handovers.List(r.Context(), query.Get("chamber_id"), from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, books)
	}
}

func getHandoverHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUID(chi.URLParam(r, "bookID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		book, ok, err := runtime.Handovers.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("handover book not found"))
			return
		}
		writeJSON(w, http.StatusOK, book)
	}
}

func refreshHandoverHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUID(chi.URLParam(r, "bookID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		book, err := runtime.Handovers.Refresh(r.Context(), id)
		if err != nil {
			writeHandoverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, book)
	}
}

func notesHandoverHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUID(chi.URLParam(r, "bookID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var input handoverNotesInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		book, err := runtime.Handovers.SaveNotes(r.Context(), id, input.Operator, input.Notes)
		if err != nil {
			writeHandoverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, book)
	}
}

func confirmHandoverHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUID(chi.URLParam(r, "bookID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var input handoverConfirmInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		book, err := runtime.Handovers.Confirm(r.Context(), id, handoff.ConfirmInput{
			ConfirmedBy: input.ConfirmedBy, Operator: input.Operator, Notes: input.Notes,
		})
		if err != nil {
			writeHandoverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, book)
	}
}

func writeHandoverError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, handoff.ErrBookSealed):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusUnprocessableEntity, err)
	}
}

func parseWindow(start, end string) (time.Time, time.Time, error) {
	if (start == "") != (end == "") {
		return time.Time{}, time.Time{}, errors.New("window_start and window_end must be provided together")
	}
	if start == "" {
		return time.Time{}, time.Time{}, nil
	}
	from, err := parseTimeQuery(start)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parseTimeQuery(end)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return from, to, nil
}

func parseTimeQuery(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, errors.New("invalid time, use RFC3339 or 2006-01-02 format")
}
