# Multi-Country Exit Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Add persistent per-template/per-country exit targets, multi-country creation, full-path health checks, and automatic or manual same-country backfilling.

**Architecture:** Persist CountryTarget records separately from tunnel runtime state, attach a stable target ID to managed tunnels, and add a single-flight CountryReconciler that repairs reserved slots before creating missing ones. Existing unmanaged tunnels retain current behavior; startup, scheduled, health-triggered, and manual reconciliation share one coordinator.

**Tech Stack:** Go 1.24, Go standard library, golang.org/x/sys, Linux network namespaces/OpenVPN/iptables, embedded HTML/CSS/JavaScript.

## Global Constraints

- Runtime remains Linux-only and requires root, network namespaces, iptables, OpenVPN, curl, and /dev/net/tun.
- Add no Go dependency; use the existing curl runtime dependency for SOCKS5 probes.
- A target key is panel kind + template inbound ID + uppercase country code.
- Existing tunnels without target_id remain unmanaged and are not counted or migrated.
- Check running managed exits every 60 seconds and require two consecutive failures.
- Reconcile deficits at startup, every 15 minutes, and on manual request.
- Cool down failed candidates for 15 minutes and do not retry them in the same pass.
- Replace only with a candidate from the same country.
- Lowering a target never deletes healthy excess exits automatically.
- Permit one reconciliation job; a trigger during a run queues one additional pass.
- The development Mac has Go 1.22.2 while go.mod requires Go 1.24. Run tests in Go 1.24 Linux, for example: docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./....

---

## File Structure

- Create country_targets.go and country_targets_test.go for the persistent model/store.
- Create health_probe.go and health_probe_test.go for SOCKS5 full-path probing.
- Create managed_tunnel.go and managed_tunnel_test.go for one-pass lifecycle operations.
- Create reconciler.go and reconciler_test.go for single-flight reconciliation and cooldown.
- Create targets_api_test.go and web_targets_test.go for API/UI contracts.
- Modify tunnel.go, state.go, state_test.go, manager.go, health.go, and exits.go for ownership, persistence, probing, and repair.
- Modify panel.go, native.go, xui.go, panel_xcl.go, provision.go, main.go, and panel fakes for clone identity and rollback.
- Modify main.go and web.go for APIs, scheduler, multi-country wizard, status, and manual trigger.
- Modify README.md for operator documentation.

---

### Task 1: Persistent target model and tunnel ownership

**Files:**
- Create: country_targets.go
- Create: country_targets_test.go
- Modify: tunnel.go:23-49
- Modify: state.go:11-64,89-120
- Modify: state_test.go:14-126

**Interfaces:**
- Produces: CountryTarget, TargetInput, CountryTargetStore, LoadCountryTargetStore(string), Upsert, List, targetID, and Tunnel.TargetID.
- Consumes: the atomic JSON pattern in saveState() and Panel.Kind() values.

- [ ] **Step 1: Write failing persistence and ownership tests**

~~~go
func TestCountryTargetStoreUpsertPersistsStableID(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadCountryTargetStore(dir)
	if err != nil { t.Fatal(err) }
	got, err := s.Upsert("native", 12, []TargetInput{
		{CountryCode: "jp", TargetCount: 3},
		{CountryCode: "KR", TargetCount: 2},
	})
	if err != nil { t.Fatal(err) }
	if got[0].ID != "native:12:JP" || got[0].CountryCode != "JP" {
		t.Fatalf("unexpected target: %+v", got[0])
	}
	reloaded, err := LoadCountryTargetStore(dir)
	if err != nil { t.Fatal(err) }
	if list := reloaded.List(); len(list) != 2 {
		t.Fatalf("targets did not persist: %+v", list)
	}
}

func TestCountryTargetStoreRejectsInvalidInput(t *testing.T) {
	s, err := LoadCountryTargetStore(t.TempDir())
	if err != nil { t.Fatal(err) }
	for _, in := range []TargetInput{
		{CountryCode: "", TargetCount: 1},
		{CountryCode: "J", TargetCount: 1},
		{CountryCode: "JP", TargetCount: 0},
	} {
		if _, err := s.Upsert("native", 12, []TargetInput{in}); err == nil {
			t.Fatalf("expected rejection for %+v", in)
		}
	}
}

