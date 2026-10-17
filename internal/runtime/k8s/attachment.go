package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

type podDoltBinding struct {
	name, controllerRoot, podRoot string
	host, port, user, password    string
	tls, tlsCA, credentialCommand string
	managedLocal                  bool
}

type projectIDReconciler func(context.Context, podDoltBinding, contract.DoltConnectionTarget, string) error

// stageRequiredStoreFilesInPod stages only canonical Dolt identity inputs.
// External rig roots map to a deterministic pod path instead of being
// rejected or treated as part of the city volume.
func stageRequiredStoreFilesInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, workDir string) error {
	cityRoot := controllerCityPath(cfg.Env)
	activeRoot, err := controllerStoreRoot(cfg.Env)
	if err != nil {
		return err
	}
	if activeRoot == "" {
		activeRoot = cityRoot
	}
	if cfg.Env["GC_K8S_CITY_ATTACH_REQUIRED"] == "1" {
		if err := stageCanonicalStoreFilesInPod(ctx, ops, podName, cityRoot, podWorkspaceRoot); err != nil {
			return fmt.Errorf("projecting canonical city store files: %w", err)
		}
		if ca := strings.TrimSpace(cfg.Env["GC_K8S_CITY_DOLT_CA"]); ca != "" {
			if err := stageDoltCAInPod(ctx, ops, podName, ca); err != nil {
				return fmt.Errorf("projecting city Dolt CA: %w", err)
			}
		}
	}
	if cfg.Env["GC_K8S_ATTACH_REQUIRED"] == "1" && !sameControllerPath(activeRoot, cityRoot) {
		activePodRoot, err := projectedPodStoreRoot(cfg, workDir)
		if err != nil {
			return err
		}
		if err := stageCanonicalStoreFilesInPod(ctx, ops, podName, activeRoot, activePodRoot); err != nil {
			return fmt.Errorf("projecting canonical active store files: %w", err)
		}
		if ca := strings.TrimSpace(cfg.Env["BEADS_DOLT_SERVER_CA"]); ca != "" {
			if err := stageDoltCAInPod(ctx, ops, podName, ca); err != nil {
				return fmt.Errorf("projecting active Dolt CA: %w", err)
			}
		}
	}
	return nil
}

// initBeadsInPod verifies required Dolt scopes before the workspace readiness
// gate opens. It never creates, repairs, or changes .beads state.
func initBeadsInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, workDir, managedServiceHost, managedServicePort string, reconciler ...projectIDReconciler) error {
	var reconcile projectIDReconciler
	if len(reconciler) != 0 {
		reconcile = reconciler[0]
	}
	cityRoot := controllerCityPath(cfg.Env)
	activeRoot, err := controllerStoreRoot(cfg.Env)
	if err != nil {
		return err
	}
	if activeRoot == "" {
		activeRoot = cityRoot
	}
	cityRequired, err := controllerScopeIsDolt(cityRoot)
	if err != nil {
		return fmt.Errorf("city storage contract is unreadable: %w", err)
	}
	activeRequired := false
	if !sameControllerPath(cityRoot, activeRoot) {
		activeRequired, err = controllerScopeIsDolt(activeRoot)
		if err != nil {
			return fmt.Errorf("active storage contract is unreadable: %w", err)
		}
	}
	if cityRequired != (cfg.Env["GC_K8S_CITY_ATTACH_REQUIRED"] == "1") {
		return errors.New("city Dolt attachment requirement is missing or inconsistent; resolve the city store contract before launching K8s")
	}
	if activeRequired != (cfg.Env["GC_K8S_ATTACH_REQUIRED"] == "1") {
		return errors.New("active Dolt attachment requirement is missing or inconsistent; resolve the active store contract before launching K8s")
	}
	if cityRequired {
		binding := podDoltBinding{
			name: "city", controllerRoot: cityRoot, podRoot: podWorkspaceRoot,
			host: cfg.Env["GC_K8S_CITY_DOLT_HOST"], port: cfg.Env["GC_K8S_CITY_DOLT_PORT"],
			user: cfg.Env["GC_K8S_CITY_DOLT_USER"], password: cfg.Env["GC_K8S_CITY_DOLT_PASSWORD"],
			tls: cfg.Env["GC_K8S_CITY_DOLT_TLS"], tlsCA: cfg.Env["GC_K8S_CITY_DOLT_CA"],
			credentialCommand: cfg.Env["GC_K8S_CITY_DOLT_CREDENTIAL_COMMAND"],
			managedLocal:      cfg.Env["GC_K8S_CITY_DOLT_MANAGED_LOCAL"] == "1",
		}
		if err := resolveAndAttachDoltScope(ctx, ops, podName, binding, cityRoot, managedServiceHost, managedServicePort, reconcile); err != nil {
			return fmt.Errorf("attaching city Dolt scope: %w", err)
		}
	}
	if activeRequired {
		activePodRoot, err := projectedPodStoreRoot(cfg, workDir)
		if err != nil {
			return err
		}
		binding := podDoltBinding{
			name: "active", controllerRoot: activeRoot, podRoot: activePodRoot,
			host: cfg.Env["GC_DOLT_HOST"], port: cfg.Env["GC_DOLT_PORT"],
			user: cfg.Env["GC_DOLT_USER"], password: cfg.Env["GC_DOLT_PASSWORD"],
			tls: cfg.Env["BEADS_DOLT_SERVER_TLS"], tlsCA: cfg.Env["BEADS_DOLT_SERVER_CA"],
			credentialCommand: cfg.Env["BEADS_DOLT_CREDENTIAL_COMMAND"],
			managedLocal:      cfg.Env["GC_K8S_DOLT_MANAGED_LOCAL"] == "1",
		}
		if err := resolveAndAttachDoltScope(ctx, ops, podName, binding, cityRoot, managedServiceHost, managedServicePort, reconcile); err != nil {
			return fmt.Errorf("attaching active Dolt scope: %w", err)
		}
	}
	return nil
}

