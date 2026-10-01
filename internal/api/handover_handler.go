package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/handover"
	"github.com/wyw14/cry-96/internal/model"
)

type generateHandoverInput struct {
	At string `json:"at,omitempty"`
}

func createHandoverHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input generateHandoverInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		chamberID := chi.URLParam(r, "chamberID")
		var at time.Time
		if input.At != "" {
			parsed, err := time.Parse(time.RFC3339, input.At)
			if err != nil {
				writeError(w, http.StatusBadRequest, errors.New("at must be an RFC3339 timestamp"))
				return
			}
			at = parsed
		}
		book, err := runtime.Handovers.Generate(r.Context(), chamberID, at)
		if err != nil {
			handoverError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, book)
	}
}

func listHandoversHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter := handover.BookFilter{
			ChamberID: r.URL.Query().Get("chamber"),
			Status:    model.HandoverStatus(r.URL.Query().Get("status")),
		}
		if value := r.URL.Query().Get("from"); value != "" {
			from, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeError(w, http.StatusBadRequest, errors.New("from must be an RFC3339 timestamp"))
				return
			}
			filter.From = from
		}
		if value := r.URL.Query().Get("to"); value != "" {
			to, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeError(w, http.StatusBadRequest, errors.New("to must be an RFC3339 timestamp"))
				return
			}
			filter.To = to
		}
		if filter.Status != "" &&
			filter.Status != model.HandoverDraft && filter.Status != model.HandoverConfirmed {
			writeError(w, http.StatusBadRequest, errors.New("status must be draft or confirmed"))
			return
		}
		books, err := runtime.Handovers.List(r.Context(), filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, books)
	}
}

func getHandoverHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bookID, err := uuid.Parse(chi.URLParam(r, "bookID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		book, ok, err := runtime.Handovers.Get(r.Context(), bookID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("handover book not found"))
			return
		}
		response := map[string]any{"book": book}
		if book.Seal != nil {
			intact, verifyErr := book.VerifySeal()
			if verifyErr == nil {
				response["seal_intact"] = intact
			}
		}
		writeJSON(w, http.StatusOK, response)
	}
}

type handoverNotesInput struct {
	Notes   string `json:"notes"`
	Officer string `json:"officer"`
}

func amendHandoverNotesHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input handoverNotesInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		bookID, err := uuid.Parse(chi.URLParam(r, "bookID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		book, err := runtime.Handovers.AmendNotes(r.Context(), bookID, input.Notes, input.Officer)
		if err != nil {
			handoverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, book)
	}
}

type confirmHandoverInput struct {
	OutgoingOfficer string `json:"outgoing_officer"`
	IncomingOfficer string `json:"incoming_officer"`
}

func confirmHandoverHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input confirmHandoverInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if input.OutgoingOfficer == "" || input.IncomingOfficer == "" {
			writeError(w, http.StatusBadRequest, errors.New("outgoing and incoming officers are required to confirm"))
			return
		}
		bookID, err := uuid.Parse(chi.URLParam(r, "bookID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		book, err := runtime.Handovers.Confirm(r.Context(), bookID, input.OutgoingOfficer, input.IncomingOfficer)
		if err != nil {
			handoverError(w, err)
			return
		}
		intact, _ := book.VerifySeal()
		writeJSON(w, http.StatusOK, map[string]any{"book": book, "seal_intact": intact})
	}
}

func handoverError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrHandoverSealed):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, handover.ErrBookNotFound):
		writeError(w, http.StatusNotFound, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
