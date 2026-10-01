package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newHandoverTestServer(t *testing.T) (*Runtime, *httptest.Server) {
	t.Helper()
	runtime, err := NewRuntime(context.Background(), "")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	server := httptest.NewServer(Router(runtime, ""))
	t.Cleanup(server.Close)
	return runtime, server
}

func doJSON(t *testing.T, method, url string, body any) (int, any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode %q: %v", string(raw), err)
		}
	}
	return response.StatusCode, decoded
}

func asMap(t *testing.T, value any) map[string]any {
	t.Helper()
	mapped, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T: %v", value, value)
	}
	return mapped
}

func TestHandoverHTTPLifecycle(t *testing.T) {
	_, server := newHandoverTestServer(t)
	base := server.URL

	status, body := doJSON(t, http.MethodPost, base+"/api/chambers/chamber-1/handovers", map[string]any{})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", status, body)
	}
	book := asMap(t, body)["id"].(string)

	status, listed := doJSON(t, http.MethodGet, base+"/api/handovers?chamber=chamber-1", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	books, ok := listed.([]any)
	if !ok || len(books) != 1 {
		t.Fatalf("listed books = %v", listed)
	}

	status, body = doJSON(t, http.MethodPost, base+"/api/handovers/"+book+"/notes",
		map[string]any{"notes": "二号充水阀响应偏慢", "officer": "zhang"})
	if status != http.StatusOK {
		t.Fatalf("notes status = %d, body = %v", status, body)
	}
	if asMap(t, body)["notes"] != "二号充水阀响应偏慢" {
		t.Fatalf("notes not persisted: %v", body)
	}

	status, body = doJSON(t, http.MethodPost, base+"/api/handovers/"+book+"/confirm",
		map[string]any{"outgoing_officer": "zhang", "incoming_officer": "li"})
	if status != http.StatusOK {
		t.Fatalf("confirm status = %d, body = %v", status, body)
	}
	if asMap(t, body)["seal_intact"] != true {
		t.Fatalf("seal_intact = %v, want true", asMap(t, body)["seal_intact"])
	}

	status, detailValue := doJSON(t, http.MethodGet, base+"/api/handovers/"+book, nil)
	if status != http.StatusOK {
		t.Fatalf("get status = %d", status)
	}
	detail := asMap(t, detailValue)
	if detail["seal_intact"] != true {
		t.Fatalf("detail seal_intact = %v", detail["seal_intact"])
	}
	confirmed := asMap(t, detail["book"])
	if confirmed["status"] != "confirmed" {
		t.Fatalf("stored status = %v", confirmed["status"])
	}
	snapshot := asMap(t, confirmed["snapshot"])
	if _, present := snapshot["event_refs"]; !present {
		t.Fatal("confirmed snapshot must retain original event references")
	}

	// Sealed books reject regeneration and further edits.
	status, body = doJSON(t, http.MethodPost, base+"/api/chambers/chamber-1/handovers", map[string]any{})
	if status != http.StatusConflict {
		t.Fatalf("regenerate confirmed status = %d, body = %v", status, body)
	}
	status, body = doJSON(t, http.MethodPost, base+"/api/handovers/"+book+"/notes",
		map[string]any{"notes": "tampered", "officer": "zhang"})
	if status != http.StatusConflict {
		t.Fatalf("edit confirmed status = %d, body = %v", status, body)
	}
}

func TestHandoverHTTPFiltersAndValidation(t *testing.T) {
	_, server := newHandoverTestServer(t)
	base := server.URL

	// chamber-1 gets a draft.
	status, body := doJSON(t, http.MethodPost, base+"/api/chambers/chamber-1/handovers", map[string]any{})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}

	status, body = doJSON(t, http.MethodGet, base+"/api/handovers?chamber=chamber-2", nil)
	if status != http.StatusOK {
		t.Fatalf("filter status = %d", status)
	}
	if books, _ := body.([]any); len(books) != 0 {
		t.Fatalf("chamber-2 should have no books, got %v", body)
	}

	status, body = doJSON(t, http.MethodGet, base+"/api/handovers?status=bogus", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad status filter code = %d, body = %v", status, body)
	}

	status, body = doJSON(t, http.MethodGet, base+"/api/handovers/not-a-uuid", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad uuid code = %d, body = %v", status, body)
	}

	status, body = doJSON(t, http.MethodGet, base+"/api/handovers/00000000-0000-0000-0000-000000000000", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing book code = %d, body = %v", status, body)
	}

	status, body = doJSON(t, http.MethodPost,
		base+"/api/handovers/00000000-0000-0000-0000-000000000000/confirm",
		map[string]any{"outgoing_officer": "a", "incoming_officer": "b"})
	if status != http.StatusNotFound {
		t.Fatalf("confirm missing code = %d, body = %v", status, body)
	}
}