func controllerScopeIsDolt(scopeRoot string) (bool, error) {
	if strings.TrimSpace(scopeRoot) == "" {
		return false, nil
	}
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
	if err != nil {
		return false, err
	}
	return ok && strings.TrimSpace(state.Backend) == "dolt", nil
}

func sameControllerPath(a, b string) bool {
	return filepath.Clean(strings.TrimSpace(a)) == filepath.Clean(strings.TrimSpace(b))
}

func resolveAndAttachDoltScope(ctx context.Context, ops k8sOps, podName string, binding podDoltBinding, cityRoot, managedServiceHost, managedServicePort string, reconcile projectIDReconciler) error {
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityRoot, binding.controllerRoot)
	if err != nil {
		return fmt.Errorf("resolved %s store endpoint is unavailable: %w", binding.name, err)
	}
	if target.Socket != "" || strings.EqualFold(strings.TrimSpace(target.DoltMode), "proxied-server") {
		return fmt.Errorf("resolved %s store transport %q is unsupported by the K8s SQL identity verifier", binding.name, target.DoltMode)
	}
	database := strings.TrimSpace(target.Database)
	if database == "" {
		return fmt.Errorf("resolved %s store database is missing", binding.name)
	}
	managedLocal := !target.External && (target.EndpointOrigin == contract.EndpointOriginManagedCity || target.EndpointOrigin == contract.EndpointOriginInheritedCity)
	if managedLocal != binding.managedLocal {
		return fmt.Errorf("resolved %s endpoint provenance does not match the projected K8s attachment mode", binding.name)
	}
	if target.External && target.EndpointStatus != contract.EndpointStatusVerified {
		return fmt.Errorf("external %s endpoint is not verified in its owning scope", binding.name)
	}
	host, port := strings.TrimSpace(target.Host), strings.TrimSpace(target.Port)
	if binding.port != "" && strings.TrimSpace(binding.port) != port {
		return fmt.Errorf("projected %s port conflicts with the resolved store binding", binding.name)
	}
	if managedLocal {
		host = strings.TrimSpace(managedServiceHost)
		port = strings.TrimSpace(managedServicePort)
		if host == "" {
			host = podManagedDoltHost
		}
		if port == "" {
			port = podManagedDoltPort
		}
	} else if binding.host != "" && strings.TrimSpace(binding.host) != host {
		return fmt.Errorf("projected %s host conflicts with the resolved external endpoint", binding.name)
	}
	if host == "" || port == "" {
		return fmt.Errorf("resolved %s SQL endpoint is incomplete", binding.name)
	}
	if strings.TrimSpace(binding.credentialCommand) != "" {
		return fmt.Errorf("%s credential command must be resolved to a controller-projected SQL credential before K8s attachment", binding.name)
	}
	user := strings.TrimSpace(binding.user)
	resolvedUser := strings.TrimSpace(target.User)
	if user != "" && resolvedUser != "" && user != resolvedUser {
		return fmt.Errorf("projected %s SQL user conflicts with the resolved store binding", binding.name)
	}
	if user == "" {
		user = resolvedUser
	}
	if user == "" {
		return fmt.Errorf("resolved %s SQL user is missing", binding.name)
	}
	target.User = user
	target.Database = database
	if err := parseDoltTLS(binding.tls); err != nil {
		return fmt.Errorf("%s endpoint: %w", binding.name, err)
	}
	tlsValue := strings.TrimSpace(strings.ToLower(binding.tls))
	tlsEnabled := tlsValue == "1" || tlsValue == "true"
	caPath := ""
	if strings.TrimSpace(binding.tlsCA) != "" {
		if !tlsEnabled {
			return fmt.Errorf("%s CA certificate was configured while TLS is disabled", binding.name)
		}
		if _, err := os.Stat(binding.tlsCA); err != nil {
			return fmt.Errorf("%s TLS CA certificate is unavailable", binding.name)
		}
		caPath = podDoltCAPath(binding.tlsCA)
	}
	expectedID, err := readControllerScopeProjectID(binding.controllerRoot, database)
	if err != nil {
		if reconcile != nil {
			if reconcileErr := reconcile(ctx, binding, target, cityRoot); reconcileErr == nil {
				if stageErr := stageCanonicalStoreFilesInPod(ctx, ops, podName, binding.controllerRoot, binding.podRoot); stageErr == nil {
					expectedID, err = readControllerScopeProjectID(binding.controllerRoot, database)
				} else {
					err = stageErr
				}
			}
		}
		if err != nil {
			metadataPath := filepath.Join(binding.controllerRoot, ".beads", "metadata.json")
			return fmt.Errorf("%s canonical project identity is missing or inconsistent; reconcile the owning store first with `gc dolt-state ensure-project-id --metadata %s --database %s --city %s`", binding.name, metadataPath, database, cityRoot)
		}
	}
	if err := attachDoltScopeInPod(ctx, ops, podName, binding.podRoot, database, expectedID, host, port, user, binding.password, tlsEnabled, caPath); err != nil {
		return fmt.Errorf("%s read-only SQL identity verification failed", binding.name)
	}
	return nil
}

