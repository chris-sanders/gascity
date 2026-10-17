package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/doltpool"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
)

type managedDoltProjectIDReport struct {
	ProjectID           string
	MetadataUpdated     bool
	DatabaseUpdated     bool
	IdentityFileUpdated bool
	Source              string
	Layer               string
}

var (
	projectIdentityDisplayPath  = filepath.ToSlash(contract.ProjectIdentityPath(""))
	projectIdentityProjectIDRef = projectIdentityDisplayPath + "#project.id"
)

type reconcileAction int

const (
	actionNoOp reconcileAction = iota
	actionRefuseL1L3Mismatch
	actionRepairL2
	actionSeedL3
	actionRepairL2SeedL3
	actionSeedL2
	actionSeedL2L3
	actionMigrateFromL2
	actionRefuseLegacyMismatch
	actionMigrateL1SeedL3
	actionAdoptFromL3SeedL2
	actionGenerate
)

type reconcileDecision struct {
	Action     reconcileAction
	ResolvedID string
	L1ID       string
	L2ID       string
	L3ID       string
	Source     string
	Layer      string
	WriteL1    bool
	WriteL2    bool
	WriteL3    bool
}

func newEnsureProjectIDCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		metadataPath string
		host         string
		port         string
		user         string
		database     string
		cityPath     string
		tlsCA        string
		tlsEnabled   bool
	)
	cmd := &cobra.Command{
		Use:    "ensure-project-id",
		Short:  "Ensure local metadata and the Dolt metadata table share a project identity",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rec, closeRecorder := openProjectIdentityEventRecorder(cityPath, stderr)
			defer closeRecorder()
			target := contract.DoltConnectionTarget{
				Host: host, Port: port, User: user, Database: database,
			}
			if strings.TrimSpace(port) == "" {
				var err error
				target, err = resolveProviderOwnedProjectIdentityTarget(metadataPath, cityPath, database, user)
				if err != nil {
					fmt.Fprintf(stderr, "gc dolt-state ensure-project-id: %v\n", err) //nolint:errcheck
					return errExit
				}
			}
			report, err := ensureManagedDoltProjectIDAtTargetTLS(metadataPath, target, cityPath, rec, tlsEnabled, tlsCA)
			if err != nil {
				fmt.Fprintf(stderr, "gc dolt-state ensure-project-id: %v\n", err) //nolint:errcheck
				return errExit
			}
			for _, line := range managedDoltProjectIDFields(report) {
				if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
					fmt.Fprintf(stderr, "gc dolt-state ensure-project-id: %v\n", writeErr) //nolint:errcheck
					return errExit
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&metadataPath, "metadata", "", "path to .beads/metadata.json")
	cmd.Flags().StringVar(&host, "host", "", "Dolt host")
	cmd.Flags().StringVar(&port, "port", "", "Dolt port")
	cmd.Flags().StringVar(&user, "user", "", "Dolt user")
	cmd.Flags().StringVar(&database, "database", "", "Dolt database")
	cmd.Flags().StringVar(&cityPath, "city", "", "city root (required for event emission)")
	cmd.Flags().BoolVar(&tlsEnabled, "tls", false, "use verified TLS for the resolved endpoint")
	cmd.Flags().StringVar(&tlsCA, "tls-ca", "", "optional CA certificate file for the resolved TLS endpoint")
	_ = cmd.MarkFlagRequired("city")
	_ = cmd.MarkFlagRequired("metadata")
	_ = cmd.MarkFlagRequired("database")
	return cmd
}

