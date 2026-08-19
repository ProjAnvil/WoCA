// Package local is the composition root for the single-binary (personal)
// edition of woca: it assembles every platform interface with its local,
// zero-dependency implementation.
package local

import (
	"errors"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/blobstore"
	"github.com/projanvil/woca/internal/platform/cache"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/runqueue"
)

// Set bundles the local implementations of all platform interfaces.
type Set struct {
	Queue runqueue.Queue
	Bus   eventbus.Bus
	Cache cache.Cache
	Exec  executor.Executor
	Blobs blobstore.Store
	Auth  authn.Provider
}

// New assembles a Set. blobRoot is the root directory of the local blob
// store (e.g. ~/.woca/blobs); all other components are in-memory.
func New(blobRoot string) (*Set, error) {
	blobs, err := blobstore.NewLocal(blobRoot)
	if err != nil {
		return nil, err
	}
	return &Set{
		Queue: runqueue.NewLocal(),
		Bus:   eventbus.NewLocal(),
		Cache: cache.NewLocal(),
		Exec:  executor.NewLocal(),
		Blobs: blobs,
		Auth:  authn.NewLocal(),
	}, nil
}

// Close releases components that hold background resources. Blob store and
// executor are stateless and need no shutdown; auth is stateless too.
func (s *Set) Close() error {
	return errors.Join(s.Queue.Close(), s.Bus.Close(), s.Cache.Close())
}