func TestSaveStateKeepsTargetID(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(20, dir)
	m.tunnels[1] = &Tunnel{
		Slot: 1, Port: 12345, Status: "waiting_fill",
		TargetID: "native:12:JP",
		Node: Node{HostName: "jp1", CountryCode: "JP"},
		Cred: SocksCred{User: "u", Pass: "p"},
	}
	if err := m.saveState(); err != nil { t.Fatal(err) }
	var st persistedState
	blob, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if err := json.Unmarshal(blob, &st); err != nil { t.Fatal(err) }
	if st.Tunnels[0].TargetID != "native:12:JP" {
		t.Fatalf("target id lost: %+v", st.Tunnels[0])
	}
}
~~~

- [ ] **Step 2: Run tests and confirm failure**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TestCountryTargetStore|TestSaveStateKeepsTargetID' -count=1
~~~

Expected: compilation fails because target types and Tunnel.TargetID do not exist.

- [ ] **Step 3: Implement target types and store**

~~~go
type TargetInput struct {
	CountryCode string
	TargetCount int
}

type CountryTarget struct {
	ID              string
	PanelKind       string
	TemplateID      int
	CountryCode     string
	TargetCount     int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastReconcileAt time.Time
	LastError       string
}

type CountryTargetStore struct {
	mu      sync.RWMutex
	path    string
	targets map[string]CountryTarget
	now     func() time.Time
}

func targetID(panelKind string, templateID int, countryCode string) string {
	return fmt.Sprintf("%s:%d:%s", panelKind, templateID,
		strings.ToUpper(strings.TrimSpace(countryCode)))
}
~~~

Add the JSON field names from the design: target_id, panel_kind, template_id, country_code, target_count, created_at, updated_at, last_reconcile_at, and last_error. Implement load, deterministic upsert, validation, sorted snapshots, result updates, and saveLocked() using mode 0600, a .tmp file, and os.Rename.

Add TargetID with JSON field target_id and omitempty to Tunnel and persistedTunnel. Save and restore it. Old state files must decode with an empty target ID.

- [ ] **Step 4: Run focused and full tests**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TestCountryTargetStore|TestSaveState|TestRestoreState' -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./...
~~~

Expected: all tests pass.

- [ ] **Step 5: Commit**

~~~bash
git add country_targets.go country_targets_test.go tunnel.go state.go state_test.go
git commit -m "feat: persist country exit targets"
~~~

---

### Task 2: Return clone identities and roll back partial clones

**Files:**
- Modify: panel.go:16-56
- Modify: native.go:212-279
- Modify: xui.go:625-707
- Modify: panel_xcl.go:374-384
- Modify: provision.go:106-118
- Modify: main.go:587-631
- Modify: sub_test.go:1-60
- Modify: native_test.go
- Modify: xui_test.go

**Interfaces:**
- Produces: ClonedInbound and Panel.CloneToTunnels(int, []string, []*Tunnel) returning []ClonedInbound.
- Consumes: Panel.DeleteInbounds for rollback.

- [ ] **Step 1: Add failing clone identity and rollback tests**

~~~go
func nativeFixture(t *testing.T) *Native {
	t.Helper()
	dir := t.TempDir()
	return &Native{
		dir: dir,
		store: &nativeStore{
			NextID: 2,
			Inbounds: []*nativeInbound{{
				ID: 1, Port: 10001, Protocol: "vless", Enable: true,
				Clients: []nativeClient{{Email: "template", ID: "uuid", Enable: true}},
			}},
		},
		proc: &xrayProc{bin: "/bin/true", dir: dir},
	}
}

func TestNativeCloneReturnsIdentity(t *testing.T) {
	n := nativeFixture(t)
	tn := &Tunnel{Status: "up", Node: Node{HostName: "jp1"}}
	created, err := n.CloneToTunnels(1, []string{"jp1"}, []*Tunnel{tn})
	if err != nil { t.Fatal(err) }
	if len(created) != 1 || created[0].ID == 0 ||
		created[0].Port == 0 || created[0].HostName != "jp1" {
		t.Fatalf("clone identity missing: %+v", created)
	}
}
~~~