// newVerifyProjectIDCmd is the read-only attachment gate used by the native
// Kubernetes runtime. It requires the controller-resolved expected identity,
// explicit database/user/endpoint values, and matching L1, L2, and remote L3
// identities. It never seeds, adopts, repairs, or emits identity stamps.
func newVerifyProjectIDCmd(stdout, stderr io.Writer) *cobra.Command {
	var metadataPath, scopeRoot, database, expectedID, host, port, user, transport, tlsCA string
	var tlsEnabled bool
	cmd := &cobra.Command{
		Use:    "verify-project-id",
		Short:  "Verify local and remote project identity without mutation",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := verifyLocalProjectIdentity(metadataPath, scopeRoot, database, expectedID); err != nil {
				fmt.Fprintf(stderr, "gc dolt-state verify-project-id: %v\n", err) //nolint:errcheck
				return errExit
			}
			if strings.TrimSpace(transport) != "tcp" {
				fmt.Fprintln(stderr, "gc dolt-state verify-project-id: unsupported transport") //nolint:errcheck
				return errExit
			}
			user = strings.TrimSpace(user)
			if user == "" {
				fmt.Fprintln(stderr, "gc dolt-state verify-project-id: resolved database user is missing") //nolint:errcheck
				return errExit
			}
			portNumber, portErr := strconv.Atoi(strings.TrimSpace(port))
			if portErr != nil || portNumber < 1 || portNumber > 65535 || strings.TrimSpace(host) == "" {
				fmt.Fprintln(stderr, "gc dolt-state verify-project-id: resolved TCP endpoint is invalid") //nolint:errcheck
				return errExit
			}
			cfg := mysql.NewConfig()
			cfg.User = user
			cfg.Passwd = os.Getenv("GC_DOLT_PASSWORD")
			cfg.Net = "tcp"
			cfg.Addr = net.JoinHostPort(strings.TrimSpace(host), strings.TrimSpace(port))
			cfg.DBName = strings.TrimSpace(database)
			cfg.ParseTime = true
			if tlsEnabled {
				tlsConfig, tlsErr := verifiedDoltTLSConfig(host, tlsCA)
				if tlsErr != nil {
					fmt.Fprintln(stderr, "gc dolt-state verify-project-id: verified TLS trust configuration is invalid") //nolint:errcheck
					return errExit
				}
				cfg.TLS = tlsConfig
			} else if strings.TrimSpace(tlsCA) != "" {
				fmt.Fprintln(stderr, "gc dolt-state verify-project-id: TLS CA was supplied while TLS is disabled") //nolint:errcheck
				return errExit
			}
			connector, err := mysql.NewConnector(cfg)
			if err != nil {
				fmt.Fprintln(stderr, "gc dolt-state verify-project-id: could not create identity connector") //nolint:errcheck
				return errExit
			}
			db := sql.OpenDB(connector)
			db.SetMaxOpenConns(1)
			defer db.Close() //nolint:errcheck // one-shot verification connection
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := db.PingContext(ctx); err != nil {
				fmt.Fprintln(stderr, "gc dolt-state verify-project-id: database identity target unavailable") //nolint:errcheck
				return errExit
			}
			var connectedDatabase string
			if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&connectedDatabase); err != nil || strings.TrimSpace(connectedDatabase) != strings.TrimSpace(database) {
				fmt.Fprintln(stderr, "gc dolt-state verify-project-id: connected database does not match the resolved database") //nolint:errcheck
				return errExit
			}
			remoteID, ok, err := readDatabaseProjectID(ctx, db)
			if err != nil || !ok || remoteID != strings.TrimSpace(expectedID) {
				fmt.Fprintln(stderr, "gc dolt-state verify-project-id: remote project identity is missing or mismatched") //nolint:errcheck
				return errExit
			}
			_, _ = fmt.Fprintln(stdout, "verified")
			return nil
		},
	}
	cmd.Flags().StringVar(&metadataPath, "metadata", "", "path to .beads/metadata.json")
	cmd.Flags().StringVar(&scopeRoot, "scope", "", "resolved store scope root")
	cmd.Flags().StringVar(&database, "database", "", "resolved Dolt database")
	cmd.Flags().StringVar(&expectedID, "expected-project-id", "", "controller-resolved project identity")
	cmd.Flags().StringVar(&host, "host", "", "resolved Dolt host")
	cmd.Flags().StringVar(&port, "port", "", "resolved Dolt port")
	cmd.Flags().StringVar(&user, "user", "", "resolved Dolt user")
	cmd.Flags().StringVar(&transport, "transport", "tcp", "resolved transport")
	cmd.Flags().BoolVar(&tlsEnabled, "tls", false, "use verified TLS for the resolved endpoint")
	cmd.Flags().StringVar(&tlsCA, "tls-ca", "", "optional CA certificate file for the resolved TLS endpoint")
	for _, flag := range []string{"metadata", "scope", "database", "expected-project-id", "host", "port", "user"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func verifiedDoltTLSConfig(serverName, caPath string) (*tls.Config, error) {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return nil, fmt.Errorf("TLS server name is empty")
	}
	config := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if strings.TrimSpace(caPath) == "" {
		return config, nil
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA file contains no certificates")
	}
	config.RootCAs = roots
	return config, nil
}

