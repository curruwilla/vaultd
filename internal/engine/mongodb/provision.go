package mongodb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/curruwilla/vaultd/internal/core"
)

// ProvisionOptions configures the staging deployment restore verification uses
// (SPEC §8, decision D3).
type ProvisionOptions struct {
	// URI is the administrative connection to the staging deployment.
	URI string
	// Prefix is the one namespace this provisioner may create and drop in.
	Prefix                 string
	BinDir                 string
	NumParallelCollections int
	ProbeTimeout           time.Duration
}

// Provisioner creates and drops ephemeral databases on a MongoDB verify
// target.
type Provisioner struct {
	opts ProvisionOptions
	conn connInfo
}

// NewProvisioner parses the administrative URI. It opens no connection.
func NewProvisioner(opts ProvisionOptions) (*Provisioner, error) {
	if strings.TrimSpace(opts.Prefix) == "" {
		return nil, errors.New("verify target: no database_prefix; vaultd refuses to create or drop databases without one")
	}
	conn, err := parseURI(opts.URI)
	if err != nil {
		return nil, fmt.Errorf("verify target: %w", err)
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 30 * time.Second
	}
	return &Provisioner{opts: opts, conn: conn}, nil
}

// Probe reads the staging deployment's version.
func (p *Provisioner) Probe(ctx context.Context) (core.ServerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	client, err := p.connect(ctx)
	if err != nil {
		return core.ServerInfo{}, err
	}
	defer func() { _ = client.Disconnect(ctx) }()

	var build struct {
		Version string `bson:"version"`
	}
	err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build)
	if err != nil {
		return core.ServerInfo{}, fmt.Errorf("reading the verify target's version: %s", p.failure(err))
	}

	return core.ServerInfo{
		Engine:     core.EngineMongoDB,
		Version:    build.Version,
		VersionNum: versionNum(build.Version),
	}, nil
}

// Create makes an empty database and returns it as a sandbox.
//
// A MongoDB archive carries the database it was dumped from, so restoring it
// somewhere else is a namespace rename. That only has one answer when the
// backup holds a single database: a dump of a whole deployment would have to
// land on the staging server under the names it already uses, which is exactly
// what the prefix guard exists to prevent.
func (p *Provisioner) Create(ctx context.Context, spec core.SandboxSpec) (core.Sandbox, error) {
	if err := p.guard(spec.Name); err != nil {
		return nil, err
	}

	source, err := sourceDatabase(spec.Tables)
	if err != nil {
		return nil, err
	}

	created, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	client, err := p.connect(created)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Disconnect(created) }()

	// MongoDB creates a database by writing to it, so there is nothing to
	// create here — only something to refuse. A name that already holds
	// collections is a leftover from a crashed run, and silently restoring
	// into it would mix two backups together.
	collections, err := client.Database(spec.Name).ListCollectionNames(created, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("inspecting the verify database %s: %s", spec.Name, p.failure(err))
	}
	if len(collections) > 0 {
		return nil, fmt.Errorf(
			"the verify database %s already holds %d collections; run `vaultd verify --gc` to collect what a crashed run left behind",
			spec.Name, len(collections))
	}

	conn, err := p.conn.withDatabase(spec.Name)
	if err != nil {
		return nil, err
	}
	// mongorestore is pointed at the deployment rather than at a database:
	// the destination is the --nsTo rename, and naming a database as well
	// would be a second, conflicting answer to the same question.
	deployment, err := p.conn.withDatabase("")
	if err != nil {
		return nil, err
	}

	restorer, err := NewRestorer(RestoreOptions{
		URI:                    deployment.Raw,
		BinDir:                 p.opts.BinDir,
		NumParallelCollections: p.opts.NumParallelCollections,
		ProbeTimeout:           p.opts.ProbeTimeout,
		NSFrom:                 source + ".*",
		NSTo:                   spec.Name + ".*",
	})
	if err != nil {
		return nil, err
	}

	return &sandbox{
		Restorer:    restorer,
		provisioner: p,
		name:        spec.Name,
		source:      source,
		conn:        conn,
	}, nil
}