// reconcileControllerDoltProjectID invokes the explicit owning-store
// migration command before the pod's verifier attaches. The migration receives
// the independently resolved target and scope credentials; the in-pod attach
// remains strictly read-only.
func reconcileControllerDoltProjectID(ctx context.Context, binding podDoltBinding, target contract.DoltConnectionTarget, cityRoot string) error {
	executable, err := os.Executable()
	if err != nil || strings.TrimSpace(executable) == "" {
		return errors.New("controller executable is unavailable")
	}
	if target.Socket != "" || strings.TrimSpace(target.Host) == "" || strings.TrimSpace(target.Port) == "" {
		return errors.New("resolved owning-store SQL endpoint is unsupported")
	}
	metadataPath := filepath.Join(binding.controllerRoot, ".beads", "metadata.json")
	args := []string{
		"dolt-state", "ensure-project-id",
		"--metadata", metadataPath,
		"--city", cityRoot,
		"--host", strings.TrimSpace(target.Host),
		"--port", strings.TrimSpace(target.Port),
		"--user", strings.TrimSpace(target.User),
		"--database", strings.TrimSpace(target.Database),
	}
	tlsEnabled := strings.EqualFold(strings.TrimSpace(binding.tls), "true") || strings.TrimSpace(binding.tls) == "1"
	if tlsEnabled {
		args = append(args, "--tls")
		if strings.TrimSpace(binding.tlsCA) != "" {
			args = append(args, "--tls-ca", strings.TrimSpace(binding.tlsCA))
		}
	} else if strings.TrimSpace(binding.tlsCA) != "" {
		return errors.New("resolved CA certificate requires TLS")
	}
	commandCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, executable, args...)
	command.Env = replaceEnvironmentValue(os.Environ(), "GC_DOLT_PASSWORD", binding.password)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("owning-store project identity reconciliation failed")
	}
	return nil
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	if value != "" {
		result = append(result, prefix+value)
	}
	return result
}