func verifyLocalProjectIdentity(metadataPath, scopeRoot, database, expectedID string) error {
	metadataPath = strings.TrimSpace(metadataPath)
	scopeRoot = strings.TrimSpace(scopeRoot)
	database = strings.TrimSpace(database)
	expectedID = strings.TrimSpace(expectedID)
	if metadataPath == "" || scopeRoot == "" || database == "" || expectedID == "" {
		return fmt.Errorf("incomplete local identity expectation")
	}
	derivedScope, err := scopeRootFromMetadataPath(metadataPath)
	if err != nil || !samePath(derivedScope, scopeRoot) {
		return fmt.Errorf("metadata path does not belong to resolved scope")
	}
	identityID, identityOK, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil || !identityOK || identityID != expectedID {
		return fmt.Errorf("canonical local project identity is missing or mismatched")
	}
	metadataID, err := readManagedMetadataProjectID(metadataPath)
	if err != nil || metadataID == "" || metadataID != expectedID {
		return fmt.Errorf("metadata project identity is missing or mismatched")
	}
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metadataPath)
	if err != nil || !ok {
		return fmt.Errorf("store metadata is unavailable or invalid")
	}
	resolvedDatabase := strings.TrimSpace(state.DoltDatabase)
	if resolvedDatabase == "" || resolvedDatabase != database {
		return fmt.Errorf("store database does not match resolved database")
	}
	return nil
}

func openProjectIdentityEventRecorder(cityPath string, stderr io.Writer) (events.Recorder, func()) {
	rec, err := openCityEventsLog(cityPath, io.Discard)
	if err != nil {
		fmt.Fprintf(stderr, "gc dolt-state ensure-project-id: events recorder unavailable: %v\n", err) //nolint:errcheck
		return events.Discard, func() {}
	}
	return rec, func() {
		_ = rec.Close()
	}
}

func ensureManagedDoltProjectIDWithRecorder(metadataPath, host, port, user, database string, cityPath string, rec events.Recorder) (managedDoltProjectIDReport, error) {
	return ensureManagedDoltProjectIDAtTarget(metadataPath, contract.DoltConnectionTarget{
		Host: host, Port: port, User: user, Database: database,
	}, cityPath, rec)
}

func ensureManagedDoltProjectIDAtTarget(metadataPath string, target contract.DoltConnectionTarget, cityPath string, rec events.Recorder) (managedDoltProjectIDReport, error) {
	return ensureManagedDoltProjectIDAtTargetTLS(metadataPath, target, cityPath, rec, false, "")
}

