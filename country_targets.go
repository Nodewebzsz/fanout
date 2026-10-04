package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const countryTargetsFile = "country_targets.json"

// TargetInput describes the desired number of managed exits for one country.
type TargetInput struct {
	CountryCode string `json:"country_code"`
	TargetCount int    `json:"target_count"`
}

// CountryTarget is a persistent desired-state record for one panel template and country.
type CountryTarget struct {
	ID              string    `json:"target_id"`
	PanelKind       string    `json:"panel_kind"`
	TemplateID      int       `json:"template_id"`
	CountryCode     string    `json:"country_code"`
	TargetCount     int       `json:"target_count"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	LastReconcileAt time.Time `json:"last_reconcile_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
}

type countryTargetState struct {
	Targets []CountryTarget `json:"targets"`
}

// CountryTargetStore persists country targets independently from tunnel runtime state.
type CountryTargetStore struct {
	mu      sync.RWMutex
	path    string
	targets map[string]CountryTarget
	now     func() time.Time
}

func targetID(panelKind string, templateID int, countryCode string) string {
	return fmt.Sprintf("%s:%d:%s",
		strings.ToLower(strings.TrimSpace(panelKind)),
		templateID,
		strings.ToUpper(strings.TrimSpace(countryCode)),
	)
}

// LoadCountryTargetStore loads the target store from dir. A missing file is an empty store.
func LoadCountryTargetStore(dir string) (*CountryTargetStore, error) {
	store := &CountryTargetStore{
		path:    filepath.Join(dir, countryTargetsFile),
		targets: make(map[string]CountryTarget),
		now:     time.Now,
	}
	blob, err := os.ReadFile(store.path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取国家出口目标失败: %w", err)
	}
	var state countryTargetState
	if err := json.Unmarshal(blob, &state); err != nil {
		return nil, fmt.Errorf("解析国家出口目标失败: %w", err)
	}
	for _, target := range state.Targets {
		if target.ID == "" {
			target.ID = targetID(target.PanelKind, target.TemplateID, target.CountryCode)
		}
		if _, exists := store.targets[target.ID]; exists {
			return nil, fmt.Errorf("国家出口目标重复: %s", target.ID)
		}
		store.targets[target.ID] = target
	}
	return store, nil
}

// Upsert validates and atomically persists all supplied country targets.
func (s *CountryTargetStore) Upsert(panelKind string, templateID int, inputs []TargetInput) ([]CountryTarget, error) {
	panelKind = strings.ToLower(strings.TrimSpace(panelKind))
	if panelKind == "" || strings.Contains(panelKind, ":") {
		return nil, fmt.Errorf("节点后端不能为空或包含冒号")
	}
	if templateID <= 0 {
		return nil, fmt.Errorf("模板 ID 必须大于 0")
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("至少需要一个国家目标")
	}

	normalized := make([]TargetInput, len(inputs))
	seen := make(map[string]bool, len(inputs))
	for i, input := range inputs {
		country := strings.ToUpper(strings.TrimSpace(input.CountryCode))
		if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
			return nil, fmt.Errorf("国家代码 %q 必须是两个英文字母", input.CountryCode)
		}
		if input.TargetCount <= 0 {
			return nil, fmt.Errorf("%s 的目标数量必须大于 0", country)
		}
		if seen[country] {
			return nil, fmt.Errorf("国家目标重复: %s", country)
		}
		seen[country] = true
		normalized[i] = TargetInput{CountryCode: country, TargetCount: input.TargetCount}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	updated := make([]CountryTarget, len(normalized))
	for i, input := range normalized {
		id := targetID(panelKind, templateID, input.CountryCode)
		target, exists := s.targets[id]
		if !exists {
			target = CountryTarget{
				ID:          id,
				PanelKind:   panelKind,
				TemplateID:  templateID,
				CountryCode: input.CountryCode,
				CreatedAt:   now,
			}
		}
		target.TargetCount = input.TargetCount
		target.UpdatedAt = now
		s.targets[id] = target
		updated[i] = target
	}
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return updated, nil
}

// List returns a stable snapshot ordered by target ID.
func (s *CountryTargetStore) List() []CountryTarget {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]CountryTarget, 0, len(s.targets))
	for _, target := range s.targets {
		list = append(list, target)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// UpdateResult records the outcome of the most recent reconciliation attempt.
func (s *CountryTargetStore) UpdateResult(id string, at time.Time, reconcileErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[id]
	if !ok {
		return fmt.Errorf("国家出口目标不存在: %s", id)
	}
	target.LastReconcileAt = at
	if reconcileErr == nil {
		target.LastError = ""
	} else {
		target.LastError = reconcileErr.Error()
	}
	s.targets[id] = target
	return s.saveLocked()
}

func (s *CountryTargetStore) saveLocked() error {
	state := countryTargetState{Targets: make([]CountryTarget, 0, len(s.targets))}
	for _, target := range s.targets {
		state.Targets = append(state.Targets, target)
	}
	sort.Slice(state.Targets, func(i, j int) bool { return state.Targets[i].ID < state.Targets[j].ID })
	blob, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("编码国家出口目标失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("创建国家出口目标目录失败: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return fmt.Errorf("写入国家出口目标失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("保存国家出口目标失败: %w", err)
	}
	return nil
}