Extend the XUI test server so a bind fails after addInbound succeeds. Assert the new inbound ID is deleted before CloneToTunnels returns the bind error.

- [ ] **Step 2: Run clone tests and confirm failure**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TestNativeCloneReturnsIdentity|TestXUICloneRollsBack' -count=1
~~~

Expected: compilation fails because clone results are []int.

- [ ] **Step 3: Change the panel contract**

~~~go
type ClonedInbound struct {
	ID       int
	Port     int
	HostName string
}
~~~

Use JSON names id, port, and hostname. Return []ClonedInbound from each implementation and fake. Native uses clone.ID, clone.Port, and host. XUI uses newID, port, and host. Track IDs created by the call. On a later add, client attachment, or bind error, delete every newly created inbound and return the original error. Native restores its prior store snapshot and valid Xray config.

Update old callers to use len(created). Preserve any legacy HTTP ports response by mapping each result's Port.

- [ ] **Step 4: Run clone and regression tests**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'Clone|Panel|Provision|Sub' -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./...
~~~

Expected: all tests pass.

- [ ] **Step 5: Commit**

~~~bash
git add panel.go native.go native_test.go xui.go xui_test.go panel_xcl.go provision.go main.go sub_test.go
git commit -m "refactor: return cloned inbound identities"
~~~

---

### Task 3: Full SOCKS5 path health probe

**Files:**
- Create: health_probe.go
- Create: health_probe_test.go
- Modify: manager.go:13-30,219-238
- Modify: health.go:10-63

**Interfaces:**
- Produces: probeExitThroughSOCKS, probeCommand, and defaultProbeCommand.
- Consumes: tunnel port/credentials and curl.

- [ ] **Step 1: Write failing fallback and mismatch tests**

~~~go
func TestProbeExitThroughSOCKSFallsBack(t *testing.T) {
	var calls int
	run := func(name string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 { return nil, errors.New("primary down") }
		return []byte("203.0.113.7\n"), nil
	}
	tn := &Tunnel{Port: 23456, Cred: SocksCred{User: "u", Pass: "p"}}
	ip, err := probeExitThroughSOCKS(tn, run)
	if err != nil || ip != "203.0.113.7" || calls != 2 {
		t.Fatalf("fallback failed: ip=%q calls=%d err=%v", ip, calls, err)
	}
}

func TestTunnelHealthyRejectsChangedExitIP(t *testing.T) {
	m := NewManager(20, t.TempDir())
	m.probeExit = func(*Tunnel) (string, error) {
		return "203.0.113.8", nil
	}
	if m.tunnelHealthy(&Tunnel{ExitIP: "203.0.113.7"}) {
		t.Fatal("changed exit IP must be unhealthy")
	}
}
~~~

Also assert command arguments include --socks5-hostname, 127.0.0.1:<port>, --proxy-user, and credentials.

- [ ] **Step 2: Run tests and confirm failure**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TestProbeExitThroughSOCKS|TestTunnelHealthy' -count=1
~~~

Expected: compilation fails because probe functions and hook are missing.

- [ ] **Step 3: Implement the probe and switch both validation paths**

~~~go
type probeCommand func(name string, args ...string) ([]byte, error)

var probeURLs = []string{
	"https://api.ipify.org",
	"https://ipv4.icanhazip.com",
}

func defaultProbeCommand(name string, args ...string) ([]byte, error) {
	return cmdOutput(exec.Command(name, args...))
}
~~~

For each URL run curl with -fsS, --max-time 8, --socks5-hostname 127.0.0.1:<port>, and --proxy-user <user>:<pass>. Accept only net.ParseIP(value).To4() != nil. Aggregate endpoint failures when neither works.

Add probeExit func(*Tunnel) (string, error) to Manager and initialize it in NewManager. Use it in tryNode() after t.serve() and in tunnelHealthy(). Change healthInterval to 60 seconds and retain healthFailures = 2.

- [ ] **Step 4: Run tests**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'Health|Probe|TryNode|Socks' -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./...
~~~

Expected: all tests pass without external network calls.

- [ ] **Step 5: Commit**

~~~bash
git add health_probe.go health_probe_test.go manager.go health.go
git commit -m "feat: verify exits through socks5"
~~~

---

### Task 4: Managed tunnel lifecycle primitives