func ensureManagedDoltProjectIDAtTargetTLS(metadataPath string, target contract.DoltConnectionTarget, cityPath string, rec events.Recorder, tlsEnabled bool, tlsCA string) (managedDoltProjectIDReport, error) {
	metadataPath = strings.TrimSpace(metadataPath)
	if metadataPath == "" {
		return managedDoltProjectIDReport{}, fmt.Errorf("missing metadata path")
	}
	scopeRoot, err := scopeRootFromMetadataPath(metadataPath)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}
	target.Database = strings.TrimSpace(target.Database)
	target.Socket = strings.TrimSpace(target.Socket)
	target.Host = strings.TrimSpace(target.Host)
	target.Port = strings.TrimSpace(target.Port)
	target.User = strings.TrimSpace(target.User)
	database := target.Database
	if database == "" {
		return managedDoltProjectIDReport{}, fmt.Errorf("missing database")
	}

	fs := fsys.OSFS{}
	identityProjectID, identityOK, err := contract.ReadProjectIdentity(fs, scopeRoot)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}

	metadataProjectID, err := readManagedMetadataProjectID(metadataPath)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}
	metadataOK := metadataProjectID != ""

	// Non-TLS managed endpoints use the shared Dolt pool. An external TLS
	// migration uses a scoped connector so its verified server name and custom
	// roots cannot be lost in or leak through the process-wide pool.
	var db *sql.DB
	ownedDB := false
	switch {
	case target.Socket != "":
		if tlsEnabled || strings.TrimSpace(tlsCA) != "" {
			return managedDoltProjectIDReport{}, fmt.Errorf("TLS is not supported for a Unix socket identity target")
		}
		db, err = managedDoltOpenDatabaseSocket(target.Socket, target.User, database)
	case tlsEnabled:
		config, configErr := verifiedDoltTLSConfig(target.Host, tlsCA)
		if configErr != nil {
			return managedDoltProjectIDReport{}, fmt.Errorf("resolved TLS trust configuration is invalid")
		}
		mysqlConfig := mysql.NewConfig()
		mysqlConfig.User = target.User
		mysqlConfig.Passwd = os.Getenv("GC_DOLT_PASSWORD")
		mysqlConfig.Net = "tcp"
		mysqlConfig.Addr = net.JoinHostPort(target.Host, target.Port)
		mysqlConfig.DBName = database
		mysqlConfig.ParseTime = true
		mysqlConfig.TLS = config
		connector, connectorErr := mysql.NewConnector(mysqlConfig)
		if connectorErr != nil {
			return managedDoltProjectIDReport{}, fmt.Errorf("could not create verified TLS identity connector")
		}
		db = sql.OpenDB(connector)
		db.SetMaxOpenConns(1)
		ownedDB = true
	default:
		if strings.TrimSpace(tlsCA) != "" {
			return managedDoltProjectIDReport{}, fmt.Errorf("TLS CA was supplied while TLS is disabled")
		}
		db, err = managedDoltOpenDatabase(target.Host, target.Port, target.User, database)
	}
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}
	if ownedDB {
		defer db.Close() //nolint:errcheck // scoped TLS migration connection
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return managedDoltProjectIDReport{}, err
	}

	databaseProjectID, ok, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		return managedDoltProjectIDReport{}, err
	}

	decision := decideReconcile(identityProjectID, identityOK, metadataProjectID, metadataOK, databaseProjectID, ok)
	seedL3 := func(ctx context.Context, projectID string) (bool, error) {
		return seedDatabaseProjectID(ctx, db, projectID)
	}
	return applyReconcileDecision(ctx, fs, scopeRoot, metadataPath, decision, cityPath, rec, seedL3)
}

func resolveProviderOwnedProjectIdentityTarget(metadataPath, cityPath, database, user string) (contract.DoltConnectionTarget, error) {
	return resolveProviderOwnedProjectIdentityTargetWithProcessTable(metadataPath, cityPath, database, user, proxyendpoint.DefaultProcessTable())
}

