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
	case core.EngineMongoDB:
		return mongodb.FindClients(ctx, name)
	default:
		return nil
	}
}

// flavorOf is the fork a MySQL-family engine needs its client to belong to.
func flavorOf(e core.Engine) mysql.Flavor {
	if e == core.EngineMariaDB {
		return mysql.FlavorMariaDB
	}
	return mysql.FlavorMySQL
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
	if e == core.EngineMySQL || e == core.EngineMariaDB {
		return mysqlClientCheck(ctx, e, client, variant)
	}

	check := Check{Group: "clients", Name: client}

	found := findClients(ctx, e, client)
	if len(found) == 0 {
		check.Status = StatusFail
		check.Detail = "not installed"
		check.Hint = missingClientHint(e, client, variant)
		return check
	}

	check.Status = StatusOK
	check.Detail = fmt.Sprintf("%s (%s)", found[0].Path, strings.Join(versionsOf(found), ", "))
	return check
}

// slim reports whether this build carries only one engine, and so cannot serve
// a config that names another. `all` is the fat image under its build-arg
// name; `latest` is the same image under its tag.
func slim(variant string) bool {
	switch variant {
	case "standalone", "latest", "all", "":
		return false
	default:
		return true
	}
}

// versionsOf lists the distinct versions found, in the order they were found.
// A distribution wrapper on PATH and the versioned binary it dispatches to are
// two paths to one client, and printing "18.6, 18.6" reads as a bug.
func versionsOf(found []engine.Binary) []string {
	var (
		versions []string
		seen     = map[string]bool{}
	)
	for _, binary := range found {
		if seen[binary.Version] {
			continue
		}
		seen[binary.Version] = true
		versions = append(versions, binary.Version)
	}
	return versions
}

// mysqlClientCheck answers the question that actually matters for the MySQL
// family: not "is there a binary with this name", but "does it belong to the
// fork this target needs".
//
// MariaDB installs `mysqldump` as a symlink onto its own `mariadb-dump`, so a
// host can have the name and not the client. vaultd refuses that at dump time
// — the flags and the output diverge — and doctor exists so the refusal is not
// a surprise at three in the morning.
func mysqlClientCheck(ctx context.Context, e core.Engine, client, variant string) Check {
	check := Check{Group: "clients", Name: client}
	wanted := flavorOf(e)

	found := mysql.FindClients(ctx, client)
	if len(found) == 0 {
		check.Status = StatusFail
		check.Detail = "not installed"
		check.Hint = missingClientHint(e, client, variant)
		return check
	}

	var wrong []string
	for _, candidate := range found {
		if candidate.Flavor == wanted {
			check.Status = StatusOK
			check.Detail = fmt.Sprintf("%s (%s %s)", candidate.Path, candidate.Flavor, candidate.Version)
			return check
		}
		wrong = append(wrong, fmt.Sprintf("%s is %s's (%s)", candidate.Path, candidate.Flavor, candidate.Version))
	}

	check.Status = StatusFail
	check.Detail = strings.Join(wrong, "; ")
	check.Hint = fmt.Sprintf(
		"a %s target needs %s's own client; vaultd refuses the other fork rather than dumping something a restore cannot read",
		e, wanted)
	if slim(variant) {
		check.Hint += fmt.Sprintf(" — vaultd:%s does not carry it, use vaultd:latest", variant)
	}
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

	if slim(variant) {
		return fmt.Sprintf("this config needs %s, which is not in vaultd:%s — use vaultd:latest", client, variant)
	}
	return fmt.Sprintf("install %s, or run the vaultd container image", pkg)
}
