package main

import (
	"fmt"
	"strings"
)

// StartManagedCandidate performs exactly one connection attempt for a new
// managed slot. A failed candidate never leaves an allocated slot behind.
func (m *Manager) StartManagedCandidate(node Node, targetID string) (*Tunnel, error) {
	if strings.TrimSpace(targetID) == "" {
		return nil, fmt.Errorf("托管出口缺少目标 ID")
	}
	tunnel, err := m.allocateTunnel(node, targetID)
	if err != nil {
		return nil, err
	}
	if err := m.tryNodeFn(tunnel); err != nil {
		tunnel.stop()
		m.mu.Lock()
		if m.tunnels[tunnel.Slot] == tunnel {
			delete(m.tunnels, tunnel.Slot)
		}
		m.mu.Unlock()
		_ = m.saveState()
		return nil, err
	}
	tunnel.Status = "up"
	tunnel.Err = ""
	if err := m.saveState(); err != nil {
		return tunnel, err
	}
	m.notifyPanel()
	return tunnel, nil
}

// RepairManagedCandidate replaces only the VPN transport while preserving the
// slot, SOCKS5 endpoint, credentials, target ownership, and panel inbounds.
func (m *Manager) RepairManagedCandidate(tunnel *Tunnel, candidate Node) error {
	if tunnel == nil {
		return fmt.Errorf("待修复出口不能为空")
	}
	if tunnel.TargetID == "" {
		return fmt.Errorf("出口 %d 不是托管出口", tunnel.Slot)
	}
	expectedCountry := targetCountryCode(tunnel.TargetID)
	if expectedCountry == "" {
		expectedCountry = strings.ToUpper(strings.TrimSpace(tunnel.Node.CountryCode))
	}
	candidateCountry := strings.ToUpper(strings.TrimSpace(candidate.CountryCode))
	if expectedCountry == "" || candidateCountry != expectedCountry {
		return fmt.Errorf("目标国家是 %s，不能使用 %s 节点", expectedCountry, candidateCountry)
	}
	if !m.tunnelActive(tunnel) {
		return fmt.Errorf("出口 %d 已不在管理器中", tunnel.Slot)
	}

	// A previous candidate may already be healthy while only the panel rebind
	// failed. Retry that stable operation before tearing down transport again.
	if pendingHost := tunnel.prevHostOf(); pendingHost != "" && tunnel.ExitIP != "" {
		if err := m.rebindFn(pendingHost, tunnel); err != nil {
			tunnel.Status = "waiting_fill"
			tunnel.Err = firstLine(err.Error())
			_ = m.saveState()
			return err
		}
		tunnel.setPrevHost("")
		tunnel.Status = "up"
		tunnel.Err = ""
		return m.saveState()
	}

	oldNode := tunnel.Node
	oldHost := oldNode.HostName
	stablePort := tunnel.Port
	stableCred := tunnel.Cred
	stableTargetID := tunnel.TargetID
	oldListener := tunnel.listener

	tunnel.Status = "starting"
	tunnel.Err = "正在填补同国家可用节点"
	tunnel.ExitIP = ""
	tunnel.setPrevHost(oldHost)
	if err := m.saveState(); err != nil {
		return err
	}
	m.stopManagedTransport(tunnel)
	tunnel.Node = candidate

	err := m.tryNodeFn(tunnel)
	if tunnel.Port != stablePort {
		if oldListener == nil && tunnel.listener != nil {
			_ = tunnel.listener.Close()
			tunnel.listener = nil
		}
		tunnel.Port = stablePort
		if err == nil {
			err = fmt.Errorf("无法保留 SOCKS5 端口 %d", stablePort)
		}
	}
	if err != nil {
		m.stopManagedTransport(tunnel)
		tunnel.Node = oldNode
		tunnel.Cred = stableCred
		tunnel.TargetID = stableTargetID
		tunnel.setPrevHost("")
		m.MarkWaitingFill(tunnel, err)
		return err
	}

	// Explicitly restore stable identity in case a lower-level operation ever
	// mutates one of these fields.
	tunnel.Port = stablePort
	tunnel.Cred = stableCred
	tunnel.TargetID = stableTargetID
	tunnel.Status = "up"
	tunnel.Err = ""
	if err := m.rebindFn(oldHost, tunnel); err != nil {
		tunnel.Status = "waiting_fill"
		tunnel.Err = firstLine(err.Error())
		_ = m.saveState() // keep prevHost so a later pass can finish the rebind
		return err
	}
	tunnel.setPrevHost("")
	return m.saveState()
}

// MarkWaitingFill records a managed slot that currently has no usable backend.
func (m *Manager) MarkWaitingFill(tunnel *Tunnel, cause error) {
	if tunnel == nil {
		return
	}
	tunnel.Status = "waiting_fill"
	tunnel.ExitIP = ""
	if cause == nil {
		tunnel.Err = ""
	} else {
		tunnel.Err = firstLine(cause.Error())
	}
	_ = m.saveState()
}

func (m *Manager) stopManagedTransport(tunnel *Tunnel) {
	tunnel.mu.Lock()
	if tunnel.ovpn != nil && tunnel.ovpn.Process != nil {
		_ = tunnel.ovpn.Process.Kill()
		tunnel.ovpn = nil
	}
	tunnel.mu.Unlock()
	tunnel.teardownNetns()
}

func targetCountryCode(id string) string {
	parts := strings.Split(id, ":")
	if len(parts) != 3 {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(parts[2]))
}