func resolveProviderOwnedProjectIdentityTargetWithProcessTable(metadataPath, cityPath, database, user string, processTable proxyendpoint.ProcessTable) (contract.DoltConnectionTarget, error) {
	scopeRoot, err := scopeRootFromMetadataPath(metadataPath)
	if err != nil {
		return contract.DoltConnectionTarget{}, err
	}
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
	if err != nil {
		return contract.DoltConnectionTarget{}, fmt.Errorf("resolve provider-owned Dolt binding: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(target.DoltMode), "proxied-server") && target.Socket == "" && target.Port == "" {
		target, err = resolveLocalProxiedProjectIdentityTarget(scopeRoot, target, processTable)
		if err != nil {
			return contract.DoltConnectionTarget{}, err
		}
	}
	if target.Socket == "" && target.Port == "" {
		return contract.DoltConnectionTarget{}, fmt.Errorf("provider-owned Dolt binding for %s has no SQL endpoint", scopeRoot)
	}
	target.Database = strings.TrimSpace(database)
	if strings.TrimSpace(user) != "" {
		target.User = strings.TrimSpace(user)
	}
	return target, nil
}

func resolveLocalProxiedProjectIdentityTarget(scopeRoot string, target contract.DoltConnectionTarget, processTable proxyendpoint.ProcessTable) (contract.DoltConnectionTarget, error) {
	root, err := proxyendpoint.ProviderRoot(scopeRoot)
	if err != nil {
		return contract.DoltConnectionTarget{}, fmt.Errorf("resolve provider-owned proxy root for %s: %w", scopeRoot, err)
	}
	endpoint := proxyendpoint.Inspect(root, processTable)
	if !endpoint.Verdict.Live() {
		return contract.DoltConnectionTarget{}, fmt.Errorf("provider-owned proxy endpoint %s is not live (%s): %w", root, endpoint.Verdict, endpoint.Err)
	}
	target.Host = proxyendpoint.Host
	target.Port = strconv.Itoa(endpoint.Record.Port)
	target.Socket = ""
	target.External = false
	return target, nil
}

func managedDoltProjectIDFields(report managedDoltProjectIDReport) []string {
	return []string{
		"project_id\t" + report.ProjectID,
		"metadata_updated\t" + strconv.FormatBool(report.MetadataUpdated),
		"database_updated\t" + strconv.FormatBool(report.DatabaseUpdated),
		"source\t" + report.Source,
		"identity_file_updated\t" + strconv.FormatBool(report.IdentityFileUpdated),
		"layer\t" + report.Layer,
	}
}

func scopeRootFromMetadataPath(metadataPath string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(metadataPath))
	if filepath.Base(cleaned) != "metadata.json" || filepath.Base(filepath.Dir(cleaned)) != ".beads" {
		return "", fmt.Errorf("metadata path %q is not <scope>/.beads/metadata.json", metadataPath)
	}
	return filepath.Dir(filepath.Dir(cleaned)), nil
}

func decideReconcile(l1 string, l1ok bool, l2 string, l2ok bool, l3 string, l3ok bool) reconcileDecision {
	if l1ok {
		switch {
		case l2ok && l3ok:
			switch {
			case l1 == l2 && l2 == l3:
				return reconcileDecision{Action: actionNoOp, ResolvedID: l1, Source: "match", Layer: "l1"}
			case l1 == l2 && l1 != l3:
				return reconcileDecision{Action: actionRefuseL1L3Mismatch, L1ID: l1, L2ID: l2, L3ID: l3}
			case l1 != l2 && l1 == l3:
				return reconcileDecision{Action: actionRepairL2, ResolvedID: l1, L1ID: l1, L2ID: l2, L3ID: l3, Source: "l2-repair", Layer: "l1", WriteL2: true}
			default:
				return reconcileDecision{Action: actionRefuseL1L3Mismatch, L1ID: l1, L2ID: l2, L3ID: l3}
			}
		case l2ok && !l3ok:
			if l1 == l2 {
				return reconcileDecision{Action: actionSeedL3, ResolvedID: l1, L1ID: l1, L2ID: l2, Source: "l3-seed", Layer: "l1", WriteL3: true}
			}
			return reconcileDecision{Action: actionRepairL2SeedL3, ResolvedID: l1, L1ID: l1, L2ID: l2, Source: "l2-repair-l3-seed", Layer: "l1", WriteL2: true, WriteL3: true}
		case !l2ok && l3ok:
			if l1 == l3 {
				return reconcileDecision{Action: actionSeedL2, ResolvedID: l1, L1ID: l1, L3ID: l3, Source: "l2-seed", Layer: "l1", WriteL2: true}
			}
			return reconcileDecision{Action: actionRefuseL1L3Mismatch, L1ID: l1, L3ID: l3}
		default:
			return reconcileDecision{Action: actionSeedL2L3, ResolvedID: l1, L1ID: l1, Source: "l2-l3-seed", Layer: "l1", WriteL2: true, WriteL3: true}
		}
	}

	switch {
	case l2ok && l3ok:
		if l2 == l3 {
			return reconcileDecision{Action: actionMigrateFromL2, ResolvedID: l2, L2ID: l2, L3ID: l3, Source: "l1-migrate-from-l2", Layer: "l2", WriteL1: true}
		}
		return reconcileDecision{Action: actionRefuseLegacyMismatch, L2ID: l2, L3ID: l3}
	case l2ok && !l3ok:
		return reconcileDecision{Action: actionMigrateL1SeedL3, ResolvedID: l2, L2ID: l2, Source: "l1-migrate-l3-seed", Layer: "l2", WriteL1: true, WriteL3: true}
	case !l2ok && l3ok:
		return reconcileDecision{Action: actionAdoptFromL3SeedL2, ResolvedID: l3, L3ID: l3, Source: "l1-adopt-l2-seed", Layer: "l3", WriteL1: true, WriteL2: true}
	default:
		return reconcileDecision{Action: actionGenerate, Source: "generated", Layer: "generated", WriteL1: true, WriteL2: true, WriteL3: true}
	}
}

