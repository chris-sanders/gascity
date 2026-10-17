package k8s

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing SleepCapability
// through to the underlying *Provider. The early cut-over for the k8s provider.
//
// ExecProvider is not passed through: k8s's Exec (execInPod) is the connection
// the carrier drives over internally; no production caller type-asserts it.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                          = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider           = (*seamBackedProvider)(nil)
	_ runtime.RelaunchProvider                  = (*seamBackedProvider)(nil)
	_ runtime.ListingAttestation                = (*seamBackedProvider)(nil)
	_ runtime.K8sStoreAttachmentProvider        = (*seamBackedProvider)(nil)
	_ runtime.StartupNudgeRecoveryProvider      = (*seamBackedProvider)(nil)
	_ runtime.StartupNudgeSessionProvider       = (*seamBackedProvider)(nil)
	_ runtime.StartupNudgeLeasedSessionProvider = (*seamBackedProvider)(nil)
	_ runtime.StartupNudgeLeaseTargetProvider   = (*seamBackedProvider)(nil)
	_ runtime.StartupNudgeEventAcknowledger     = (*seamBackedProvider)(nil)
)

// NewSeamBacked constructs a k8s provider served through the seams.
func NewSeamBacked() (runtime.Provider, error) {
	return NewSeamBackedWithConfig(nil, nil, PodSecurityConfig{})
}

// NewSeamBackedWithSecretProjection constructs a seam-backed K8s provider with
// the city's configured Secret projections.
func NewSeamBackedWithSecretProjection(secretEnv []runtime.K8sSecretEnv, secretMounts []runtime.K8sSecretMount) (runtime.Provider, error) {
	return NewSeamBackedWithConfig(secretEnv, secretMounts, PodSecurityConfig{})
}

// NewSeamBackedWithSecurityConfig constructs a K8s provider with optional
// pod-level UID, GID, and fsGroup values from the city session config.
func NewSeamBackedWithSecurityConfig(config PodSecurityConfig) (runtime.Provider, error) {
	return NewSeamBackedWithConfig(nil, nil, config)
}

// NewSeamBackedWithConfig constructs a K8s provider with Secret projections
// and optional pod-level identity configuration.
func NewSeamBackedWithConfig(secretEnv []runtime.K8sSecretEnv, secretMounts []runtime.K8sSecretMount, config PodSecurityConfig) (runtime.Provider, error) {
	return NewSeamBackedWithConfigAndCityPath(secretEnv, secretMounts, config, processCityPath())
}

// NewSeamBackedWithConfigAndCityPath binds controller-owned runtime metadata
// to the selected city even when --city was supplied without process env.
func NewSeamBackedWithConfigAndCityPath(secretEnv []runtime.K8sSecretEnv, secretMounts []runtime.K8sSecretMount, config PodSecurityConfig, cityPath string) (runtime.Provider, error) {
	raw, err := NewProviderWithConfigAndCityPath(secretEnv, secretMounts, config, cityPath)
	if err != nil {
		return nil, err
	}
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}, nil
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// Relaunch passes through to the underlying provider's warm-pod relaunch
// (respawn-pane via execInPod; B2, RelaunchProvider).
func (s *seamBackedProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return s.raw.Relaunch(ctx, name, cfg)
}

// ListRunningComplete passes the underlying provider's listing attestation
// through; the seams route ListRunning to the same raw listing.
func (s *seamBackedProvider) ListRunningComplete() bool {
	return s.raw.ListRunningComplete()
}

// RequiresK8sStoreAttachment preserves the leaf capability through the seam
// wrapper so composite routing can select it by the actual backend.
func (s *seamBackedProvider) RequiresK8sStoreAttachment() bool {
	return s.raw.RequiresK8sStoreAttachment()
}

// RecoverStartupNudges keeps controller-owned startup delivery recovery
// reachable through the runtime seam wrapper.
func (s *seamBackedProvider) RecoverStartupNudges(ctx context.Context) []runtime.StartupNudgeStatus {
	return s.raw.RecoverStartupNudges(ctx)
}

func (s *seamBackedProvider) StartupNudgeSessionNames(ctx context.Context) []string {
	return s.raw.StartupNudgeSessionNames(ctx)
}

func (s *seamBackedProvider) RecoverStartupNudgesForSession(ctx context.Context, name string) []runtime.StartupNudgeStatus {
	return s.raw.RecoverStartupNudgesForSession(ctx, name)
}

func (s *seamBackedProvider) RecoverStartupNudgesForLeasedSession(ctx context.Context, name, sessionID string) []runtime.StartupNudgeStatus {
	return s.raw.RecoverStartupNudgesForLeasedSession(ctx, name, sessionID)
}

func (s *seamBackedProvider) StartupNudgeLeaseTargets(ctx context.Context) []runtime.StartupNudgeLeaseTarget {
	return s.raw.StartupNudgeLeaseTargets(ctx)
}

func (s *seamBackedProvider) AcknowledgeStartupNudgeEvent(ctx context.Context, name, intentID string, revision uint64) error {
	return s.raw.AcknowledgeStartupNudgeEvent(ctx, name, intentID, revision)
}