// List names the sandbox databases the deployment holds right now.
func (p *Provisioner) List(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	client, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Disconnect(ctx) }()

	names, err := client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("listing the verify databases: %s", p.failure(err))
	}

	var out []string
	for _, name := range names {
		if strings.HasPrefix(name, p.opts.Prefix) && name != p.opts.Prefix {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Drop removes one sandbox database. It is idempotent, and it refuses any name
// outside the configured prefix.
func (p *Provisioner) Drop(ctx context.Context, name string) error {
	if err := p.guard(name); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
	defer cancel()

	client, err := p.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(ctx) }()

	if err := client.Database(name).Drop(ctx); err != nil {
		return fmt.Errorf("dropping the verify database %s: %s", name, p.failure(err))
	}
	return nil
}

// guard is the whole safety story of a verify target: vaultd creates and drops
// databases on a live deployment, and it only ever touches names carrying the
// prefix the config declared (SPEC §8).
func (p *Provisioner) guard(name string) error {
	switch {
	case !strings.HasPrefix(name, p.opts.Prefix), name == p.opts.Prefix:
		return fmt.Errorf(
			"refusing to touch database %q: a verify target only ever creates and drops databases named %s…",
			name, p.opts.Prefix)
	case systemDatabases[name]:
		return fmt.Errorf("refusing to touch database %q: it is one of MongoDB's own", name)
	}
	return nil
}

func (p *Provisioner) connect(ctx context.Context) (*mongo.Client, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(p.conn.Raw))
	if err != nil {
		return nil, fmt.Errorf("connecting to the verify target %s: %s", p.conn.Hosts, p.failure(err))
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("connecting to the verify target %s: %s", p.conn.Hosts, p.failure(err))
	}
	return client, nil
}

func (p *Provisioner) failure(err error) string {
	msg := p.conn.redact(err.Error())
	if len(msg) > 300 {
		return msg[:300] + "…"
	}
	return msg
}

// sourceDatabase reads the database a backup came from out of the manifest's
// collection names, which are `database.collection`.
func sourceDatabase(tables []core.TableInfo) (string, error) {
	seen := map[string]bool{}
	for _, table := range tables {
		database, _, found := strings.Cut(table.Name, ".")
		if found && database != "" {
			seen[database] = true
		}
	}

	switch len(seen) {
	case 1:
		for database := range seen {
			return database, nil
		}
		return "", nil // unreachable: the map holds exactly one key

	case 0:
		return "", fmt.Errorf(
			"%w: the manifest lists no collections, so there is no namespace to restore into", core.ErrSandboxUnsupported)

	default:
		names := make([]string, 0, len(seen))
		for database := range seen {
			names = append(names, database)
		}
		sort.Strings(names)
		return "", fmt.Errorf(
			"%w: this backup holds %d databases (%s); restore verification renames one database into the ephemeral one, so point the target's uri at a single database",
			core.ErrSandboxUnsupported, len(names), strings.Join(names, ", "))
	}
}

// sandbox is one ephemeral database: the restorer that renames the archive
// into it, plus the queries the assertions read it back with.
type sandbox struct {
	*Restorer
	provisioner *Provisioner
	name        string
	// source is the database the archive was dumped from, which is what the
	// manifest's collection names are qualified with.
	source string
	conn   connInfo
}

func (s *sandbox) Name() string { return s.name }

func (s *sandbox) Drop(ctx context.Context) error { return s.provisioner.Drop(ctx, s.name) }

// IsEmpty reports on the sandbox database alone. The embedded restorer is
// pointed at the whole deployment — that is where the rename lands — and would
// otherwise answer for every database the staging server holds.
func (s *sandbox) IsEmpty(ctx context.Context) (bool, error) {
	client, err := s.provisioner.connect(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = client.Disconnect(ctx) }()

	collections, err := client.Database(s.name).ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return false, fmt.Errorf("inspecting the verify database %s: %s", s.name, s.provisioner.failure(err))
	}
	return len(collections) == 0, nil
}

func (s *sandbox) Tables(ctx context.Context) ([]string, error) {
	client, err := s.provisioner.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Disconnect(ctx) }()

	names, err := client.Database(s.name).ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("listing the restored collections: %s", s.provisioner.failure(err))
	}

	out := make([]string, 0, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, "system.") {
			out = append(out, s.name+"."+name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// CountRows counts one restored collection. The manifest names it
// `database.collection` against the database the backup came from; here it
// lives under the sandbox's name.
func (s *sandbox) CountRows(ctx context.Context, table string) (int64, error) {
	client, err := s.provisioner.connect(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = client.Disconnect(ctx) }()

	collection := strings.TrimPrefix(table, s.source+".")

	count, err := client.Database(s.name).Collection(collection).CountDocuments(ctx, bson.D{})
	if err != nil {
		return 0, fmt.Errorf("counting documents in %s: %s", table, s.provisioner.failure(err))
	}
	return count, nil
}

// Scalar has nothing to run: a `query` assertion is SQL, and validation
// refuses one on a MongoDB target before anything reaches this point.
func (s *sandbox) Scalar(context.Context, string) (any, error) {
	return nil, core.ErrQueryUnsupported
}

var (
	_ core.Provisioner = (*Provisioner)(nil)
	_ core.Sandbox     = (*sandbox)(nil)
)
