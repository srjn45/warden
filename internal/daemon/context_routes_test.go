package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/ctxstore"
)

func newCtxTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cs, err := ctxstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("ctxstore.New: %v", err)
	}
	return httptest.NewServer((&Server{cstore: cs}).router())
}

func TestContextSetGetRoundTrip(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()

	body := bytes.NewBufferString(`{"value":"hello","by":"agent-A"}`)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/context/global.greeting", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/context/global.greeting")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var e ctxstore.Entry
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Value != "hello" || e.UpdatedBy != "agent-A" {
		t.Fatalf("got %+v", e)
	}
}

func TestContextGetMissing404(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/context/missing")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestContextSetDefaultsWriterToHuman(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/context/k", bytes.NewBufferString(`{"value":"v"}`))
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	putResp.Body.Close()

	resp, err := http.Get(ts.URL + "/context/k")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var e ctxstore.Entry
	json.NewDecoder(resp.Body).Decode(&e)
	if e.UpdatedBy != "human" {
		t.Fatalf("want writer 'human', got %q", e.UpdatedBy)
	}
}

func TestContextCASConflictReturns409(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()

	// Seed a value, then CAS with a stale expected → 409.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/context/lock", bytes.NewBufferString(`{"value":"agent-A"}`))
	put, _ := http.DefaultClient.Do(req)
	put.Body.Close()

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/context/lock/cas", bytes.NewBufferString(`{"expected":"someone-else","value":"agent-B"}`))
	conflict, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("CAS POST: %v", err)
	}
	conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 on stale expected, got %d", conflict.StatusCode)
	}

	// Matching expected → 200 and the value swaps.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/context/lock/cas", bytes.NewBufferString(`{"expected":"agent-A","value":"agent-B"}`))
	ok, _ := http.DefaultClient.Do(req)
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("want 200 on matching expected, got %d", ok.StatusCode)
	}
	resp, _ := http.Get(ts.URL + "/context/lock")
	var e ctxstore.Entry
	json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if e.Value != "agent-B" {
		t.Fatalf("want value agent-B after successful CAS, got %q", e.Value)
	}
}

func TestContextAppend(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()
	for _, v := range []string{"one", "two"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/context/log/append",
			bytes.NewBufferString(`{"value":"`+v+`","sep":"\n"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("append POST: %v", err)
		}
		resp.Body.Close()
	}
	resp, _ := http.Get(ts.URL + "/context/log")
	var e ctxstore.Entry
	json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if e.Value != "one\ntwo" {
		t.Fatalf("want \"one\\ntwo\", got %q", e.Value)
	}
}

func TestContextListAndDelete(t *testing.T) {
	ts := newCtxTestServer(t)
	defer ts.Close()
	for _, k := range []string{"pipeline.p.a", "pipeline.p.b", "global.x"} {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/context/"+k, bytes.NewBufferString(`{"value":"v"}`))
		http.DefaultClient.Do(req)
	}

	resp, _ := http.Get(ts.URL + "/context?prefix=pipeline.p.")
	var lr struct {
		Entries []ctxstore.Entry `json:"entries"`
	}
	json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	if len(lr.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(lr.Entries))
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/context/global.x", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/context/global.x")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 after delete, got %d", resp.StatusCode)
	}
}
