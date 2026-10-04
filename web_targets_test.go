package main

import (
	"strings"
	"testing"
)

func TestIndexHTMLContainsTargetControls(t *testing.T) {
	for _, want := range []string{
		`id="targetList"`,
		`id="reconcileNow"`,
		`/api/targets`,
		`/api/targets/reconcile`,
		`selectedTargets`,
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

func TestIndexHTMLUsesIndependentCountryCounts(t *testing.T) {
	for _, want := range []string{
		`data-target-count`,
		`country_code`,
		`target_count`,
		`当前空闲数仅供参考`,
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("multi-country target form missing %q", want)
		}
	}
}

func TestIndexHTMLExplainsReadOnlyBackendTargets(t *testing.T) {
	if !strings.Contains(indexHTML, "xray-cf-lite 模式只支持绑定已有节点，不能创建自动保有目标") {
		t.Fatal("read-only backend explanation missing")
	}
}

func TestIndexHTMLUsesStableCountryCardLayout(t *testing.T) {
	for _, want := range []string{
		"grid-template-columns:repeat(auto-fit,minmax(154px,1fr))",
		".countrypick{position:relative;display:block;min-height:74px}",
		".countrypick .countrycount{position:absolute",
		"-webkit-line-clamp:2",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("country card layout missing %q", want)
		}
	}
}

func TestIndexHTMLResetsWizardOnClose(t *testing.T) {
	for _, want := range []string{
		"function resetWizard(){",
		"selectedTargets.clear()",
		"if(id === 'wizard') resetWizard()",
		"forEach(m => closeModal(m.id))",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("wizard reset behavior missing %q", want)
		}
	}
}
