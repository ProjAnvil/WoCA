package server

import (
	"context"
	"errors"
	"testing"

	"github.com/projanvil/woca/internal/platform/blobstore"
)

// ctxRecordingBlobs is a blobstore.Store fake that captures the context each
// call receives, so tests can assert the caller's request context reaches the
// blob IO path instead of being dropped for context.Background().
type ctxRecordingBlobs struct {
	getCtx context.Context
}

func (c *ctxRecordingBlobs) Put(context.Context, string, []byte) error { return nil }

func (c *ctxRecordingBlobs) Get(ctx context.Context, _ string) ([]byte, error) {
	c.getCtx = ctx
	return nil, blobstore.ErrNotFound
}

func (c *ctxRecordingBlobs) Delete(context.Context, string) error { return nil }

func (c *ctxRecordingBlobs) List(context.Context, string) ([]string, error) { return nil, nil }

type callerCtxKey struct{}

// The blob load path must honor the caller's context: an already-cancelled
// request context must reach the blob Get (cancellation and values alike),
// not be replaced with context.Background().
func TestLoadBlobHonorsCallerContext(t *testing.T) {
	s := newSessionStore("")
	blobs := &ctxRecordingBlobs{}
	s.blobs = blobs
	s.dir = t.TempDir()

	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), callerCtxKey{}, "caller"))
	cancel()
	if got := s.load(ctx, "sess-x"); got != nil {
		t.Fatalf("load = %v, want nil for a missing blob", got)
	}
	if blobs.getCtx == nil {
		t.Fatal("blob store never saw a Get call")
	}
	if v := blobs.getCtx.Value(callerCtxKey{}); v != "caller" {
		t.Fatalf("blob Get ctx lost the caller context (value=%v)", v)
	}
	if err := blobs.getCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("blob Get ctx not cancelled with the caller (err=%v)", err)
	}
}
