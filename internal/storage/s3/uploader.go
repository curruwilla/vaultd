package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sync/errgroup"

	"github.com/curruwilla/vaultd/internal/core"
)

// Multipart tuning (SPEC §4).
//
// The size of a dump is unknown when the upload starts — that is the price of
// streaming — so the part size adapts: it starts small and doubles every
// partsPerGrowth parts, which keeps small backups cheap while still fitting a
// large one inside the 10,000-part protocol limit.
//
// maxPartSize and maxConcurrency together bound the memory this stage holds:
// at most maxConcurrency parts are in flight, so the ceiling is 128MB, leaving
// room under the 256MB target (SPEC O2) for the compressor. Raising
// maxPartSize raises both the object ceiling and the resident memory.
const (
	minPartSize     = 5 << 20 // the S3 minimum for a non-final part
	initialPartSize = 8 << 20
	maxPartSize     = 32 << 20
	partsPerGrowth  = 1000
	maxParts        = 10000
	maxConcurrency  = 4

	// metadataSHA256 carries our own checksum of the stored bytes, so a HEAD
	// can answer an L0 integrity check without a manifest lookup.
	metadataSHA256 = "vaultd-sha256"
)

// upload writes r to key, choosing between a single PUT and a multipart upload
// by how much data actually turns up.
func (s *Store) upload(ctx context.Context, key string, r io.Reader, opt core.PutOptions) (core.ObjectInfo, error) {
	partSize := clampPartSize(int(opt.PartSize))
	concurrency := opt.Concurrency
	if concurrency < 1 || concurrency > maxConcurrency {
		concurrency = maxConcurrency
	}

	first, last, err := readPart(r, partSize)
	if err != nil {
		return core.ObjectInfo{}, fmt.Errorf("reading the backup stream: %w", err)
	}

	// A dump that fits in one part — a small database, or a failure that
	// produced almost nothing — never needs the multipart dance.
	if last {
		return s.putSingle(ctx, key, first, opt)
	}
	return s.putMultipart(ctx, key, r, first, partSize, concurrency, opt)
}

func (s *Store) putSingle(ctx context.Context, key string, data []byte, opt core.PutOptions) (core.ObjectInfo, error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   contentType(opt),
		StorageClass:  s.storageClass,
		Metadata:      opt.Metadata,
	})
	if err != nil {
		return core.ObjectInfo{}, s.wrap("putting", key, err)
	}
	return core.ObjectInfo{Key: key, Bytes: int64(len(data)), ETag: unquote(out.ETag)}, nil
}

func (s *Store) putMultipart(
	ctx context.Context,
	key string,
	r io.Reader,
	first []byte,
	partSize, concurrency int,
	opt core.PutOptions,
) (core.ObjectInfo, error) {
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		ContentType:  contentType(opt),
		StorageClass: s.storageClass,
		Metadata:     opt.Metadata,
	})
	if err != nil {
		return core.ObjectInfo{}, s.wrap("starting a multipart upload for", key, err)
	}
	uploadID := aws.ToString(created.UploadId)

	completed := false
	defer func() {
		if completed {
			return
		}
		// Abandoned parts keep costing storage until a lifecycle rule sweeps
		// them, so the abort runs even when ctx is already cancelled — that is
		// precisely the case that leaves garbage behind.
		s.abort(context.WithoutCancel(ctx), key, uploadID)
	}()

	var (
		mu    sync.Mutex
		parts []types.CompletedPart
	)

	g, gctx := errgroup.WithContext(ctx)
	// The limit is the backpressure that keeps memory bounded: dispatching a
	// part blocks until a worker is free, so at most `concurrency` buffers
	// exist at once.
	g.SetLimit(concurrency)

	dispatch := func(number int32, data []byte) {
		g.Go(func() error {
			out, err := s.client.UploadPart(gctx, &s3.UploadPartInput{
				Bucket:        aws.String(s.bucket),
				Key:           aws.String(key),
				UploadId:      aws.String(uploadID),
				PartNumber:    aws.Int32(number),
				Body:          bytes.NewReader(data),
				ContentLength: aws.Int64(int64(len(data))),
			})
			if err != nil {
				return fmt.Errorf("uploading part %d of %q: %w", number, key, err)
			}

			mu.Lock()
			defer mu.Unlock()
			parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(number)})
			return nil
		})
	}

	var (
		number   int32 = 1
		uploaded int64
		data     = first
		readErr  error
	)

	for {
		dispatch(number, data)
		uploaded += int64(len(data))

		// A part has failed: stop feeding the pipeline instead of reading the
		// rest of a dump that is no longer going anywhere.
		if gctx.Err() != nil {
			break
		}

		var last bool
		number++
		if number > maxParts {
			readErr = fmt.Errorf("backup exceeds the multipart limit of %d parts of up to %s", maxParts, humanBytes(maxPartSize))
			break
		}
		if (number-1)%partsPerGrowth == 0 {
			partSize = min(partSize*2, maxPartSize)
		}

		data, last, readErr = readPart(r, partSize)
		if readErr != nil {
			readErr = fmt.Errorf("reading the backup stream: %w", readErr)
			break
		}
		if len(data) == 0 {
			break // the previous part ended exactly on a boundary
		}
		if last {
			dispatch(number, data)
			uploaded += int64(len(data))
			break
		}
	}

	if err := g.Wait(); err != nil {
		return core.ObjectInfo{}, err
	}
	if readErr != nil {
		return core.ObjectInfo{}, readErr
	}

	sort.Slice(parts, func(i, j int) bool { return *parts[i].PartNumber < *parts[j].PartNumber })

	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return core.ObjectInfo{}, s.wrap("completing the multipart upload of", key, err)
	}
	completed = true

	return core.ObjectInfo{Key: key, Bytes: uploaded, ETag: unquote(out.ETag)}, nil
}

func (s *Store) abort(ctx context.Context, key, uploadID string) {
	_, _ = s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
}

// readPart fills one part. It reports last=true when the stream ended, which
// includes the case of a short final part.
func readPart(r io.Reader, size int) (data []byte, last bool, err error) {
	buf := make([]byte, size)

	n, err := io.ReadFull(r, buf)
	switch {
	case errors.Is(err, io.EOF):
		return nil, true, nil
	case errors.Is(err, io.ErrUnexpectedEOF):
		return buf[:n], true, nil
	case err != nil:
		return nil, false, err
	default:
		return buf, false, nil
	}
}

func clampPartSize(size int) int {
	if size <= 0 {
		return initialPartSize
	}
	return min(max(size, minPartSize), maxPartSize)
}

func contentType(opt core.PutOptions) *string {
	if opt.ContentType == "" {
		return nil
	}
	return aws.String(opt.ContentType)
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%dGB", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%dMB", n>>20)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
