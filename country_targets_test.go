package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCountryTargetStoreUpsertPersistsStableID(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadCountryTargetStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Upsert("native", 12, []TargetInput{
		{CountryCode: "jp", TargetCount: 3},
		{CountryCode: "KR", TargetCount: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != "native:12:JP" || got[0].CountryCode != "JP" {
		t.Fatalf("unexpected target: %+v", got[0])
	}
	if info, err := os.Stat(filepath.Join(dir, "country_targets.json")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0600 {
		t.Fatalf("unexpected file mode: %o", info.Mode().Perm())
	}

	reloaded, err := LoadCountryTargetStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if list := reloaded.List(); len(list) != 2 {
		t.Fatalf("targets did not persist: %+v", list)
	}
}

func TestCountryTargetStoreUpdatesExistingTargetAndResult(t *testing.T) {
	store, err := LoadCountryTargetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	created, err := store.Upsert("native", 12, []TargetInput{{CountryCode: "JP", TargetCount: 1}})
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Hour)
	updated, err := store.Upsert("native", 12, []TargetInput{{CountryCode: "jp", TargetCount: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if updated[0].ID != created[0].ID || !updated[0].CreatedAt.Equal(created[0].CreatedAt) {
		t.Fatalf("upsert changed target identity: created=%+v updated=%+v", created[0], updated[0])
	}
	if updated[0].TargetCount != 4 || !updated[0].UpdatedAt.Equal(now) {
		t.Fatalf("upsert did not update target: %+v", updated[0])
	}

	reconcileAt := now.Add(time.Minute)
	if err := store.UpdateResult(updated[0].ID, reconcileAt, errors.New("no candidates")); err != nil {
		t.Fatal(err)
	}
	got := store.List()[0]
	if !got.LastReconcileAt.Equal(reconcileAt) || got.LastError != "no candidates" {
		t.Fatalf("result not recorded: %+v", got)
	}
}

func TestCountryTargetStoreRejectsInvalidInput(t *testing.T) {
	store, err := LoadCountryTargetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		panelKind string
		template  int
		input     TargetInput
	}{
		{name: "empty panel kind", template: 12, input: TargetInput{CountryCode: "JP", TargetCount: 1}},
		{name: "invalid template", panelKind: "native", input: TargetInput{CountryCode: "JP", TargetCount: 1}},
		{name: "empty country", panelKind: "native", template: 12, input: TargetInput{CountryCode: "", TargetCount: 1}},
		{name: "short country", panelKind: "native", template: 12, input: TargetInput{CountryCode: "J", TargetCount: 1}},
		{name: "long country", panelKind: "native", template: 12, input: TargetInput{CountryCode: "JPN", TargetCount: 1}},
		{name: "non letter country", panelKind: "native", template: 12, input: TargetInput{CountryCode: "J1", TargetCount: 1}},
		{name: "zero count", panelKind: "native", template: 12, input: TargetInput{CountryCode: "JP", TargetCount: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.Upsert(tt.panelKind, tt.template, []TargetInput{tt.input}); err == nil {
				t.Fatalf("expected rejection for panel=%q template=%d input=%+v", tt.panelKind, tt.template, tt.input)
			}
		})
	}
}

func TestCountryTargetStoreUpsertRejectsDuplicateCountries(t *testing.T) {
	store, err := LoadCountryTargetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Upsert("native", 12, []TargetInput{
		{CountryCode: "jp", TargetCount: 1},
		{CountryCode: "JP", TargetCount: 2},
	})
	if err == nil {
		t.Fatal("expected duplicate country rejection")
	}
}