func applyReconcileDecision(ctx context.Context, fs fsys.FS, scopeRoot string, metadataPath string, decision reconcileDecision, cityPath string, rec events.Recorder, seedL3 func(context.Context, string) (bool, error)) (managedDoltProjectIDReport, error) {
	report := managedDoltProjectIDReport{
		ProjectID: decision.ResolvedID,
		Source:    decision.Source,
		Layer:     decision.Layer,
	}

	switch decision.Action {
	case actionNoOp:
		return report, nil
	case actionRefuseL1L3Mismatch:
		return managedDoltProjectIDReport{}, formatL1L3MismatchError(decision.L1ID, decision.L3ID)
	case actionRefuseLegacyMismatch:
		return managedDoltProjectIDReport{}, formatLegacyL2L3MismatchError(decision.L2ID, decision.L3ID)
	case actionRepairL2:
		updated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.MetadataUpdated = updated
		if updated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L2", decision.L2ID, decision.ResolvedID)
		}
		return report, nil
	case actionSeedL3:
		updated, err := seedL3(ctx, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.DatabaseUpdated = updated
		if updated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L3", "", decision.ResolvedID)
		}
		return report, nil
	case actionRepairL2SeedL3:
		metaUpdated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if metaUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L2", decision.L2ID, decision.ResolvedID)
		}
		dbUpdated, err := seedL3(ctx, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if dbUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L3", "", decision.ResolvedID)
		}
		report.MetadataUpdated = metaUpdated
		report.DatabaseUpdated = dbUpdated
		return report, nil
	case actionSeedL2:
		updated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.MetadataUpdated = updated
		if updated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L2", "", decision.ResolvedID)
		}
		return report, nil
	case actionSeedL2L3:
		metaUpdated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if metaUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L2", "", decision.ResolvedID)
		}
		dbUpdated, err := seedL3(ctx, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if dbUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "cache_repair", "L3", "", decision.ResolvedID)
		}
		report.MetadataUpdated = metaUpdated
		report.DatabaseUpdated = dbUpdated
		return report, nil
	case actionMigrateFromL2:
		updated, err := writeProjectIdentityIfNeeded(fs, scopeRoot, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.IdentityFileUpdated = updated
		if updated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_metadata", "L1", "", decision.ResolvedID)
		}
		return report, nil
	case actionMigrateL1SeedL3:
		identityUpdated, err := writeProjectIdentityIfNeeded(fs, scopeRoot, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if identityUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_metadata", "L1", "", decision.ResolvedID)
		}
		dbUpdated, err := seedL3(ctx, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if dbUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_metadata", "L3", "", decision.ResolvedID)
		}
		report.IdentityFileUpdated = identityUpdated
		report.DatabaseUpdated = dbUpdated
		return report, nil
	case actionAdoptFromL3SeedL2:
		identityUpdated, err := writeProjectIdentityIfNeeded(fs, scopeRoot, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if identityUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_database", "L1", "", decision.ResolvedID)
		}
		metaUpdated, err := writeManagedMetadataProjectID(metadataPath, decision.ResolvedID)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		if metaUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "migrated_from_database", "L2", "", decision.ResolvedID)
		}
		report.IdentityFileUpdated = identityUpdated
		report.MetadataUpdated = metaUpdated
		return report, nil
	case actionGenerate:
		projectID, err := generateLocalProjectID()
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		identityUpdated, metaUpdated, dbUpdated, err := writeProjectIdentityToAllLayers(ctx, fs, scopeRoot, projectID, seedL3)
		if err != nil {
			return managedDoltProjectIDReport{}, err
		}
		report.ProjectID = projectID
		report.IdentityFileUpdated = identityUpdated
		report.MetadataUpdated = metaUpdated
		report.DatabaseUpdated = dbUpdated
		if identityUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "generated", "L1", "", projectID)
		}
		if metaUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "generated", "L2", "", projectID)
		}
		if dbUpdated {
			emitProjectIdentityStampedEvent(rec, cityPath, scopeRoot, "generated", "L3", "", projectID)
		}
		return report, nil
	default:
		return managedDoltProjectIDReport{}, fmt.Errorf("unknown project identity reconcile action %d", decision.Action)
	}
}

