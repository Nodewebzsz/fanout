package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	reconcileInterval = 15 * time.Minute
	candidateCooldown = 15 * time.Minute
)

// CountryTargetStatus is the desired target plus its current runtime counts.
type CountryTargetStatus struct {
	CountryTarget
	Healthy       int       `json:"healthy"`
	Starting      int       `json:"starting"`
	WaitingFill   int       `json:"waiting_fill"`
	Missing       int       `json:"missing"`
	Excess        int       `json:"excess"`
	LastCheckedAt time.Time `json:"last_checked_at,omitempty"`
	NextCheckAt   time.Time `json:"next_check_at,omitempty"`
}

// CountryReconciler serializes every startup, scheduled, health, and manual pass.
type CountryReconciler struct {
	mu       sync.Mutex
	mgr      *Manager
	store    *CountryTargetStore
	active   *Job
	pending  bool
	cooldown map[string]time.Time
	now      func() time.Time

	pass         func(*Job)
	refreshNodes func() (int, error)
	panelOpen    func() (Panel, error)
}

func NewCountryReconciler(mgr *Manager, store *CountryTargetStore) *CountryReconciler {
	r := &CountryReconciler{
		mgr:          mgr,
		store:        store,
		cooldown:     make(map[string]time.Time),
		now:          time.Now,
		refreshNodes: mgr.RefreshNodes,
		panelOpen:    openPanel,
	}
	r.pass = r.RunOnce
	return r
}

// Trigger starts a pass or coalesces the request into one extra pass when a job
// is already active. The returned bool reports whether a new job was started.
func (r *CountryReconciler) Trigger(reason string) (*Job, bool) {
	r.mu.Lock()
	if r.active != nil {
		r.pending = true
		job := r.active
		r.mu.Unlock()
		return job, false
	}

	targets := r.store.List()
	labels := make([]string, 0, len(targets))
	for _, target := range targets {
		labels = append(labels, fmt.Sprintf("%s · 模板 %d", target.CountryCode, target.TemplateID))
	}
	if len(labels) == 0 {
		labels = append(labels, "检查出口保有目标")
	}
	summary := "检测并自动填补国家出口"
	if strings.TrimSpace(reason) != "" {
		summary += "（" + reason + "）"
	}
	job := r.mgr.jobs.New(summary, labels)
	r.active = job
	r.pending = false
	r.mu.Unlock()

	go r.run(job)
	return job, true
}

func (r *CountryReconciler) run(job *Job) {
	for {
		r.pass(job)
		r.mu.Lock()
		if r.pending {
			r.pending = false
			r.mu.Unlock()
			continue
		}
		job.Finish()
		r.active = nil
		r.mu.Unlock()
		return
	}
}

// RequestRepair is the health monitor callback for managed exits.
func (r *CountryReconciler) RequestRepair(tunnel *Tunnel) {
	if tunnel == nil || tunnel.TargetID == "" {
		return
	}
	r.Trigger("健康检查")
}

// RunScheduler triggers full deficit reconciliation at the configured interval.
func (r *CountryReconciler) RunScheduler(interval time.Duration) {
	if interval <= 0 {
		interval = reconcileInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		r.Trigger("定时检查")
	}
}

// Statuses returns deterministic live counts for every persistent target.
func (r *CountryReconciler) Statuses() []CountryTargetStatus {
	tunnels := r.mgr.Tunnels()
	targets := r.store.List()
	statuses := make([]CountryTargetStatus, 0, len(targets))
	for _, target := range targets {
		statuses = append(statuses, calculateTargetStatus(target, tunnels))
	}
	return statuses
}

