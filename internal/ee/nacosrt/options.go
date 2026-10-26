// BackendOptions: a sandbox backend's execution parameters (design §17
// sandbox params). The console edits them, they ride the pack on the
// sandbox row, and the executor reads them from the materialized table —
// the same six fields at every hop.
package nacosrt

import (
	"encoding/json"
	"fmt"
)

// BackendOptions is the per-backend execution contract. Zero values mean
// "the default": Normalize fills them in so a stored options set is
// always explicit by the time the executor sees it.
type BackendOptions struct {
	Image    string            `json:"image"`
	Runtime  string            `json:"runtime"`
	Network  string            `json:"network"`
	CPU      int               `json:"cpu"`
	MemoryMB int               `json:"memory_mb"`
	Env      map[string]string `json:"env,omitempty"`
}

// Normalize validates the options and fills defaults in place: network
// defaults to "none" and admits only none|bridge, runtime defaults to
// "runc" and admits only runc|runsc, and cpu/memory must not be negative.
// A nil options set is "no options" — a no-op, not an error.
func (o *BackendOptions) Normalize() error {
	if o == nil {
		return nil
	}
	if o.Network == "" {
		o.Network = "none"
	}
	if o.Network != "none" && o.Network != "bridge" {
		return fmt.Errorf("nacosrt: options.network must be none or bridge, got %q", o.Network)
	}
	if o.Runtime == "" {
		o.Runtime = "runc"
	}
	if o.Runtime != "runc" && o.Runtime != "runsc" {
		return fmt.Errorf("nacosrt: options.runtime must be runc or runsc, got %q", o.Runtime)
	}
	if o.CPU < 0 {
		return fmt.Errorf("nacosrt: options.cpu must not be negative, got %d", o.CPU)
	}
	if o.MemoryMB < 0 {
		return fmt.Errorf("nacosrt: options.memory_mb must not be negative, got %d", o.MemoryMB)
	}
	return nil
}

// EncodeOptionsText serializes options for the TEXT storage columns
// (sandbox_backends.options on both the config and materialize side);
// nil encodes as "".
func EncodeOptionsText(o *BackendOptions) (string, error) {
	if o == nil {
		return "", nil
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("nacosrt: options encode: %w", err)
	}
	return string(raw), nil
}

// DecodeOptionsText parses a stored options column; "" decodes to nil.
// Invalid text is an error, not an empty options set — the column is
// written only through validated paths.
func DecodeOptionsText(s string) (*BackendOptions, error) {
	if s == "" {
		return nil, nil
	}
	var o BackendOptions
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		return nil, fmt.Errorf("nacosrt: options decode: %w", err)
	}
	return &o, nil
}
