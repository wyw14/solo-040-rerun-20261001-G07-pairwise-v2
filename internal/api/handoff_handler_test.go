package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wyw14/cry-96/internal/handoff"
)

func newHandoverTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	runtime, err := NewRuntime(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	webDir := filepath.Join("..", "..", "web")
	server := httptest.NewServer(Router(runtime, webDir))
	return server, server.Close
}

func doJSON(t *testing.T, server *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		decoded = map[string]any{}
	}
	return response.StatusCode, decoded
}

func TestHandoverHTTPEndToEnd(t *testing.T) {
	server, closeServer := newHandoverTestServer(t)
	defer closeServer()

	status, body := doJSON(t, server, http.MethodGet, "/api/handovers?chamber_id=chamber-1", "")
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}

	// Open/generate the current shift book.
	now := time.Now().UTC()
	status, book := doJSON(t, server, http.MethodPost, "/api/handovers/open",
		`{"chamber_id":"chamber-1","operator":"op-1"}`)
	if status != http.StatusOK {
		t.Fatalf("open status = %d body=%v", status, book)
	}
	id, _ := book["id"].(string)
	if id == "" || book["status"] != string(handoff.StatusOpen) {
		t.Fatalf("unexpected open book: %v", book)
	}
	snapshot, _ := book["snapshot"].(map[string]any)
	if snapshot == nil || snapshot["window_start"] == nil {
		t.Fatal("book must carry a generated snapshot with shift window")
	}
	windowStart, _ := time.Parse(time.RFC3339, snapshot["window_start"].(string))
	if now.Add(-24 * time.Hour).After(windowStart) {
		t.Fatalf("window start %s not within the current day", windowStart)
	}

	// Add duty notes.
	status, body = doJSON(t, server, http.MethodPost, "/api/handovers/"+id+"/notes",
		`{"operator":"op-1","notes":"上游闸门检修后复紧"}`)
	if status != http.StatusOK || body["notes"] != "上游闸门检修后复紧" {
		t.Fatalf("notes status=%d body=%v", status, body)
	}

	// Refresh the open snapshot.
	status, body = doJSON(t, server, http.MethodPost, "/api/handovers/"+id+"/refresh", "")
	if status != http.StatusOK || body["status"] != string(handoff.StatusOpen) {
		t.Fatalf("refresh status=%d body=%v", status, body)
	}

	// Confirm and seal.
	status, body = doJSON(t, server, http.MethodPost, "/api/handovers/"+id+"/confirm",
		`{"confirmed_by":"op-1","operator":"op-1","notes":"上游闸门检修后复紧"}`)
	if status != http.StatusOK || body["status"] != string(handoff.StatusConfirmed) {
		t.Fatalf("confirm status=%d body=%v", status, body)
	}
	if body["confirmed_at"] == nil || body["confirmed_by"] != "op-1" {
		t.Fatalf("sealed book missing confirmation metadata: %v", body)
	}

	// Confirmed records reject further edits with 409.
	status, body = doJSON(t, server, http.MethodPost, "/api/handovers/"+id+"/notes",
		`{"operator":"op-2","notes":"hacked"}`)
	if status != http.StatusConflict {
		t.Fatalf("sealed note edit status = %d, want 409", status)
	}
	status, _ = doJSON(t, server, http.MethodPost, "/api/handovers/"+id+"/confirm",
		`{"confirmed_by":"op-2"}`)
	if status != http.StatusConflict {
		t.Fatalf("re-confirm status = %d, want 409", status)
	}

	// Same shift window cannot be reopened.
	status, body = doJSON(t, server, http.MethodPost, "/api/handovers/open",
		`{"chamber_id":"chamber-1"}`)
	if status == http.StatusOK {
		t.Fatalf("sealed shift window must not reopen, got %v", body)
	}

	// Query page: filtered by chamber and time.
	listRequest, _ := http.NewRequest(http.MethodGet,
		server.URL+"/api/handovers?chamber_id=chamber-1&from=2000-01-01&to=2100-01-01", nil)
	listResponse, err := http.DefaultClient.Do(listRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("list sealed status = %d", listResponse.StatusCode)
	}
	rawList, err := io.ReadAll(listResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	var books []map[string]any
	if err := json.Unmarshal(rawList, &books); err != nil {
		t.Fatalf("list is not an array: %v (%s)", err, rawList)
	}
	if len(books) != 1 || books[0]["status"] != string(handoff.StatusConfirmed) {
		t.Fatalf("list must contain the sealed book: %s", rawList)
	}

	// HTML page is served.
	response, err := http.Get(server.URL + "/handovers")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d", response.StatusCode)
	}
}

func TestHandoverValidation(t *testing.T) {
	server, closeServer := newHandoverTestServer(t)
	defer closeServer()

	status, _ := doJSON(t, server, http.MethodPost, "/api/handovers/open", `{}`)
	if status != http.StatusBadRequest {
		t.Fatalf("missing chamber status = %d, want 400", status)
	}
	status, _ = doJSON(t, server, http.MethodGet, "/api/handovers/not-a-uuid", "")
	if status != http.StatusBadRequest {
		t.Fatalf("bad uuid status = %d, want 400", status)
	}
	status, _ = doJSON(t, server, http.MethodGet, "/api/handovers/00000000-0000-0000-0000-000000000000", "")
	if status != http.StatusNotFound {
		t.Fatalf("missing book status = %d, want 404", status)
	}
	status, _ = doJSON(t, server, http.MethodGet, "/api/handovers?from=not-a-time", "")
	if status != http.StatusBadRequest {
		t.Fatalf("bad time filter status = %d, want 400", status)
	}
}
