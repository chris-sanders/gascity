package k8s

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/gastownhall/gascity/internal/processenv"
)

// secretEnvRef is an immutable provider-level reference to a Secret key. The
// provider carries only this metadata; kubelet resolves the value when it
// creates a worker Pod.
type secretEnvRef struct {
	Name     string `json:"name"`
	Secret   string `json:"secret"`
	Key      string `json:"key"`
	Optional bool   `json:"optional"`
}

// parseSecretEnvProjection validates the operator-owned GC_K8S_SECRET_ENV
// contract once, when the native provider is constructed. Secret values are
// never read by this path.
func parseSecretEnvProjection(raw string) ([]secretEnvRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.HasPrefix(raw, "[") {
		return nil, fmt.Errorf("parsing GC_K8S_SECRET_ENV: expected a JSON array")
	}

	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var refs []secretEnvRef
	if err := decoder.Decode(&refs); err != nil {
		return nil, fmt.Errorf("parsing GC_K8S_SECRET_ENV: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parsing GC_K8S_SECRET_ENV: expected one JSON value")
		}
		return nil, fmt.Errorf("parsing GC_K8S_SECRET_ENV: %w", err)
	}

	seen := make(map[string]struct{}, len(refs))
	for i, ref := range refs {
		if ref.Name == "" {
			return nil, fmt.Errorf("validating GC_K8S_SECRET_ENV entry %d: name must be non-empty", i)
		}
		if ref.Secret == "" {
			return nil, fmt.Errorf("validating GC_K8S_SECRET_ENV entry %d (%q): secret must be non-empty", i, ref.Name)
		}
		if ref.Key == "" {
			return nil, fmt.Errorf("validating GC_K8S_SECRET_ENV entry %d (%q): key must be non-empty", i, ref.Name)
		}
		if processenv.IsControllerOnlyEnv(ref.Name) {
			return nil, fmt.Errorf("validating GC_K8S_SECRET_ENV entry %q: controller-only environment variable is not allowed", ref.Name)
		}
		if _, ok := seen[ref.Name]; ok {
			return nil, fmt.Errorf("validating GC_K8S_SECRET_ENV: duplicate environment variable %q", ref.Name)
		}
		seen[ref.Name] = struct{}{}
		if errs := validation.IsEnvVarName(ref.Name); len(errs) > 0 {
			return nil, fmt.Errorf("validating GC_K8S_SECRET_ENV entry %q: invalid environment variable name: %s", ref.Name, strings.Join(errs, "; "))
		}
		if errs := validation.IsDNS1123Subdomain(ref.Secret); len(errs) > 0 {
			return nil, fmt.Errorf("validating GC_K8S_SECRET_ENV entry %q: invalid Secret name %q: %s", ref.Name, ref.Secret, strings.Join(errs, "; "))
		}
		if errs := validation.IsConfigMapKey(ref.Key); len(errs) > 0 {
			return nil, fmt.Errorf("validating GC_K8S_SECRET_ENV entry %q: invalid Secret data key %q: %s", ref.Name, ref.Key, strings.Join(errs, "; "))
		}
	}
	return refs, nil
}
