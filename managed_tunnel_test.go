package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedCandidateTimeoutRemovesNewSlot(t *testing.T) {
	oldTimeout := connectAttemptTimeout
	connectAttemptTimeout = 20 * time.Millisecond
	defer func() { connectAttemptTimeout = oldTimeout }()

	m := NewManager(20, t.TempDir())
	m.tryNodeFn = func(*Tunnel) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	_, err := m.StartManagedCandidate(Node{HostName: "jp-stuck", CountryCode: "JP"}, "native:12:JP")
	if err == nil {
		t.Fatal("expected connection timeout")
	}
	if len(m.Tunnels()) != 0 {
		t.Fatalf("timed out managed candidate leaked slot: %+v", m.Tunnels())
	}
}

func TestManagedCandidateFailureRemovesNewSlot(t *testing.T) {
	m := NewManager(20, t.TempDir())
	var attempts int
	m.tryNodeFn = func(*Tunnel) error {
		attempts++
		return errors.New("dial failed")
	}
	_, err := m.StartManagedCandidate(
		Node{HostName: "jp1", CountryCode: "JP"}, "native:12:JP")
	if err == nil {
		t.Fatal("expected failure")
	}
	if attempts != 1 {
		t.Fatalf("managed candidate must be attempted once, got %d", attempts)
	}
	if len(m.Tunnels()) != 0 {
		t.Fatalf("failed new slot leaked: %+v", m.Tunnels())
	}
}

func TestRepairManagedCandidateKeepsIdentity(t *testing.T) {
	m := NewManager(20, t.TempDir())
	m.tryNodeFn = func(tunnel *Tunnel) error {
		tunnel.ExitIP = "203.0.113.9"
		return nil
	}
	var reboundFrom string
	m.rebindFn = func(old string, tunnel *Tunnel) error {
		reboundFrom = old
		return nil
	}
	cred := SocksCred{User: "u", Pass: "p"}
	tunnel := &Tunnel{
		Slot: 4, Port: 25000, Status: "waiting_fill",
		TargetID: "native:12:JP",
		Node:     Node{HostName: "jp-old", CountryCode: "JP"},
		Cred:     cred,
	}
	m.tunnels[4] = tunnel

	if err := m.RepairManagedCandidate(tunnel, Node{HostName: "jp-new", CountryCode: "JP"}); err != nil {
		t.Fatal(err)
	}
	if tunnel.Slot != 4 || tunnel.Port != 25000 || tunnel.Cred != cred ||
		tunnel.TargetID != "native:12:JP" || tunnel.Status != "up" {
		t.Fatalf("identity changed: %+v", tunnel)
	}
	if tunnel.Node.HostName != "jp-new" || reboundFrom != "jp-old" || tunnel.prevHostOf() != "" {
		t.Fatalf("repair was not settled: tunnel=%+v reboundFrom=%q prevHost=%q", tunnel, reboundFrom, tunnel.prevHostOf())
	}
}

func TestRepairManagedCandidateRejectsAnotherCountry(t *testing.T) {
	m := NewManager(20, t.TempDir())
	called := false
	m.tryNodeFn = func(*Tunnel) error {
		called = true
		return nil
	}
	tunnel := &Tunnel{
		Slot: 1, Port: 25000, Status: "waiting_fill",
		TargetID: "native:12:JP",
		Node:     Node{HostName: "jp-old", CountryCode: "JP"},
	}
	m.tunnels[1] = tunnel
	if err := m.RepairManagedCandidate(tunnel, Node{HostName: "us1", CountryCode: "US"}); err == nil {
		t.Fatal("expected cross-country repair rejection")
	}
	if called || tunnel.Node.HostName != "jp-old" {
		t.Fatalf("rejected candidate changed tunnel: called=%v tunnel=%+v", called, tunnel)
	}
}

func TestMarkWaitingFillPersistsManagedSlot(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(20, dir)
	tunnel := &Tunnel{
		Slot: 1, Port: 25000, Status: "up", ExitIP: "203.0.113.7",
		TargetID: "native:12:JP",
		Node:     Node{HostName: "jp-old", CountryCode: "JP"},
		Cred:     SocksCred{User: "u", Pass: "p"},
	}
	m.tunnels[1] = tunnel
	m.MarkWaitingFill(tunnel, errors.New("no candidates\nretry later"))
	if tunnel.Status != "waiting_fill" || tunnel.ExitIP != "" || tunnel.Err != "no candidates" {
		t.Fatalf("unexpected waiting state: %+v", tunnel)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("waiting state was not persisted: %v", err)
	}
}

func TestRestoreManagedTunnelWaitsForReconciler(t *testing.T) {
	dir := t.TempDir()
	blob := `{"tunnels":[{"slot":1,"port":25000,"hostname":"jp-old","country_code":"JP","config":"x","socks_user":"u","socks_pass":"p","target_id":"native:12:JP"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(blob), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(20, dir)
	if _, err := m.restoreState(); err != nil {
		t.Fatal(err)
	}
	tunnel := m.Tunnels()[0]
	if tunnel.Status != "waiting_fill" {
		t.Fatalf("managed tunnel should wait for one-pass reconciliation, got %q", tunnel.Status)
	}
}
