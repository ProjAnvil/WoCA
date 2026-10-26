package worker

import (
	"context"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/blobstore"
	"github.com/projanvil/woca/internal/session"
)

// NewFileSessions opens the shared session transcript store (the same
// JSONL-behind-blobstore layout woca-scheduler uses) when dir points at a
// directory shared with the gateways. An empty dir — or a store that cannot
// be opened — disables transcript persistence (prompt-only history): the
// constructor returns nil, which Config.Sessions treats as disabled. Note:
// the listing index (index.json) stays gateway-owned — workers update
// transcripts only, so a gateway's session title/message-count refresh on
// its next own save.
func NewFileSessions(dir string, log zerolog.Logger) Sessions {
	if dir == "" {
		return nil
	}
	blobs, err := blobstore.NewLocal(dir)
	if err != nil {
		log.Warn().Str("dir", dir).Err(err).Msg("session store unavailable; runs execute with prompt-only history")
		return nil
	}
	return &blobSessions{blobs: blobs}
}

// blobSessions implements Sessions over the shared transcript directory.
type blobSessions struct {
	blobs blobstore.Store
}

func (b *blobSessions) Load(id string) []messages.Message {
	msgs, err := session.LoadBlob(context.Background(), b.blobs, id+".jsonl")
	if err != nil {
		return nil
	}
	return msgs
}

func (b *blobSessions) Save(id string, msgs []messages.Message, _ string) error {
	return session.SaveBlob(context.Background(), b.blobs, id+".jsonl", msgs)
}