**Files:**
- Create: managed_tunnel.go
- Create: managed_tunnel_test.go
- Modify: manager.go:63-109,219-238,287-305
- Modify: tunnel.go:23-49

**Interfaces:**
- Produces: allocateTunnel, StartManagedCandidate, RepairManagedCandidate, and MarkWaitingFill.
- Consumes: Tunnel.TargetID, tryNode, rebind, saveState, and panel sync.

- [ ] **Step 1: Write failing lifecycle tests**

~~~go
func TestManagedCandidateFailureRemovesNewSlot(t *testing.T) {
	m := NewManager(20, t.TempDir())
	m.tryNodeFn = func(*Tunnel) error { return errors.New("dial failed") }
	_, err := m.StartManagedCandidate(
		Node{HostName: "jp1", CountryCode: "JP"}, "native:12:JP")
	if err == nil { t.Fatal("expected failure") }
	if len(m.Tunnels()) != 0 {
		t.Fatalf("failed new slot leaked: %+v", m.Tunnels())
	}
}

func TestRepairManagedCandidateKeepsIdentity(t *testing.T) {
	m := NewManager(20, t.TempDir())
	m.tryNodeFn = func(tn *Tunnel) error {
		tn.ExitIP = "203.0.113.9"
		return nil
	}
	m.rebindFn = func(old string, tn *Tunnel) error { return nil }
	tn := &Tunnel{
		Slot: 4, Port: 25000, Status: "waiting_fill",
		TargetID: "native:12:JP",
		Node: Node{HostName: "jp-old", CountryCode: "JP"},
		Cred: SocksCred{User: "u", Pass: "p"},
	}
	m.tunnels[4] = tn
	if err := m.RepairManagedCandidate(
		tn, Node{HostName: "jp-new", CountryCode: "JP"}); err != nil {
		t.Fatal(err)
	}
	if tn.Slot != 4 || tn.Port != 25000 ||
		tn.TargetID != "native:12:JP" || tn.Status != "up" {
		t.Fatalf("identity changed: %+v", tn)
	}
}
~~~

- [ ] **Step 2: Run tests and confirm failure**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TestManagedCandidate|TestRepairManaged' -count=1
~~~

Expected: compilation fails because managed methods and hooks do not exist.

- [ ] **Step 3: Factor allocation and add one-pass operations**

Create:

~~~go
func (m *Manager) allocateTunnel(node Node, targetID string) (*Tunnel, error)
~~~

It allocates slot, port, and credentials; assigns TargetID; inserts the tunnel; and starts no goroutine. Start calls it with an empty target ID and launches existing unmanaged bringUp.

StartManagedCandidate tries exactly one supplied node synchronously. On failure it stops and removes the new tunnel. On success it marks up, saves, and syncs the panel.

RepairManagedCandidate rejects another country, preserves slot/port/credentials/target ID, records prevHost, tears down OpenVPN/netns while retaining the listener, tries once, rebinds, clears prevHost, and saves.

~~~go
func (m *Manager) MarkWaitingFill(t *Tunnel, cause error) {
	t.Status = "waiting_fill"
	t.ExitIP = ""
	t.Err = firstLine(cause.Error())
	_ = m.saveState()
}
~~~

Add narrow tryNodeFn and rebindFn hooks initialized to real methods in NewManager.

- [ ] **Step 4: Run lifecycle and full tests**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'Managed|Repair|SaveState|RestoreState|Start|Stop' -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./...
~~~

Expected: all tests pass.

- [ ] **Step 5: Commit**

~~~bash
git add managed_tunnel.go managed_tunnel_test.go manager.go tunnel.go
git commit -m "feat: add managed tunnel lifecycle"
~~~

---

### Task 5: Country reconciler, cooldown, and status

**Files:**
- Create: reconciler.go
- Create: reconciler_test.go
- Modify: country_targets.go
- Modify: job.go only if an active-job accessor is needed

**Interfaces:**
- Produces: NewCountryReconciler, Trigger, Statuses, RunOnce, RequestRepair, calculateTargetStatus, and filterCandidates.
- Consumes: target store, managed lifecycle, Panel clone/delete, manager state, and JobStore.

- [ ] **Step 1: Write failing status, candidate, and single-flight tests**

