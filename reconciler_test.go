package main

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type reconcilePanel struct {
	fakePanel
	templateID int
	cloneErr   error
	cloned     []string
	deleted    []int
	nextID     int
}

func (p *reconcilePanel) Kind() string { return "native" }

func (p *reconcilePanel) InboundDetail(id int, publicHost string) (*InboundDetail, error) {
	if id != p.templateID {
		return nil, fmt.Errorf("入站 %d 不存在", id)
	}
	return &InboundDetail{Inbound: Inbound{ID: id, Enable: true, Tag: "in-template-tcp"}}, nil
}

func (p *reconcilePanel) CloneToTunnels(templateID int, hosts []string, tunnels []*Tunnel) ([]ClonedInbound, error) {
	if p.cloneErr != nil {
		return nil, p.cloneErr
	}
	if len(hosts) != 1 {
		return nil, fmt.Errorf("expected one host, got %d", len(hosts))
	}
	p.nextID++
	p.cloned = append(p.cloned, hosts[0])
	return []ClonedInbound{{ID: p.nextID, Port: 30000 + p.nextID, HostName: hosts[0]}}, nil
}

func (p *reconcilePanel) DeleteInbounds(ids []int, tunnels []*Tunnel) error {
	p.deleted = append(p.deleted, ids...)
	return nil
}

func newReconcilerFixture(t *testing.T, count int, nodes []Node) (*CountryReconciler, *Manager, *reconcilePanel) {
	t.Helper()
	withResidentialOnly(t, boolPtr(false))
	manager := NewManager(20, t.TempDir())
	manager.nodes = append([]Node(nil), nodes...)
	manager.tryNodeFn = func(tunnel *Tunnel) error {
		tunnel.ExitIP = "203.0.113.7"
		return nil
	}
	panel := &reconcilePanel{templateID: 12, nextID: 100}
	usePanel(t, panel)
	store, err := LoadCountryTargetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert("native", 12, []TargetInput{{CountryCode: "JP", TargetCount: count}}); err != nil {
		t.Fatal(err)
	}
	reconciler := NewCountryReconciler(manager, store)
	reconciler.refreshNodes = func() (int, error) { return len(nodes), nil }
	return reconciler, manager, panel
}

func runReconcileOnce(r *CountryReconciler) {
	job := r.mgr.jobs.New("test reconcile", []string{"JP"})
	r.RunOnce(job)
	job.Finish()
}

