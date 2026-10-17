package main

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime/k8s"
)

type startupNudgeDeliveryCheck struct{ cityPath string }

func newStartupNudgeDeliveryCheck(cityPath string) *startupNudgeDeliveryCheck {
	return &startupNudgeDeliveryCheck{cityPath: cityPath}
}

func (*startupNudgeDeliveryCheck) Name() string                     { return "startup-nudge-delivery" }
func (*startupNudgeDeliveryCheck) CanFix() bool                     { return false }
func (*startupNudgeDeliveryCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *startupNudgeDeliveryCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	diagnostics, err := k8s.StartupNudgeDiagnostics(c.cityPath)
	if err != nil {
		return warnCheck(c.Name(), "startup nudge intent state could not be read", "check controller-owned runtime metadata permissions, then rerun gc doctor", nil)
	}
	if len(diagnostics) == 0 {
		return okCheck(c.Name(), "no unresolved startup nudge deliveries")
	}
	details := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		line := fmt.Sprintf("session=%s intent=%s incarnation=%s provider=%s status=%s",
			diagnostic.SessionName, diagnostic.IntentID, diagnostic.Incarnation, diagnostic.ProviderFamily, diagnostic.Status)
		if diagnostic.Reason != "" {
			line += " reason=" + diagnostic.Reason
		}
		details = append(details, line)
	}
	sort.Strings(details)
	return errorCheck(c.Name(), fmt.Sprintf("%d startup nudge delivery issue(s) need operator attention", len(details)),
		"inspect each pane and resolve the exact intent and incarnation explicitly; uncertain prompts are never replayed automatically", details)
}