~~~go
func TestTargetStatusCountsWaitingSlotWithoutDuplicating(t *testing.T) {
	target := CountryTarget{
		ID: "native:12:JP", TargetCount: 3, CountryCode: "JP",
	}
	tunnels := []*Tunnel{
		{TargetID: target.ID, Status: "up"},
		{TargetID: target.ID, Status: "up"},
		{TargetID: target.ID, Status: "waiting_fill"},
	}
	st := calculateTargetStatus(target, tunnels)
	if st.Healthy != 2 || st.WaitingFill != 1 || st.Missing != 0 {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestCandidatesStayInCountryAndRespectCooldown(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := &CountryReconciler{
		now: func() time.Time { return now },
		cooldown: map[string]time.Time{"jp1": now.Add(time.Minute)},
	}
	got := r.filterCandidates("JP", []Node{
		{HostName: "jp1", CountryCode: "JP", SpeedMbps: 500},
		{HostName: "us1", CountryCode: "US", SpeedMbps: 900},
		{HostName: "jp2", CountryCode: "JP", SpeedMbps: 300},
	}, nil)
	if len(got) != 1 || got[0].HostName != "jp2" {
		t.Fatalf("unexpected candidates: %+v", got)
	}
}
~~~

Use channels and atomics for a third test: block the first pass, call Trigger twice, release it, and assert maximum concurrency 1 and total passes 2.

- [ ] **Step 2: Run tests and confirm failure**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TestTargetStatus|TestCandidates|TestTrigger' -count=1
~~~

Expected: compilation fails because reconciler types are missing.

- [ ] **Step 3: Implement status and filtering**

~~~go
type CountryTargetStatus struct {
	CountryTarget
	Healthy       int
	Starting      int
	WaitingFill   int
	Missing       int
	Excess        int
	LastCheckedAt time.Time
	NextCheckAt   time.Time
}
~~~

Use JSON names healthy, starting, waiting_fill, missing, excess, last_checked_at, and next_check_at. Count every managed slot toward occupancy but only up toward health. Missing is max(0, target count - managed slots); Excess is max(0, managed slots - target count). Filter by country, residential setting, unused hostname, current-pass failure, and cooldown. Sort speed descending, sessions ascending, hostname ascending.

- [ ] **Step 4: Implement single-flight reconciliation**

~~~go
type CountryReconciler struct {
	mu       sync.Mutex
	mgr      *Manager
	store    *CountryTargetStore
	active   *Job
	pending  bool
	cooldown map[string]time.Time
	now      func() time.Time
	pass     func(*Job)
}
~~~

Trigger returns the active job with started=false and sets pending while running. After RunOnce, execute one additional pass if pending. Clear active only after Job.Finish.

Each pass refreshes VPN Gate, validates panel kind/template, repairs waiting slots first, recomputes status, creates only Missing slots, clones one inbound per new slot, rolls back clone/tunnel failure, cools failed candidates for 15 minutes, updates target result fields, and ends without sleeping when candidates are exhausted.

- [ ] **Step 5: Add scenario tests**

Test partial JP creation, no US substitution, repair-before-create, clone rollback, excess reporting without deletion, one candidate attempt per pass, and eligibility after advancing the fake clock 15 minutes.

- [ ] **Step 6: Run reconciler and full tests**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TargetStatus|Candidates|Trigger|Reconcile|Cooldown' -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./...
~~~

Expected: all tests pass.

- [ ] **Step 7: Commit**

~~~bash
git add reconciler.go reconciler_test.go country_targets.go country_targets_test.go job.go
git commit -m "feat: reconcile country exit targets"
~~~

---

### Task 6: HTTP APIs, scheduler, and health-triggered repair

**Files:**
- Create: targets_api_test.go
- Modify: main.go:82-142,434-473
- Modify: health.go:19-118
- Modify: exits.go:18-34,87-134
- Modify: reconciler.go

**Interfaces:**
- Produces: apiTargets, apiTargetsReconcile, RunScheduler, and the managed health callback.
- Consumes: reconciler trigger/status and authenticated mux.

- [ ] **Step 1: Write failing API tests**

~~~go
type targetFakePanel struct{ fakePanel }

func (p *targetFakePanel) Kind() string { return "native" }
func (p *targetFakePanel) InboundDetail(id int, publicHost string) (*InboundDetail, error) {
	if id != 12 { return nil, fmt.Errorf("入站 %d 不存在", id) }
	return &InboundDetail{Inbound: Inbound{ID: 12, Enable: true}}, nil
}

func newTargetAPIFixture(t *testing.T) *CountryReconciler {
	t.Helper()
	usePanel(t, &targetFakePanel{})
	store, err := LoadCountryTargetStore(t.TempDir())
	if err != nil { t.Fatal(err) }
	r := NewCountryReconciler(NewManager(20, t.TempDir()), store)
	r.pass = func(*Job) {}
	return r
}

func TestAPITargetsUpsertsMultipleCountries(t *testing.T) {
	r := newTargetAPIFixture(t)
	body := strings.NewReader("{\"template_id\":12,\"targets\":[{\"country_code\":\"JP\",\"target_count\":3},{\"country_code\":\"KR\",\"target_count\":2}]}")
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
}

func TestAPIManualReconcileReusesRunningJob(t *testing.T) {
	r := newTargetAPIFixture(t)
	release := make(chan struct{})
	r.pass = func(*Job) { <-release }
	defer close(release)
	first := httptest.NewRecorder()
	apiTargetsReconcile(r)(first,
		httptest.NewRequest(http.MethodPost, "/api/targets/reconcile", nil))
	second := httptest.NewRecorder()
	apiTargetsReconcile(r)(second,
		httptest.NewRequest(http.MethodPost, "/api/targets/reconcile", nil))
	if !strings.Contains(second.Body.String(), "\"started\":false") {
		t.Fatalf("expected reused job: %s", second.Body.String())
	}
}
~~~

Also test GET, invalid JSON, zero template ID, empty targets, xray-cf-lite rejection, missing template, and wrong methods.

- [ ] **Step 2: Run API tests and confirm failure**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TestAPITargets|TestAPIManualReconcile' -count=1
~~~

Expected: compilation fails because handlers are missing.

- [ ] **Step 3: Implement and register APIs**

~~~go
type upsertTargetsRequest struct {
	TemplateID int
	Targets    []TargetInput
}
~~~

Use JSON names template_id and targets. Register /api/targets and /api/targets/reconcile. Derive panel kind from openPanel, reject xray-cf-lite, verify InboundDetail, persist entries, and trigger. GET returns Statuses. POST reconcile returns job and started. Add TargetID to Exit JSON.

- [ ] **Step 4: Wire startup, schedule, and health**

~~~go
targetStore, err := LoadCountryTargetStore(*workDir)
if err != nil {
	log.Fatalf("加载国家出口目标失败: %v", err)
}
reconciler := NewCountryReconciler(mgr, targetStore)
reconciler.Trigger("startup")
go reconciler.RunScheduler(15 * time.Minute)
go mgr.WatchHealth(reconciler.RequestRepair)
~~~

Change WatchHealth to accept func(*Tunnel). Managed tunnels become waiting_fill and call the callback after two failures; unmanaged tunnels retain reconnect. A managed failure during a run sets pending.

- [ ] **Step 5: Run API, health, and full tests**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'API.*Target|ManualReconcile|WatchHealth|Scheduler|Exit' -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./...
~~~

Expected: all tests pass.

- [ ] **Step 6: Commit**

~~~bash
git add targets_api_test.go main.go health.go exits.go reconciler.go
git commit -m "feat: expose target reconciliation api"
~~~

---

### Task 7: Multi-country wizard and target dashboard

**Files:**
- Modify: web.go:112-150,220-285,577-690,693-808,882-925,1411-1412
- Create: web_targets_test.go

**Interfaces:**
- Consumes: GET/POST /api/targets, POST /api/targets/reconcile, /api/regions, /api/exits, and /api/jobs.
- Produces: multi-country form, status area, manual trigger, and managed-stop warning.

- [ ] **Step 1: Write failing embedded UI tests**

~~~go
func TestIndexHTMLContainsTargetControls(t *testing.T) {
	for _, want := range []string{
		"id=\"targetList\"",
		"id=\"reconcileNow\"",
		"/api/targets",
		"/api/targets/reconcile",
		"selectedTargets",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("indexHTML missing %q", want)
		}
	}
}

func TestIndexHTMLWarnsBeforeStoppingManagedExit(t *testing.T) {
	if !strings.Contains(indexHTML, "目标数量不变时，系统会自动补齐") {
		t.Fatal("managed stop warning missing")
	}
}
~~~

- [ ] **Step 2: Run UI tests and confirm failure**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'TestIndexHTML.*Target|TestIndexHTMLWarns' -count=1
~~~

Expected: tests fail because controls are absent.

- [ ] **Step 3: Replace single-region wizard state**

~~~js
let regions = [], regionsLoaded = false;
const selectedTargets = new Map();
~~~

Render every country as a selectable row with its own number input, default 1. Do not clamp requested count to candidate count. Require a real template and at least one country.

~~~js
const body = {
  template_id: Number($('#tpl').value),
  targets: Array.from(selectedTargets, ([country_code, target_count]) => ({
    country_code, target_count,
  })),
};
await api('/api/targets', {
  method: 'POST',
  headers: {'Content-Type': 'application/json'},
  body: JSON.stringify(body),
});
~~~

Remove the old unlimited/every-country special choices from this managed form. Disable the form for xray-cf-lite and show its read-only explanation.

- [ ] **Step 4: Add status and manual trigger**

Add targetList above exits and reconcileNow. Poll /api/targets and show template, country, healthy, starting, waiting, target, excess, last/next check, and error.

~~~js
await api('/api/targets/reconcile', {method:'POST'});
toast('已开始检测并自动填补');
poll();
~~~

Disable the button while a reconcile job is active. Before stopping an exit with target_id, confirm: “此出口属于自动保有目标；目标数量不变时，系统会自动补齐。仍要停止吗？”.

- [ ] **Step 5: Run UI and full tests**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'IndexHTML|API.*Target' -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./...
~~~

Expected: all tests pass.

- [ ] **Step 6: Perform Linux browser smoke test**

On a disposable Linux host with TUN/netns access: create a template; submit JP 2 and KR 1; observe country progress; verify healthy/waiting counts; click manual fill twice and see one job; stop a managed exit and see the warning.

Expected: all checks pass with no browser console error.

- [ ] **Step 7: Commit**

~~~bash
git add web.go web_targets_test.go
git commit -m "feat: add multi-country target controls"
~~~

---

### Task 8: Documentation and final verification

**Files:**
- Modify: README.md:69-151,181-206
- Modify: approved design spec only for an approved factual correction

**Interfaces:**
- Consumes: completed behavior and wording.
- Produces: operator-facing usage and limitations.

- [ ] **Step 1: Update README**

Document fixed multi-country targets, candidate estimates, 60-second/two-failure health, 15-minute/startup/manual fill, same-country waiting, retained excess, managed-stop refill, legacy unmanaged exits, and /var/lib/fanout/country_targets.json.

- [ ] **Step 2: Format and statically check**

~~~bash
gofmt -w *.go
git diff --check
docker run --rm -v "$PWD":/src -w /src golang:1.24 go vet ./...
~~~

Expected: checks exit successfully with no diagnostics.

- [ ] **Step 3: Run normal, race, and repeated tests**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./... -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 go test ./... -race -count=1
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test ./... -run 'CountryTarget|Reconcile|Managed|Health|Clone|API.*Target|IndexHTML' -count=20
~~~

Expected: every run passes.

- [ ] **Step 4: Build both Linux architectures**

~~~bash
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  sh -c 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/fanout-amd64 . && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/fanout-arm64 .'
~~~

Expected: both builds succeed.

- [ ] **Step 5: Review coverage against the design**

~~~bash
git diff --stat origin/main...HEAD
git log --oneline origin/main..HEAD
~~~

Confirm code/tests cover multi-country input, fixed targets, one-pass maximum creation, 60-second health, 15-minute fill, same-country replacement, retained deficits, single-flight behavior, and non-destructive reduction.

- [ ] **Step 6: Commit documentation**

~~~bash
git add README.md docs/superpowers/specs/2026-10-04-multi-country-exit-reconciliation-design.md
git commit -m "docs: explain automatic country target maintenance"
~~~
