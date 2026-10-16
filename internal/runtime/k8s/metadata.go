package k8s

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seedStartMetadata projects only the runtime-approved metadata from the
// session environment into tmux. MetaSeedKeys orders the incarnation token and
// runtime epoch before GC_SESSION_ID so a reader that observes the ID can also
// observe its identity fence. Values stay out of diagnostics.
func (p *Provider) seedStartMetadata(ctx context.Context, podName, sessionName string, env map[string]string) error {
	seed, _ := runtime.SplitEnvForMetaSeed(env)
	for _, key := range runtime.MetaSeedKeys(seed) {
		_, err := p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "set-environment", "-t", tmuxSession, key, seed[key]}, nil)
		if err != nil {
			return fmt.Errorf("k8s runtime metadata projection failed for session %q key %q", sessionName, key)
		}
	}
	return nil
}
