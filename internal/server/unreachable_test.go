package server_test

import (
	"context"
	"errors"
	"io"
	"iter"

	"github.com/curruwilla/vaultd/internal/core"
)

// unreachable is a store that answers nothing, standing in for a bucket whose
// credentials, endpoint or network have gone away.
type unreachable struct{}

var errUnreachable = errors.New("dial tcp: no route to host")

func (unreachable) Put(context.Context, string, io.Reader, core.PutOptions) (core.ObjectInfo, error) {
	return core.ObjectInfo{}, errUnreachable
}

func (unreachable) Get(context.Context, string) (io.ReadCloser, error) { return nil, errUnreachable }

func (unreachable) Head(context.Context, string) (core.ObjectInfo, error) {
	return core.ObjectInfo{}, errUnreachable
}

func (unreachable) List(context.Context, string) iter.Seq2[core.ObjectInfo, error] {
	return func(yield func(core.ObjectInfo, error) bool) { yield(core.ObjectInfo{}, errUnreachable) }
}

func (unreachable) Delete(context.Context, []string) error { return errUnreachable }

func (unreachable) PutIfAbsent(context.Context, string, []byte) (bool, error) {
	return false, errUnreachable
}

func (unreachable) PutIfMatch(context.Context, string, []byte, string) (core.ObjectInfo, bool, error) {
	return core.ObjectInfo{}, false, errUnreachable
}

var _ core.Store = unreachable{}