func readControllerScopeProjectID(scopeRoot, expectedDatabase string) (string, error) {
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	if _, err := os.Stat(filepath.Join(scopeRoot, ".beads", "config.yaml")); err != nil {
		return "", err
	}
	identityID, identityOK, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil || !identityOK || strings.TrimSpace(identityID) == "" {
		return "", errors.New("canonical identity is missing")
	}
	database, databaseOK, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
	if err != nil || !databaseOK || strings.TrimSpace(database) != strings.TrimSpace(expectedDatabase) {
		return "", errors.New("canonical database is missing or mismatched")
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", err
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", err
	}
	metadataID, _ := metadata["project_id"].(string)
	if strings.TrimSpace(metadataID) == "" || strings.TrimSpace(metadataID) != strings.TrimSpace(identityID) {
		return "", errors.New("canonical metadata project identity is missing or mismatched")
	}
	return strings.TrimSpace(identityID), nil
}

func parseDoltTLS(value string) error {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", "0", "false", "1", "true":
		return nil
	default:
		return errors.New("resolved store TLS mode is unsupported")
	}
}

func podDoltCAPath(controllerPath string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(strings.TrimSpace(controllerPath))))
	return filepath.Join(podWorkspaceRoot, ".gc", "store-certs", hex.EncodeToString(sum[:])+".pem")
}

func stageDoltCAInPod(ctx context.Context, ops k8sOps, podName, controllerPath string) error {
	data, err := os.ReadFile(controllerPath)
	if err != nil {
		return err
	}
	stageDir, err := os.MkdirTemp("", "gc-k8s-dolt-ca-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir) //nolint:errcheck
	stagePath := filepath.Join(stageDir, "ca.pem")
	if err := os.WriteFile(stagePath, data, 0o644); err != nil {
		return err
	}
	return copyToPod(ctx, ops, podName, "agent", stagePath, podDoltCAPath(controllerPath))
}

// attachDoltScopeInPod verifies the pod-visible canonical identity and remote
// SQL project id. Password and CA contents travel through protected files or
// tar stdin; credentials never appear in exec argv or error text.
func attachDoltScopeInPod(ctx context.Context, ops k8sOps, podName, scopeRoot, database, expectedID, host, port, user, password string, tlsEnabled bool, tlsCA string) error {
	const attachScript = `set +x
set -eu
umask 077
dir=$(mktemp -d /tmp/gc-store-check.XXXXXX)
chmod 700 "$dir"
envfile="$dir/env"
cleanup() { rm -rf "$dir"; }
trap cleanup EXIT HUP INT TERM
( set -C; cat > "$envfile" )
chmod 600 "$envfile"
set -a
. "$envfile"
set +a
SCOPE=$1
META="$SCOPE/.beads/metadata.json"
test -s "$META" && test -f "$SCOPE/.beads/config.yaml"
exec gc dolt-state verify-project-id --metadata "$META" --scope "$SCOPE" --database "$2" --expected-project-id "$3" --host "$4" --port "$5" --user "$6" --transport tcp --tls="$7" --tls-ca "$8" >/dev/null 2>&1`
	enabled := "false"
	if tlsEnabled {
		enabled = "true"
	}
	envBody := "GC_DOLT_PASSWORD=" + shellquote.Quote(password) + "\n"
	args := []string{"sh", "-c", attachScript, "sh", scopeRoot, database, expectedID, host, port, user, enabled, tlsCA}
	if _, err := ops.execInPod(ctx, podName, "agent", args, strings.NewReader(envBody)); err != nil {
		return errors.New("store identity verification command failed")
	}
	return nil
}