func emitProjectIdentityStampedEvent(rec events.Recorder, cityPath, scopeRoot, source, layer, oldID, newID string) {
	if rec == nil {
		return
	}
	payload := gcapi.ProjectIdentityStampedPayload{
		ScopeRoot: projectIdentityEventScopeRoot(cityPath, scopeRoot),
		Source:    source,
		Layer:     layer,
		OldID:     oldID,
		NewID:     newID,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	rec.Record(events.Event{
		Type:    events.ProjectIdentityStamped,
		Actor:   "gc dolt-state ensure-project-id",
		Subject: payload.ScopeRoot,
		Payload: data,
	})
}

func projectIdentityEventScopeRoot(cityPath, scopeRoot string) string {
	cityPath = strings.TrimSpace(cityPath)
	scopeRoot = strings.TrimSpace(scopeRoot)
	if cityPath == "" || scopeRoot == "" {
		return filepath.ToSlash(filepath.Clean(scopeRoot))
	}
	rel, err := filepath.Rel(filepath.Clean(cityPath), filepath.Clean(scopeRoot))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(filepath.Clean(scopeRoot))
	}
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func writeProjectIdentityIfNeeded(fs fsys.FS, scopeRoot string, id string) (bool, error) {
	existing, ok, err := contract.ReadProjectIdentity(fs, scopeRoot)
	if err != nil {
		return false, err
	}
	if ok {
		if existing == id {
			return false, nil
		}
		return false, fmt.Errorf("identity %s already has project.id %q, refusing to overwrite with %q", contract.ProjectIdentityPath(scopeRoot), existing, id)
	}
	if err := contract.WriteProjectIdentity(fs, scopeRoot, id); err != nil {
		return false, err
	}
	return true, nil
}

func writeProjectIdentityToAllLayers(ctx context.Context, fs fsys.FS, scopeRoot string, id string, seedL3 func(context.Context, string) (bool, error)) (l1Updated, l2Updated, l3Updated bool, err error) {
	l1Updated, err = writeProjectIdentityIfNeeded(fs, scopeRoot, id)
	if err != nil {
		return false, false, false, err
	}
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	l2Updated, err = writeManagedMetadataProjectID(metadataPath, id)
	if err != nil {
		return l1Updated, false, false, err
	}
	l3Updated, err = seedL3(ctx, id)
	if err != nil {
		return l1Updated, l2Updated, false, err
	}
	return l1Updated, l2Updated, l3Updated, nil
}

func formatL1L3MismatchError(l1, l3 string) error {
	return fmt.Errorf(
		"PROJECT IDENTITY MISMATCH — refusing to connect:\n"+
			"  canonical "+projectIdentityProjectIDRef+" = %q\n"+
			"  database metadata._project_id              = %q\n"+
			"\n"+
			"The git-tracked identity does not match the database stamp. "+
			"The database may belong to a different rig, or the identity "+
			"file may have been changed without re-stamping the database. "+
			"Inspect both values and resolve manually before reconnecting.",
		l1, l3,
	)
}

func formatLegacyL2L3MismatchError(l2, l3 string) error {
	return fmt.Errorf(
		"LEGACY PROJECT IDENTITY MISMATCH — refusing to connect:\n"+
			"  metadata.json project_id      = %q\n"+
			"  database metadata._project_id  = %q\n"+
			"\n"+
			"This rig predates the canonical "+projectIdentityDisplayPath+" file. "+
			"The two legacy storage layers disagree, so we cannot safely "+
			"seed the canonical layer from either one. Inspect both values "+
			"and decide which is correct, then create "+projectIdentityDisplayPath+" "+
			"with the chosen value to unblock reconcile.",
		l2, l3,
	)
}

// managedDoltOpenDatabase returns the shared pooled *sql.DB for a managed
// Dolt database. The handle is owned by internal/doltpool — callers must
// NOT Close it. The previous per-call sql.Open+Close pattern here was the
// 2,618-TIME_WAIT hotspot (city-scale plan item 1.2).
func managedDoltOpenDatabase(host, port, user, database string) (*sql.DB, error) {
	host = managedDoltConnectHost(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return nil, fmt.Errorf("missing port")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	database = strings.TrimSpace(database)
	if database == "" {
		return nil, fmt.Errorf("missing database")
	}
	return doltpool.Open(host, port, user, managedDoltPassword(), database)
}

func managedDoltOpenDatabaseSocket(socket, user, database string) (*sql.DB, error) {
	socket = strings.TrimSpace(socket)
	if socket == "" {
		return nil, fmt.Errorf("missing socket")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	database = strings.TrimSpace(database)
	if database == "" {
		return nil, fmt.Errorf("missing database")
	}
	return doltpool.OpenSocket(socket, user, managedDoltPassword(), database)
}

func readManagedMetadataProjectID(metadataPath string) (string, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse metadata %s: %w", metadataPath, err)
	}
	raw, ok := meta["project_id"]
	if !ok || raw == nil {
		return "", nil
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value), nil
	default:
		projectID := strings.TrimSpace(fmt.Sprint(value))
		if projectID == "" || projectID == "<nil>" || strings.EqualFold(projectID, "null") {
			return "", nil
		}
		return projectID, nil
	}
}

func writeManagedMetadataProjectID(metadataPath, projectID string) (bool, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return false, err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return false, fmt.Errorf("parse metadata %s: %w", metadataPath, err)
	}
	if strings.TrimSpace(fmt.Sprint(meta["project_id"])) == projectID {
		return false, nil
	}
	meta["project_id"] = projectID
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return false, err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(metadataPath, encoded, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func seedDatabaseProjectID(ctx context.Context, db *sql.DB, projectID string) (bool, error) {
	existing, ok, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		return false, err
	}
	if ok {
		if existing != projectID {
			return false, fmt.Errorf("database _project_id %q does not match desired %q", existing, projectID)
		}
		return false, nil
	}
	if err := ensureDatabaseMetadataTable(ctx, db); err != nil {
		return false, err
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO metadata (`key`, value) VALUES ('_project_id', ?) ON DUPLICATE KEY UPDATE value = VALUES(value)", projectID); err != nil {
		return false, fmt.Errorf("seed database _project_id: %w", err)
	}
	return true, nil
}

func ensureDatabaseMetadataTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS metadata (`key` VARCHAR(255) PRIMARY KEY, value LONGTEXT)")
	if err != nil {
		return fmt.Errorf("ensure metadata table: %w", err)
	}
	return nil
}

func generateLocalProjectID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gc-local-" + hex.EncodeToString(buf), nil
}
