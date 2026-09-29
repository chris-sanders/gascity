package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/spf13/cobra"
)

// newInternalProjectK8sLocalStateCmd projects the narrow machine-local state
// that a Kubernetes worker needs when it is an execution environment for the
// same City as the controller. It deliberately does not inherit arbitrary
// .gc/ state or alter gc init --from semantics.
func newInternalProjectK8sLocalStateCmd(stdout, stderr io.Writer) *cobra.Command {
	var sourceRoot, controllerCityRoot, destRoot string
	cmd := &cobra.Command{
		Use:    "project-k8s-local-state",
		Short:  "Project same-City Kubernetes local state",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			roots, err := normalizeK8sLocalStateRoots(sourceRoot, controllerCityRoot, destRoot)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal project-k8s-local-state: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if err := projectK8sLocalState(roots.source, roots.controllerCity, roots.dest); err != nil {
				fmt.Fprintf(stderr, "gc internal project-k8s-local-state: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			fmt.Fprintf(stdout, "projected Kubernetes local state into %s\n", roots.dest) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceRoot, "source-root", "", "copied controller City root")
	cmd.Flags().StringVar(&controllerCityRoot, "controller-city-root", "", "original controller City root")
	cmd.Flags().StringVar(&destRoot, "dest-root", "", "pod-local City root")
	return cmd
}

type k8sLocalStateRoots struct {
	source, controllerCity, dest string
}

func normalizeK8sLocalStateRoots(sourceRoot, controllerCityRoot, destRoot string) (k8sLocalStateRoots, error) {
	source, err := normalizeK8sLocalStateRoot("--source-root", sourceRoot)
	if err != nil {
		return k8sLocalStateRoots{}, err
	}
	controllerCity, err := normalizeK8sLocalStateRoot("--controller-city-root", controllerCityRoot)
	if err != nil {
		return k8sLocalStateRoots{}, err
	}
	dest, err := normalizeK8sLocalStateRoot("--dest-root", destRoot)
	if err != nil {
		return k8sLocalStateRoots{}, err
	}
	return k8sLocalStateRoots{source: source, controllerCity: controllerCity, dest: dest}, nil
}

func normalizeK8sLocalStateRoot(flag, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", flag)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolving %s %q: %w", flag, value, err)
	}
	return filepath.Clean(abs), nil
}

func projectK8sLocalState(sourceRoot, controllerCityRoot, destRoot string) error {
	fs := fsys.OSFS{}
	if err := projectK8sSiteBinding(fs, sourceRoot, controllerCityRoot, destRoot); err != nil {
		return fmt.Errorf("projecting site binding: %w", err)
	}
	if err := projectK8sSuspensionState(fs, sourceRoot, destRoot); err != nil {
		return fmt.Errorf("projecting suspension state: %w", err)
	}
	return nil
}

func projectK8sSiteBinding(fs fsys.FS, sourceRoot, controllerCityRoot, destRoot string) error {
	source, err := config.LoadSiteBinding(fs, sourceRoot)
	if err != nil {
		return err
	}
	destination, err := config.LoadSiteBinding(fs, destRoot)
	if err != nil {
		return fmt.Errorf("loading destination site binding: %w", err)
	}

	// Validate and map every source binding before touching the destination.
	mapped := make([]config.Rig, 0, len(source.Rigs))
	for _, sourceRig := range source.Rigs {
		path := strings.TrimSpace(sourceRig.Path)
		if path == "" {
			continue
		}
		mappedPath, err := remapK8sRigPath(path, controllerCityRoot, destRoot)
		if err != nil {
			return fmt.Errorf("rig %q path %q: %w", sourceRig.Name, sourceRig.Path, err)
		}
		mapped = append(mapped, config.Rig{Name: sourceRig.Name, Path: mappedPath})
	}

	workspaceName := strings.TrimSpace(destination.WorkspaceName)
	if sourceName := strings.TrimSpace(source.WorkspaceName); sourceName != "" {
		workspaceName = sourceName
	}
	workspacePrefix := strings.TrimSpace(destination.WorkspacePrefix)
	if sourcePrefix := strings.TrimSpace(source.WorkspacePrefix); sourcePrefix != "" {
		workspacePrefix = sourcePrefix
	}

	// These are the canonical site-binding writers. Workspace identity is
	// written first so it can preserve the destination field omitted by the
	// source; rig persistence then applies the complete validated projection.
	if err := config.PersistWorkspaceSiteBinding(fs, destRoot, workspaceName, workspacePrefix); err != nil {
		return err
	}
	if err := config.PersistRigSiteBindings(fs, destRoot, mapped); err != nil {
		return err
	}
	return nil
}

func remapK8sRigPath(path, controllerCityRoot, destRoot string) (string, error) {
	controllerPath := path
	if !filepath.IsAbs(controllerPath) {
		controllerPath = filepath.Join(controllerCityRoot, controllerPath)
	}
	controllerPath = filepath.Clean(controllerPath)
	relative, err := filepath.Rel(controllerCityRoot, controllerPath)
	if err != nil {
		return "", fmt.Errorf("resolving path relative to controller City: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes controller City root %q", controllerCityRoot)
	}
	return filepath.Clean(filepath.Join(destRoot, relative)), nil
}

func projectK8sSuspensionState(fs fsys.FS, sourceRoot, destRoot string) error {
	sourcePath := citylayout.SuspensionStateFile(sourceRoot)
	data, err := fs.ReadFile(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	// Load through the existing schema reader before copying the original bytes.
	// Saving the decoded value would rewrite updated_at; preserving the source
	// bytes keeps explicit false/resume state and its timestamp exact.
	if _, err := suspensionstate.Load(fs, sourceRoot); err != nil {
		return fmt.Errorf("validating %q: %w", sourcePath, err)
	}
	destinationPath := citylayout.SuspensionStateFile(destRoot)
	if err := fs.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return fmt.Errorf("creating runtime directory: %w", err)
	}
	if err := fsys.WriteFileAtomic(fs, destinationPath, data, 0o644); err != nil {
		return fmt.Errorf("writing %q: %w", destinationPath, err)
	}
	return nil
}
