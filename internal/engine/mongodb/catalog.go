package mongodb

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/curruwilla/vaultd/internal/core"
)

// systemDatabases are MongoDB's own; mongodump skips them on a full dump and
// so does the collection listing.
var systemDatabases = map[string]bool{"admin": true, "local": true, "config": true}

// deployment is what the probe learns about the server itself.
type deployment struct {
	Version    string
	VersionNum int
	// ReplicaSet is the set name, empty for a standalone server. It decides
	// whether an oplog-consistent dump is even possible.
	ReplicaSet string
}

func (d *Dumper) inspect(ctx context.Context) (deployment, []core.TableInfo, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(d.conn.Raw))
	if err != nil {
		return deployment{}, nil, fmt.Errorf("connecting to %s: %s", d.conn.Hosts, d.failure(err))
	}
	defer func() { _ = client.Disconnect(ctx) }()

	admin := client.Database("admin")

	var build struct {
		Version string `bson:"version"`
	}
	if err := admin.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build); err != nil {
		return deployment{}, nil, fmt.Errorf("connecting to %s: %s", d.conn.Hosts, d.failure(err))
	}

	var hello struct {
		SetName string `bson:"setName"`
	}
	// A standalone server answers hello without a set name; that is the
	// answer, not a failure.
	_ = admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)

	info := deployment{
		Version:    build.Version,
		VersionNum: versionNum(build.Version),
		ReplicaSet: hello.SetName,
	}

	collections, err := d.collections(ctx, client)
	if err != nil {
		return deployment{}, nil, err
	}
	return info, collections, nil
}

// collections lists what will be dumped, as `database.collection`, with counts
// gathered according to the configured strategy.
func (d *Dumper) collections(ctx context.Context, client *mongo.Client) ([]core.TableInfo, error) {
	databases, err := d.databaseNames(ctx, client)
	if err != nil {
		return nil, err
	}

	var out []core.TableInfo
	for _, name := range databases {
		db := client.Database(name)

		names, err := db.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return nil, fmt.Errorf("listing collections of %s: %s", name, d.failure(err))
		}
		sort.Strings(names)

		for _, collection := range names {
			if strings.HasPrefix(collection, "system.") {
				continue
			}

			info := core.TableInfo{Name: name + "." + collection}
			if err := d.count(ctx, db.Collection(collection), &info); err != nil {
				return nil, err
			}
			out = append(out, info)
		}
	}
	return out, nil
}

func (d *Dumper) count(ctx context.Context, collection *mongo.Collection, info *core.TableInfo) error {
	switch d.opts.RowEstimate {
	case RowsOff:
		return nil

	case RowsExact:
		// A real scan: correct, and proportional to the collection size.
		count, err := collection.CountDocuments(ctx, bson.D{})
		if err != nil {
			return fmt.Errorf("counting documents in %s: %s", info.Name, d.failure(err))
		}
		info.Rows = count
		info.RowsExact = true
		return nil

	default:
		// Collection metadata: constant time, and can lag after an unclean
		// shutdown, which is why the manifest records that it is an estimate.
		count, err := collection.EstimatedDocumentCount(ctx)
		if err != nil {
			return fmt.Errorf("counting documents in %s: %s", info.Name, d.failure(err))
		}
		info.Rows = count
		return nil
	}
}

func (d *Dumper) databaseNames(ctx context.Context, client *mongo.Client) ([]string, error) {
	if d.conn.Database != "" {
		return []string{d.conn.Database}, nil
	}

	names, err := client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("listing databases: %s", d.failure(err))
	}

	out := make([]string, 0, len(names))
	for _, name := range names {
		if !systemDatabases[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// oplogHead reads where the replica set's oplog stands right now, as
// "seconds,ordinal" — the same shape MongoDB prints a timestamp in.
func (d *Dumper) oplogHead(ctx context.Context) (string, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(d.conn.Raw))
	if err != nil {
		return "", fmt.Errorf("reading the oplog position: %s", d.failure(err))
	}
	defer func() { _ = client.Disconnect(ctx) }()

	var entry struct {
		TS bson.Timestamp `bson:"ts"`
	}
	// $natural descending is the cheapest way to the newest entry.
	err = client.Database("local").Collection("oplog.rs").
		FindOne(ctx, bson.D{}, options.FindOne().SetSort(bson.D{{Key: "$natural", Value: -1}})).
		Decode(&entry)
	if err != nil {
		return "", fmt.Errorf("reading the oplog position: %s", d.failure(err))
	}

	return fmt.Sprintf("%d,%d", entry.TS.T, entry.TS.I), nil
}

// versionNum renders 7.0.14 as 70014, matching how the other engines report a
// comparable number.
func versionNum(version string) int {
	parts := strings.SplitN(version, ".", 4)

	num := 0
	for i, weight := range []int{10000, 100, 1} {
		if i >= len(parts) {
			break
		}
		// Release candidates and build suffixes ride along on the last
		// component: 6.0.16-rc0.
		value, err := strconv.Atoi(leadingDigits(parts[i]))
		if err != nil {
			break
		}
		num += value * weight
	}
	return num
}

func leadingDigits(s string) string {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return s[:i]
		}
	}
	return s
}

// failure strips credentials from a driver error before it travels anywhere.
func (d *Dumper) failure(err error) string {
	msg := d.conn.redact(err.Error())
	if len(msg) > 300 {
		return msg[:300] + "…"
	}
	return msg
}
