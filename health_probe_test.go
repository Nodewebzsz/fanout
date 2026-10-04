package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestProbeExitThroughSOCKSFallsBack(t *testing.T) {
	var calls int
	run := func(name string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("primary down")
		}
		return []byte("203.0.113.7\n"), nil
	}
	tunnel := &Tunnel{Port: 23456, Cred: SocksCred{User: "u", Pass: "p"}}
	ip, err := probeExitThroughSOCKS(tunnel, run)
	if err != nil || ip != "203.0.113.7" || calls != 2 {
		t.Fatalf("fallback failed: ip=%q calls=%d err=%v", ip, calls, err)
	}
}

func TestProbeExitThroughSOCKSUsesAuthenticatedProxy(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte("203.0.113.7"), nil
	}
	tunnel := &Tunnel{Port: 23456, Cred: SocksCred{User: "alice", Pass: "secret"}}
	if _, err := probeExitThroughSOCKS(tunnel, run); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-fsS", "--max-time", "8",
		"--socks5-hostname", "127.0.0.1:23456",
		"--proxy-user", "alice:secret",
		probeURLs[0],
	}
	if gotName != "curl" || !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("unexpected command: %s %#v", gotName, gotArgs)
	}
}

func TestProbeExitThroughSOCKSRejectsNonIPv4Responses(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		return []byte("2001:db8::1"), nil
	}
	if _, err := probeExitThroughSOCKS(&Tunnel{Port: 23456}, run); err == nil {
		t.Fatal("expected non-IPv4 response rejection")
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

func TestTunnelHealthyAcceptsMatchingExitIP(t *testing.T) {
	m := NewManager(20, t.TempDir())
	m.probeExit = func(*Tunnel) (string, error) {
		return "203.0.113.7", nil
	}
	if !m.tunnelHealthy(&Tunnel{ExitIP: "203.0.113.7"}) {
		t.Fatal("matching exit IP must be healthy")
	}
}
