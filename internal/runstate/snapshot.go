// snapshot.go defines the run dispatch contract between the gateway and
// the worker (design §5.3/§13): when a gateway instance dispatches a run to
// the shared queue instead of executing it in-process, the prompt and its
// execution environment ride the runs.config_snapshot column as this JSON
// blob. Both sides import this package, so the schema lives next to the
// queue it travels over — not in either role.
package runstate

import "encoding/json"

// Attachment is one image attached to a dispatched prompt (base64 body).
type Attachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

// RunSnapshot is the serialized execution request of a dispatched run.
// Workspace and SessionID are the execution environment the worker restores
// (per-run workspace scoping and transcript persistence); ThreadID pins the
// checkpoint thread the gateway minted so resumed sessions keep their state.
type RunSnapshot struct {
	Prompt      string       `json:"prompt"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Workspace   string       `json:"workspace,omitempty"`
	SessionID   string       `json:"session_id,omitempty"`
	ThreadID    string       `json:"thread_id,omitempty"`
}

// EncodeSnapshot serializes snap for the runs.config_snapshot column.
func EncodeSnapshot(snap RunSnapshot) (string, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseSnapshot decodes a config snapshot blob. An empty blob yields an
// empty snapshot (runs re-queued by the reconciler before dispatch existed,
// or state-cleanup claims).
func ParseSnapshot(blob string) (RunSnapshot, error) {
	if blob == "" {
		return RunSnapshot{}, nil
	}
	var snap RunSnapshot
	if err := json.Unmarshal([]byte(blob), &snap); err != nil {
		return RunSnapshot{}, err
	}
	return snap, nil
}
