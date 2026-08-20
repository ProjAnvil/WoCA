// migrate.go is the Phase 2 personal→enterprise memory migration path
// (design §6.3.8): export paginates one or more workspaces of any Engine
// (the personal embedded ladyM) into line-delimited JSON; import replays the
// records into a target Engine (the remote ladyM enterprise client) under a
// chosen workspace key. The remote attention gate may drop duplicates —
// drops are counted, not errors.
package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// ExportRecord is one migrated memory on the wire.
type ExportRecord struct {
	Workspace string   `json:"workspace"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	Source    string   `json:"source"`
	Layer     string   `json:"layer"`
	Type      string   `json:"type"`
	UpdatedAt float64  `json:"updated_at"`
}

const exportPageSize = 200

// ExportEngine writes every memory of the given workspaces as JSONL and
// returns the record count.
func ExportEngine(ctx context.Context, eng Engine, workspaces []string, w io.Writer) (int, error) {
	bw := bufio.NewWriter(w)
	total := 0
	for _, ws := range workspaces {
		for offset := 0; ; offset += exportPageSize {
			hits, err := eng.List(ctx, ws, "", exportPageSize, offset)
			if err != nil {
				return total, fmt.Errorf("memory export: %w", err)
			}
			for _, h := range hits {
				rec := ExportRecord{
					Workspace: ws,
					Content:   h.Content,
					Tags:      h.Tags,
					Source:    h.Source,
					Layer:     h.Layer,
					Type:      h.Type,
					UpdatedAt: h.UpdatedAt,
				}
				line, err := json.Marshal(rec)
				if err != nil {
					return total, err
				}
				if _, err := bw.Write(append(line, '\n')); err != nil {
					return total, err
				}
				total++
			}
			if len(hits) < exportPageSize {
				break
			}
		}
	}
	if err := bw.Flush(); err != nil {
		return total, err
	}
	return total, nil
}

// ImportInto replays JSONL records into eng under targetWorkspace and
// returns (imported, gateDropped). Blank lines are skipped; malformed lines
// abort the import with their line number.
func ImportInto(ctx context.Context, eng Engine, r io.Reader, targetWorkspace string) (imported, dropped int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var rec ExportRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return imported, dropped, fmt.Errorf("memory import: line %d: %w", line, err)
		}
		if rec.Content == "" {
			continue
		}
		id, err := eng.Remember(ctx, targetWorkspace, rec.Content, rec.Tags, orDefaultSource(rec.Source))
		if err != nil {
			return imported, dropped, fmt.Errorf("memory import: line %d: %w", line, err)
		}
		if id == "" {
			dropped++
			continue
		}
		imported++
	}
	return imported, dropped, sc.Err()
}

func orDefaultSource(s string) string {
	if s == "" {
		return "woca:migration"
	}
	return s
}