func calculateTargetStatus(target CountryTarget, tunnels []*Tunnel) CountryTargetStatus {
	status := CountryTargetStatus{
		CountryTarget: target,
		LastCheckedAt: target.LastReconcileAt,
	}
	if !target.LastReconcileAt.IsZero() {
		status.NextCheckAt = target.LastReconcileAt.Add(reconcileInterval)
	}
	occupied := 0
	for _, tunnel := range tunnels {
		if tunnel.TargetID != target.ID || tunnel.Status == "stopped" {
			continue
		}
		occupied++
		switch tunnel.Status {
		case "up":
			status.Healthy++
		case "starting":
			status.Starting++
		case "waiting_fill", "failed":
			status.WaitingFill++
		}
	}
	if occupied < target.TargetCount {
		status.Missing = target.TargetCount - occupied
	}
	if occupied > target.TargetCount {
		status.Excess = occupied - target.TargetCount
	}
	return status
}

// filterCandidates applies country, residential, ownership, pass-failure, and
// cooldown constraints, then sorts by speed, sessions, and hostname.
func (r *CountryReconciler) filterCandidates(country string, nodes []Node, failed map[string]bool) []Node {
	country = strings.ToUpper(strings.TrimSpace(country))
	used := make(map[string]bool)
	if r.mgr != nil {
		for _, tunnel := range r.mgr.Tunnels() {
			if tunnel.Node.HostName != "" {
				used[tunnel.Node.HostName] = true
			}
		}
	}
	now := r.now()
	r.mu.Lock()
	for host, until := range r.cooldown {
		if !until.After(now) {
			delete(r.cooldown, host)
		}
	}
	cooldown := make(map[string]time.Time, len(r.cooldown))
	for host, until := range r.cooldown {
		cooldown[host] = until
	}
	r.mu.Unlock()

	out := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		if !strings.EqualFold(node.CountryCode, country) || node.HostName == "" {
			continue
		}
		if residentialOnly() && !node.Residential {
			continue
		}
		if used[node.HostName] || failed[node.HostName] {
			continue
		}
		if until, cooling := cooldown[node.HostName]; cooling && until.After(now) {
			continue
		}
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SpeedMbps != out[j].SpeedMbps {
			return out[i].SpeedMbps > out[j].SpeedMbps
		}
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions < out[j].Sessions
		}
		return out[i].HostName < out[j].HostName
	})
	return out
}

func (r *CountryReconciler) coolDown(host string) {
	if host == "" {
		return
	}
	r.mu.Lock()
	r.cooldown[host] = r.now().Add(candidateCooldown)
	r.mu.Unlock()
}

// RunOnce performs one bounded reconciliation pass. It never sleeps waiting for
// candidates; unresolved deficits remain persistent for the next trigger.
func (r *CountryReconciler) RunOnce(job *Job) {
	targets := r.store.List()
	if len(targets) == 0 {
		job.Set(0, "ok", "尚未设置出口保有目标")
		return
	}
	if _, err := r.refreshNodes(); err != nil {
		for i, target := range targets {
			resultErr := fmt.Errorf("刷新 VPN Gate 节点失败: %w", err)
			_ = r.store.UpdateResult(target.ID, r.now(), resultErr)
			job.Set(i, "failed", firstLine(resultErr.Error()))
		}
		return
	}
	panel, err := r.panelOpen()
	if err != nil {
		for i, target := range targets {
			resultErr := fmt.Errorf("节点链接后端不可用: %w", err)
			_ = r.store.UpdateResult(target.ID, r.now(), resultErr)
			job.Set(i, "failed", firstLine(resultErr.Error()))
		}
		return
	}

	for i, target := range targets {
		job.Set(i, "running", "正在检查并填补")
		var resultErr error
		detail := ""
		switch {
		case panel.Kind() != target.PanelKind:
			resultErr = fmt.Errorf("目标属于 %s 后端，当前后端是 %s", target.PanelKind, panel.Kind())
		default:
			if _, err := panel.InboundDetail(target.TemplateID, ""); err != nil {
				resultErr = fmt.Errorf("模板入站 %d 不可用: %w", target.TemplateID, err)
			} else {
				detail, resultErr = r.reconcileTarget(target, panel)
			}
		}
		if err := r.store.UpdateResult(target.ID, r.now(), resultErr); err != nil && resultErr == nil {
			resultErr = err
		}
		if resultErr != nil {
			job.Set(i, "failed", firstLine(resultErr.Error()))
		} else {
			job.Set(i, "ok", detail)
		}
	}
}

