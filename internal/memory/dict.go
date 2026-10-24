package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	ladymstorage "github.com/ProjAnvil/LadyM/storage"
)

// CJKDictStatus is the panel/API view of the CJK dictionary state. The
// embedded engine's tokenizer consults the dictionary for Han/Kana
// segmentation of tokens (which reach the embedding, recall's coverage
// scoring, and per-memory statistics); a missing dictionary degrades to
// per-character tokens.
type CJKDictStatus struct {
	Available bool   `json:"available"`
	Source    string `json:"source"`  // file | embedded | none
	Variant   string `json:"variant"` // active variant, "" when none
	Dir       string `json:"dir"`
	Bytes     int64  `json:"bytes"` // on-disk size when source == file
}

// DictManager is the CJK dictionary capability of the embedded engine.
// Downloading is the one network touch and stays an explicit user action —
// never a startup path or background loop (LadyM's own download contract:
// sha256-verified files, atomic install, immediate reload). The remote
// data-plane does not carry the capability; enterprise deployments manage
// the dictionary on the LadyM service itself.
type DictManager interface {
	CJKDictStatus() CJKDictStatus
	CJKDictDownload(ctx context.Context) (CJKDictStatus, error)
}

// DefaultDictDir returns ~/.woca/dict (empty on home-dir failure). The
// dictionary lives under woca's home; LadyM's own default (~/.ladyM/dict)
// is another application's home.
func DefaultDictDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca", "dict")
}

// CJKDictStatus implements DictManager: a read over LadyM's global
// tokenizer state (the dict dir was routed at Build time).
func (l *LadyM) CJKDictStatus() CJKDictStatus {
	st := ladymstorage.CJKDictStatusNow()
	return CJKDictStatus{
		Available: st.Available, Source: st.Source, Variant: string(st.Variant),
		Dir: st.Dir, Bytes: st.Bytes,
	}
}

// CJKDictDownload implements DictManager: fetch, verify, install, reload —
// one blocking call (~9MB over CDN mirrors). Failure leaves the previously
// installed dictionary coherent (downloads complete before anything is
// written into place).
func (l *LadyM) CJKDictDownload(_ context.Context) (CJKDictStatus, error) {
	st, err := ladymstorage.DownloadCJKDictTo("", l.dictDir, "")
	if err != nil {
		return CJKDictStatus{}, fmt.Errorf("memory: cjk dict download: %w", err)
	}
	return CJKDictStatus{
		Available: st.Available, Source: st.Source, Variant: string(st.Variant),
		Dir: st.Dir, Bytes: st.Bytes,
	}, nil
}