func TestTargetStatusCountsWaitingSlotWithoutDuplicating(t *testing.T) {
	target := CountryTarget{ID: "native:12:JP", TargetCount: 3, CountryCode: "JP"}
	tunnels := []*Tunnel{
		{TargetID: target.ID, Status: "up"},
		{TargetID: target.ID, Status: "up"},
		{TargetID: target.ID, Status: "waiting_fill"},
	}
	status := calculateTargetStatus(target, tunnels)
	if status.Healthy != 2 || status.WaitingFill != 1 || status.Missing != 0 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestTargetStatusReportsExcessWithoutDeleting(t *testing.T) {
	target := CountryTarget{ID: "native:12:JP", TargetCount: 1, CountryCode: "JP"}
	status := calculateTargetStatus(target, []*Tunnel{
		{TargetID: target.ID, Status: "up"},
		{TargetID: target.ID, Status: "up"},
	})
	if status.Healthy != 2 || status.Excess != 1 || status.Missing != 0 {
		t.Fatalf("unexpected excess status: %+v", status)
	}
}

func TestCandidatesStayInCountryAndRespectCooldown(t *testing.T) {
	withResidentialOnly(t, boolPtr(false))
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := &CountryReconciler{
		now:      func() time.Time { return now },
		cooldown: map[string]time.Time{"jp1": now.Add(time.Minute)},
		mgr:      NewManager(20, t.TempDir()),
	}
	got := r.filterCandidates("JP", []Node{
		{HostName: "jp1", CountryCode: "JP", SpeedMbps: 500},
		{HostName: "us1", CountryCode: "US", SpeedMbps: 900},
		{HostName: "jp3", CountryCode: "jp", SpeedMbps: 300, Sessions: 20},
		{HostName: "jp2", CountryCode: "JP", SpeedMbps: 300, Sessions: 10},
	}, nil)
	if len(got) != 2 || got[0].HostName != "jp2" || got[1].HostName != "jp3" {
		t.Fatalf("unexpected candidates: %+v", got)
	}
}

func TestReconcileAdoptsExistingUnboundInboundBeforeCloning(t *testing.T) {
	r, manager, panel := newReconcilerFixture(t, 1, []Node{{HostName: "jp1", CountryCode: "JP", Residential: true}})
	panel.fakePanel.inbounds = []Inbound{{ID: 99, Tag: "in-56388-tcp", Remark: "vless-56388"}}
	runReconcileOnce(r)
	if len(panel.cloned) != 0 {
		t.Fatalf("adopting an existing inbound must not clone: %+v", panel.cloned)
	}
	if len(manager.Tunnels()) != 1 || manager.Tunnels()[0].Status != "up" {
		t.Fatalf("expected one healthy managed tunnel: %+v", manager.Tunnels())
	}
}

func TestReconcileAdoptsUnboundTemplateWhenTargetAlreadyHealthy(t *testing.T) {
	r, manager, panel := newReconcilerFixture(t, 1, nil)
	manager.tunnels[1] = &Tunnel{
		Slot: 1, Status: "up", TargetID: "native:12:JP",
		Node: Node{HostName: "jp1", CountryCode: "JP"},
	}
	runReconcileOnce(r)
	if len(panel.cloned) != 0 {
		t.Fatalf("a healthy target with an unbound template must not clone: %+v", panel.cloned)
	}
}

func TestTriggerCoalescesConcurrentRequestsIntoOneExtraPass(t *testing.T) {
	manager := NewManager(20, t.TempDir())
	store, err := LoadCountryTargetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := NewCountryReconciler(manager, store)
	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	var running, maxRunning, passes atomic.Int32
	r.pass = func(*Job) {
		current := running.Add(1)
		for {
			max := maxRunning.Load()
			if current <= max || maxRunning.CompareAndSwap(max, current) {
				break
			}
		}
		passes.Add(1)
		entered <- struct{}{}
		<-release
		running.Add(-1)
	}

	job, started := r.Trigger("manual")
	if !started {
		t.Fatal("first trigger should start a job")
	}
	awaitSignal(t, entered)
	second, started := r.Trigger("timer")
	if started || second != job {
		t.Fatal("second trigger should reuse the active job")
	}
	third, started := r.Trigger("health")
	if started || third != job {
		t.Fatal("third trigger should reuse the active job")
	}
	release <- struct{}{}
	awaitSignal(t, entered)
	release <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for job.View().Status == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if passes.Load() != 2 || maxRunning.Load() != 1 || job.View().Status != "done" {
		t.Fatalf("passes=%d max=%d job=%+v", passes.Load(), maxRunning.Load(), job.View())
	}
}

func TestReconcileCreatesPartialCountryTargetWithoutSubstitution(t *testing.T) {
	nodes := []Node{
		{HostName: "jp1", CountryCode: "JP", SpeedMbps: 500, Residential: true},
		{HostName: "jp2", CountryCode: "JP", SpeedMbps: 400, Residential: true},
		{HostName: "us1", CountryCode: "US", SpeedMbps: 900, Residential: true},
	}
	r, manager, panel := newReconcilerFixture(t, 2, nodes)
	manager.tryNodeFn = func(tunnel *Tunnel) error {
		if tunnel.Node.HostName == "jp2" {
			return errors.New("jp2 down")
		}
		tunnel.ExitIP = "203.0.113.7"
		return nil
	}
	runReconcileOnce(r)
	tunnels := manager.Tunnels()
	if len(tunnels) != 1 || tunnels[0].Node.HostName != "jp1" || tunnels[0].TargetID != "native:12:JP" {
		t.Fatalf("unexpected managed tunnels: %+v", tunnels)
	}
	if len(panel.cloned) != 1 || panel.cloned[0] != "jp1" {
		t.Fatalf("unexpected cloned inbounds: %+v", panel.cloned)
	}
	status := r.Statuses()[0]
	if status.Healthy != 1 || status.Missing != 1 || status.LastError == "" {
		t.Fatalf("partial target status missing deficit: %+v", status)
	}
}

func TestReconcileRepairsWaitingSlotBeforeCreating(t *testing.T) {
	r, manager, panel := newReconcilerFixture(t, 1, []Node{{HostName: "jp-new", CountryCode: "JP", SpeedMbps: 500}})
	waiting := &Tunnel{
		Slot: 4, Port: 25000, Status: "waiting_fill", TargetID: "native:12:JP",
		Node: Node{HostName: "jp-old", CountryCode: "JP"}, Cred: SocksCred{User: "u", Pass: "p"},
	}
	manager.tunnels[4] = waiting
	runReconcileOnce(r)
	if len(manager.Tunnels()) != 1 || manager.Tunnels()[0].Slot != 4 || manager.Tunnels()[0].Node.HostName != "jp-new" {
		t.Fatalf("waiting slot was not repaired in place: %+v", manager.Tunnels())
	}
	if len(panel.cloned) != 0 {
		t.Fatalf("repair should preserve inbound instead of cloning: %+v", panel.cloned)
	}
}

func TestReconcileCloneFailureRollsBackTunnel(t *testing.T) {
	r, manager, panel := newReconcilerFixture(t, 1, []Node{{HostName: "jp1", CountryCode: "JP", SpeedMbps: 500}})
	panel.cloneErr = errors.New("bind failed")
	runReconcileOnce(r)
	if len(manager.Tunnels()) != 0 {
		t.Fatalf("clone failure leaked tunnel: %+v", manager.Tunnels())
	}
	if status := r.Statuses()[0]; status.Missing != 1 || status.LastError == "" {
		t.Fatalf("clone failure not reported: %+v", status)
	}
}

func TestReconcileCooldownAllowsCandidateAfterFifteenMinutes(t *testing.T) {
	r, manager, _ := newReconcilerFixture(t, 1, []Node{{HostName: "jp1", CountryCode: "JP", SpeedMbps: 500}})
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	var attempts int
	manager.tryNodeFn = func(*Tunnel) error {
		attempts++
		return errors.New("down")
	}
	runReconcileOnce(r)
	runReconcileOnce(r)
	if attempts != 1 {
		t.Fatalf("candidate retried during cooldown: %d", attempts)
	}
	now = now.Add(15 * time.Minute)
	runReconcileOnce(r)
	if attempts != 2 {
		t.Fatalf("candidate did not return after cooldown: %d", attempts)
	}
}

func TestReconcileDoesNotDeleteHealthyExcess(t *testing.T) {
	r, manager, panel := newReconcilerFixture(t, 1, nil)
	for slot := 1; slot <= 2; slot++ {
		manager.tunnels[slot] = &Tunnel{
			Slot: slot, Status: "up", TargetID: "native:12:JP",
			Node: Node{HostName: fmt.Sprintf("jp%d", slot), CountryCode: "JP"},
		}
	}
	runReconcileOnce(r)
	if len(manager.Tunnels()) != 2 || len(panel.cloned) != 0 {
		t.Fatalf("excess exits must be retained: tunnels=%+v cloned=%+v", manager.Tunnels(), panel.cloned)
	}
	if status := r.Statuses()[0]; status.Excess != 1 {
		t.Fatalf("excess not reported: %+v", status)
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconciler")
	}
}
