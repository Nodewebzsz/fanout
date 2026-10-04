package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTargetAPIFixture(t *testing.T) (*CountryReconciler, *reconcilePanel) {
	t.Helper()
	panel := &reconcilePanel{templateID: 12, nextID: 100}
	usePanel(t, panel)
	store, err := LoadCountryTargetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := NewCountryReconciler(NewManager(20, t.TempDir()), store)
	r.pass = func(*Job) {}
	return r, panel
}

func TestAPITargetsUpsertsMultipleCountries(t *testing.T) {
	r, _ := newTargetAPIFixture(t)
	body := strings.NewReader(`{"template_id":12,"targets":[{"country_code":"JP","target_count":3},{"country_code":"KR","target_count":2}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/targets", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	apiTargets(r)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := r.store.List(); len(got) != 2 {
		t.Fatalf("targets=%+v", got)
	}
	if !strings.Contains(rec.Body.String(), `"started":true`) {
		t.Fatalf("response did not start reconciliation: %s", rec.Body.String())
	}
}

func TestAPITargetsGETReturnsLiveStatus(t *testing.T) {
	r, _ := newTargetAPIFixture(t)
	if _, err := r.store.Upsert("native", 12, []TargetInput{{CountryCode: "JP", TargetCount: 1}}); err != nil {
		t.Fatal(err)
	}
	r.mgr.tunnels[1] = &Tunnel{Slot: 1, Status: "up", TargetID: "native:12:JP"}
	rec := httptest.NewRecorder()
	apiTargets(r)(rec, httptest.NewRequest(http.MethodGet, "/api/targets", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"healthy":1`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIManualReconcileReusesRunningJob(t *testing.T) {
	r, _ := newTargetAPIFixture(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	r.pass = func(*Job) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	defer close(release)

	first := httptest.NewRecorder()
	apiTargetsReconcile(r)(first, httptest.NewRequest(http.MethodPost, "/api/targets/reconcile", nil))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reconcile job did not start")
	}
	second := httptest.NewRecorder()
	apiTargetsReconcile(r)(second, httptest.NewRequest(http.MethodPost, "/api/targets/reconcile", nil))
	if !strings.Contains(second.Body.String(), `"started":false`) {
		t.Fatalf("expected reused job: %s", second.Body.String())
	}
}

func TestAPITargetsRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid json", body: `{`},
		{name: "zero template", body: `{"template_id":0,"targets":[{"country_code":"JP","target_count":1}]}`},
		{name: "empty targets", body: `{"template_id":12,"targets":[]}`},
		{name: "missing template", body: `{"template_id":99,"targets":[{"country_code":"JP","target_count":1}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newTargetAPIFixture(t)
			rec := httptest.NewRecorder()
			apiTargets(r)(rec, httptest.NewRequest(http.MethodPost, "/api/targets", strings.NewReader(tt.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

type readOnlyTargetPanel struct{ *reconcilePanel }

func (p *readOnlyTargetPanel) Kind() string { return "xray-cf-lite" }

func TestAPITargetsRejectsReadOnlyPanel(t *testing.T) {
	r, panel := newTargetAPIFixture(t)
	usePanel(t, &readOnlyTargetPanel{reconcilePanel: panel})
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"template_id":12,"targets":[{"country_code":"JP","target_count":1}]}`)
	apiTargets(r)(rec, httptest.NewRequest(http.MethodPost, "/api/targets", body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTargetAPIsRejectWrongMethods(t *testing.T) {
	r, _ := newTargetAPIFixture(t)
	for _, tc := range []struct {
		handler http.HandlerFunc
		method  string
		path    string
	}{
		{handler: apiTargets(r), method: http.MethodPut, path: "/api/targets"},
		{handler: apiTargetsReconcile(r), method: http.MethodGet, path: "/api/targets/reconcile"},
	} {
		rec := httptest.NewRecorder()
		tc.handler(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status=%d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestExitJSONIncludesTargetID(t *testing.T) {
	blob, err := json.Marshal(Exit{TargetID: "native:12:JP"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"target_id":"native:12:JP"`) {
		t.Fatalf("target ownership missing from exit JSON: %s", blob)
	}
}

func TestManagedHealthFailureRequestsReconciliationAfterTwoChecks(t *testing.T) {
	m := NewManager(20, t.TempDir())
	m.probeExit = func(*Tunnel) (string, error) { return "", errors.New("down") }
	tunnel := &Tunnel{
		Slot: 1, Port: 25000, Status: "up", ExitIP: "203.0.113.7",
		TargetID: "native:12:JP", Node: Node{HostName: "jp1", CountryCode: "JP"},
		Cred: SocksCred{User: "u", Pass: "p"},
	}
	m.tunnels[1] = tunnel
	failures := map[int]int{}
	requested := 0
	callback := func(got *Tunnel) {
		if got != tunnel {
			t.Errorf("unexpected callback tunnel: %+v", got)
		}
		requested++
	}
	m.checkHealth(failures, callback)
	if tunnel.Status != "up" || requested != 0 {
		t.Fatalf("one failure must not evict tunnel: status=%s requested=%d", tunnel.Status, requested)
	}
	m.checkHealth(failures, callback)
	if tunnel.Status != "waiting_fill" || requested != 1 {
		t.Fatalf("managed failure was not queued: status=%s requested=%d", tunnel.Status, requested)
	}
}
