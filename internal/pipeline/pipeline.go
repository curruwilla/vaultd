// Package pipeline streams a dump from a database to object storage without
// ever holding it in memory or on disk: dump → compress → encrypt → upload,
// with the plaintext and the ciphertext hashed on the way past (SPEC §4).
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"

	"golang.org/x/sync/errgroup"
)

// Produce writes the raw dump. It is core.Dumper.Dump, wrapped by the caller.
type Produce func(ctx context.Context, w io.Writer) error

// Consume reads the finished object. It is the upload.
type Consume func(ctx context.Context, r io.Reader) error

// Result reports what went through the pipeline. Both checksums matter and
// they answer different questions: the ciphertext sum proves the object in the
// bucket is intact, the plaintext sum proves a restore produced what the
// database originally handed over (SPEC §5).
type Result struct {
	Plaintext  Sum
	Ciphertext Sum
}

// Run drives one backup stream to completion.
//
// The two halves run concurrently over an io.Pipe: whichever fails first
// closes the pipe with its error, which unblocks the other half instead of
// deadlocking it, and cancels the shared context so a running pg_dump or a
// half-finished multipart upload is torn down rather than left behind.
func Run(ctx context.Context, spec Spec, produce Produce, consume Consume) (Result, error) {
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}

	var result Result
	pr, pw := io.Pipe()

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		plaintext := newSum()

		w, closeStages, err := spec.writer(pw)
		if err != nil {
			// Nothing has been written yet; fail the read side too.
			pw.CloseWithError(err)
			return err
		}

		err = produce(ctx, io.MultiWriter(w, plaintext))
		if err == nil {
			// Flushing the compressor and finalizing the age header are part
			// of producing a valid object: a failure here is a failed backup,
			// not a cleanup detail.
			err = closeStages()
		}
		if err != nil {
			pw.CloseWithError(err)
			return err
		}

		result.Plaintext = plaintext.Sum()
		return pw.Close()
	})

	g.Go(func() error {
		ciphertext := newSum()
		// Closing the read side makes a still-writing producer fail fast
		// rather than block on a reader that has gone away.
		defer pr.Close()

		if err := consume(ctx, io.TeeReader(pr, ciphertext)); err != nil {
			_ = pr.CloseWithError(err)
			return err
		}

		// A consumer that stopped early would leave the producer blocked and
		// the object truncated; drain to be sure everything was read.
		if _, err := io.Copy(io.Discard, io.TeeReader(pr, ciphertext)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return fmt.Errorf("draining the pipeline: %w", err)
		}

		result.Ciphertext = ciphertext.Sum()
		return nil
	})

	if err := g.Wait(); err != nil {
		return Result{}, err
	}
	return result, nil
}
