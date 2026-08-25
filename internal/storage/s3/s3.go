// Package s3 implements core.Store on top of any S3-compatible object store:
// AWS S3, Cloudflare R2, MinIO, or GCS in interop mode.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/curruwilla/vaultd/internal/core"
)

// Provider names the flavour of S3 being talked to. It only changes defaults,
// never the protocol.
type Provider string

const (
	ProviderS3         Provider = "s3"
	ProviderR2         Provider = "r2"
	ProviderMinIO      Provider = "minio"
	ProviderGCSInterop Provider = "gcs-interop"
)

// Config is everything needed to reach one bucket.
type Config struct {
	Provider        Provider
	Bucket          string
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
	StorageClass    string
}

// Store is a bucket. It is safe for concurrent use.
type Store struct {
	client       *s3.Client
	bucket       string
	storageClass types.StorageClass
}

// New builds a Store. It performs no network call: reaching the bucket is
// `vaultd doctor`'s job, and a constructor that dials would make every command
// fail slowly when the network is down.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("no bucket configured")
	}

	region := cfg.Region
	if region == "" && cfg.Provider == ProviderR2 {
		region = "auto"
	}

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
		// R2 (and MinIO, historically) reject the SDK's default CRC32 trailer
		// on streaming uploads, so checksums are only sent where the API
		// actually requires them (SPEC §6). Integrity is covered by our own
		// SHA-256 of the ciphertext, recorded in the manifest.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if cfg.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}

	pathStyle := cfg.ForcePathStyle || cfg.Provider == ProviderMinIO
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = pathStyle
	})

	return &Store{
		client:       client,
		bucket:       cfg.Bucket,
		storageClass: types.StorageClass(cfg.StorageClass),
	}, nil
}

// CreateBucket creates the bucket if it is not there yet. vaultd does not
// create buckets during a backup — a typo in a bucket name must fail rather
// than quietly start a new one — but tests and first-time setup need it.
func (s *Store) CreateBucket(ctx context.Context) error {
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}

	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return nil
	}
	return fmt.Errorf("creating bucket %s: %w", s.bucket, err)
}

// Put streams r to key. See uploader.go for the multipart strategy.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, opt core.PutOptions) (core.ObjectInfo, error) {
	return s.upload(ctx, key, r, opt)
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, s.wrap("getting", key, err)
	}
	return out.Body, nil
}

func (s *Store) Head(ctx context.Context, key string) (core.ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return core.ObjectInfo{}, s.wrap("heading", key, err)
	}

	info := core.ObjectInfo{Key: key, ETag: unquote(out.ETag)}
	if out.ContentLength != nil {
		info.Bytes = *out.ContentLength
	}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	info.SHA256 = out.Metadata[metadataSHA256]
	return info, nil
}

// List walks every object under prefix, newest pages first as the API returns
// them. The iterator yields an error once and stops: a partial listing must
// never be mistaken for a complete one, which is what makes retention safe.
func (s *Store) List(ctx context.Context, prefix string) iter.Seq2[core.ObjectInfo, error] {
	return func(yield func(core.ObjectInfo, error) bool) {
		paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
			Bucket: aws.String(s.bucket),
			Prefix: aws.String(prefix),
		})

		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				yield(core.ObjectInfo{}, s.wrap("listing", prefix, err))
				return
			}
			for _, obj := range page.Contents {
				info := core.ObjectInfo{Key: aws.ToString(obj.Key), ETag: unquote(obj.ETag)}
				if obj.Size != nil {
					info.Bytes = *obj.Size
				}
				if obj.LastModified != nil {
					info.LastModified = *obj.LastModified
				}
				if !yield(info, nil) {
					return
				}
			}
		}
	}
}

// Delete removes objects in batches. It is the only write path that destroys
// data, so it reports which key failed rather than a bare count.
func (s *Store) Delete(ctx context.Context, keys []string) error {
	const batchSize = 1000

	for start := 0; start < len(keys); start += batchSize {
		end := min(start+batchSize, len(keys))

		ids := make([]types.ObjectIdentifier, 0, end-start)
		for _, key := range keys[start:end] {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(key)})
		}

		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("deleting objects: %w", err)
		}
		if len(out.Errors) > 0 {
			first := out.Errors[0]
			return fmt.Errorf("deleting %s: %s (%d of %d objects failed)",
				aws.ToString(first.Key), aws.ToString(first.Message), len(out.Errors), end-start)
		}
	}
	return nil
}

// PutIfAbsent writes key only if nothing is there yet, using a conditional
// write. It is the primitive the target lock is built on: two daemon replicas
// racing for the same target, and only one wins.
func (s *Store) PutIfAbsent(ctx context.Context, key string, b []byte) (bool, error) {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(string(b)),
		IfNoneMatch: aws.String("*"),
	})
	if err == nil {
		return true, nil
	}
	if isPreconditionFailed(err) {
		return false, nil
	}
	return false, s.wrap("creating", key, err)
}

// PutIfMatch overwrites key only if its ETag still matches, which is what
// makes a read-modify-write of the index safe against a concurrent writer.
func (s *Store) PutIfMatch(ctx context.Context, key string, b []byte, etag string) (core.ObjectInfo, bool, error) {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(b),
		ContentType: aws.String("application/x-ndjson"),
	}
	if etag == "" {
		// No ETag means "it should not exist yet".
		input.IfNoneMatch = aws.String("*")
	} else {
		input.IfMatch = aws.String(quote(etag))
	}

	out, err := s.client.PutObject(ctx, input)
	switch {
	case err == nil:
		return core.ObjectInfo{Key: key, Bytes: int64(len(b)), ETag: unquote(out.ETag)}, true, nil
	case isPreconditionFailed(err), isNotFound(err):
		// Someone else wrote first, or the object vanished: the caller re-reads
		// and tries again rather than overwriting what it has not seen.
		return core.ObjectInfo{}, false, nil
	default:
		return core.ObjectInfo{}, false, s.wrap("updating", key, err)
	}
}

// wrap turns a provider error into one a caller can reason about: not-found is
// a condition vaultd handles, everything else is a failure to report.
func (s *Store) wrap(action, key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%s %q: %w", action, key, core.ErrNotFound)
	}
	return fmt.Errorf("%s %q in bucket %s: %w", action, key, s.bucket, err)
}

func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}
	return statusCode(err) == http.StatusNotFound
}

func isPreconditionFailed(err error) bool {
	if statusCode(err) == http.StatusPreconditionFailed {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "PreconditionFailed"
	}
	return false
}

func statusCode(err error) int {
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode()
	}
	return 0
}

func unquote(etag *string) string { return strings.Trim(aws.ToString(etag), `"`) }

func quote(etag string) string {
	if strings.HasPrefix(etag, `"`) {
		return etag
	}
	return `"` + etag + `"`
}

var _ core.Store = (*Store)(nil)