func (r *CountryReconciler) reconcileTarget(target CountryTarget, panel Panel) (string, error) {
	failed := make(map[string]bool)
	nodes, _ := r.mgr.Nodes()
	repaired := 0
	createdCount := 0
	var lastErr error

	// Reserved slots are repaired first so their SOCKS5 and inbound identities stay stable.
	for _, tunnel := range r.mgr.Tunnels() {
		if tunnel.TargetID != target.ID || (tunnel.Status != "waiting_fill" && tunnel.Status != "failed") {
			continue
		}
		if tunnel.prevHostOf() != "" && tunnel.ExitIP != "" {
			if err := r.mgr.RepairManagedCandidate(tunnel, tunnel.Node); err != nil {
				return "", fmt.Errorf("出口 %d 完成入站改绑失败: %w", tunnel.Slot, err)
			}
			repaired++
			continue
		}

		filled := false
		for _, candidate := range r.filterCandidates(target.CountryCode, nodes, failed) {
			if err := r.mgr.RepairManagedCandidate(tunnel, candidate); err != nil {
				failed[candidate.HostName] = true
				lastErr = err
				if tunnel.ExitIP != "" { // transport worked; panel/rebind failed
					return "", fmt.Errorf("出口 %d 改绑失败: %w", tunnel.Slot, err)
				}
				r.coolDown(candidate.HostName)
				continue
			}
			repaired++
			filled = true
			break
		}
		if !filled {
			r.mgr.MarkWaitingFill(tunnel, lastErr)
		}
	}

	status := calculateTargetStatus(target, r.mgr.Tunnels())
	for status.Missing > 0 {
		candidates := r.filterCandidates(target.CountryCode, nodes, failed)
		if len(candidates) == 0 {
			break
		}
		createdThisSlot := false
		for _, candidate := range candidates {
			tunnel, err := r.mgr.StartManagedCandidate(candidate, target.ID)
			if err != nil {
				failed[candidate.HostName] = true
				lastErr = err
				r.coolDown(candidate.HostName)
				continue
			}
			cloned, err := panel.CloneToTunnels(target.TemplateID, []string{candidate.HostName}, r.mgr.Tunnels())
			invalidateInbounds()
			if err != nil {
				_ = r.mgr.Stop(tunnel.Slot)
				return "", fmt.Errorf("为 %s 创建入站失败: %w", candidate.HostName, err)
			}
			if len(cloned) != 1 || cloned[0].HostName != candidate.HostName {
				ids := make([]int, 0, len(cloned))
				for _, inbound := range cloned {
					ids = append(ids, inbound.ID)
				}
				if len(ids) > 0 {
					_ = panel.DeleteInbounds(ids, r.mgr.Tunnels())
				}
				_ = r.mgr.Stop(tunnel.Slot)
				return "", fmt.Errorf("为 %s 创建入站返回了异常结果", candidate.HostName)
			}
			createdCount++
			createdThisSlot = true
			break
		}
		if !createdThisSlot {
			break
		}
		status = calculateTargetStatus(target, r.mgr.Tunnels())
	}

	status = calculateTargetStatus(target, r.mgr.Tunnels())
	detail := fmt.Sprintf("健康 %d/%d", status.Healthy, target.TargetCount)
	if repaired > 0 || createdCount > 0 {
		detail += fmt.Sprintf("，修复 %d，新建 %d", repaired, createdCount)
	}
	if status.Excess > 0 {
		detail += fmt.Sprintf("，超出目标 %d", status.Excess)
	}
	if status.Healthy < target.TargetCount {
		missingHealthy := target.TargetCount - status.Healthy
		if lastErr != nil {
			return detail, fmt.Errorf("%s 健康出口仍缺 %d 个: %w", target.CountryCode, missingHealthy, lastErr)
		}
		return detail, fmt.Errorf("%s 暂无可用候选，健康出口仍缺 %d 个", target.CountryCode, missingHealthy)
	}
	return detail, nil
}
