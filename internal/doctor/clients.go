package doctor

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/curruwilla/vaultd/internal/config"
	"github.com/curruwilla/vaultd/internal/core"
	"github.com/curruwilla/vaultd/internal/engine"
	"github.com/curruwilla/vaultd/internal/engine/mongodb"
	"github.com/curruwilla/vaultd/internal/engine/mysql"
	"github.com/curruwilla/vaultd/internal/engine/postgres"
)

// VariantEnv is set in the published container images to the tag they were
// built as: `latest` for the fat image, `pg17` and friends for the slim ones
// (decision D1). Outside a vaultd image it is unset, and doctor says so.
const VariantEnv = "VAULTD_VARIANT"

// Variant reports which image this binary is running in, or "standalone" when
// it is not running in one of ours.
func Variant() string {
	if variant := strings.TrimSpace(os.Getenv(VariantEnv)); variant != "" {
		return variant
	}
	return "standalone"
}

// clientsFor lists the binaries an engine needs on this host.
func clientsFor(e core.Engine) []string {
	switch e {
	case core.EnginePostgres:
		return postgres.Clients
	case core.EngineMySQL:
		return mysql.Clients(mysql.FlavorMySQL)
	case core.EngineMariaDB:
		return mysql.Clients(mysql.FlavorMariaDB)
	case core.EngineMongoDB:
		return mongodb.Clients
	default:
		return nil
	}
}

// findClients scans for one client of one engine.
func findClients(ctx context.Context, e core.Engine, name string) []engine.Binary {
	switch e {
	case core.EnginePostgres:
		return postgres.FindClients(ctx, name)
	case core.EngineMySQL, core.EngineMariaDB:
		return mysql.FindClients(ctx, name)
	case core.EngineMongoDB:
		return mongodb.FindClients(ctx, name)
	default:
		return nil
	}
}

// checkClients reports which database clients are installed, for every engine
// the config actually uses.
//
// A missing client is fatal rather than a warning: the dump would fail at the
// moment it was most needed, and the whole reason this command exists is to
// find that out now instead (SPEC §3).
func (d *Doctor) checkClients(ctx context.Context, report *Report, targets []*config.Target, cfg *config.Config) {
	engines := map[string]bool{}
	for _, target := range targets {
		engines[string(target.Engine)] = true
	}
	// A verify target restores, so it needs the restore client of its engine
	// even when nothing on this host dumps that engine.
	for i := range cfg.VerifyTargets {
		engines[string(cfg.VerifyTargets[i].Engine)] = true
	}

	report.add(Check{
		Group:  "clients",
		Name:   "image",
		Status: StatusOK,
		Detail: fmt.Sprintf("running the %s build", report.Variant),
	})

	for _, name := range sortedNames(engines) {
		e := core.Engine(name)
		for _, client := range clientsFor(e) {
			report.add(d.withTimeout(ctx, func(ctx context.Context) Check {
				return clientCheck(ctx, e, client, report.Variant)
			}))
		}
	}
}

func clientCheck(ctx context.Context, e core.Engine, client, variant string) Check {
	check := Check{Group: "clients", Name: client}

	found := findClients(ctx, e, client)
	if len(found) == 0 {
		check.Status = StatusFail
		check.Detail = "not installed"
		check.Hint = missingClientHint(e, client, variant)
		return check
	}

	versions := make([]string, 0, len(found))
	for _, binary := range found {
		versions = append(versions, binary.Version)
	}

	check.Status = StatusOK
	check.Detail = fmt.Sprintf("%s (%s)", found[0].Path, strings.Join(versions, ", "))
	return check
}

// missingClientHint says what to install, and — inside one of the slim images
// — that the image itself is the problem (decision D1).
func missingClientHint(e core.Engine, client, variant string) string {
	pkg := map[core.Engine]string{
		core.EnginePostgres: "postgresql-client-<major>",
		core.EngineMySQL:    "mysql-client",
		core.EngineMariaDB:  "mariadb-client",
		core.EngineMongoDB:  "mongodb-database-tools",
	}[e]

	if variant != "standalone" && variant != "latest" {
		return fmt.Sprintf("this config needs %s, which is not in vaultd:%s — use vaultd:latest", client, variant)
	}
	return fmt.Sprintf("install %s, or run the vaultd container image", pkg)
}
