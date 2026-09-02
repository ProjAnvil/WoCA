package pluginrt

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/runws"
)

// pluginWorkdir resolves the cwd for a plugin subprocess: the calling run's
// workspace when the context carries one (serve-mode sessions), else the
// harness workdir the manager was built with. Plugin scripts resolve
// relative paths against this directory.
func pluginWorkdir(ctx context.Context, fallback string) string {
	if ws, ok := runws.From(ctx); ok && ws != "" {
		return ws
	}
	return fallback
}

// browserTools builds the browser plugin's agent tools over the pinchtab
// client. The server is started (or adopted) lazily on first use.
func browserTools(br *BrowserRuntime) []coretools.Tool {
	mk := func(name, desc string, fn any) coretools.Tool {
		t, err := coretools.FromFunc(name, desc, fn)
		if err != nil {
			panic(err) // reflection failure = programmer error
		}
		return t
	}

	type navigateArgs struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars,omitempty"`
	}
	type snapshotArgs struct{}
	type textArgs struct {
		MaxChars int  `json:"max_chars,omitempty"`
		Raw      bool `json:"raw,omitempty"`
	}
	type actionArgs struct {
		Kind string `json:"kind"` // click | fill | press
		Ref  string `json:"ref,omitempty"`
		Text string `json:"text,omitempty"`
		Key  string `json:"key,omitempty"`
	}

	navigate := mk("BrowserNavigate",
		`Navigate the browser to a URL and return the page: its title, the main text content (readability extraction, token-efficient) and a snapshot of interactive elements (refs like e5). Use BrowserAction with those refs to interact. Requires the browser plugin (pinchtab + local Chrome).`,
		func(ctx context.Context, a navigateArgs) (coretools.Result, error) {
			if a.URL == "" {
				return coretools.Result{}, fmt.Errorf("url is required")
			}
			var out string
			err := br.call(ctx, func(c *pinchClient) error {
				nav, err := c.navigate(ctx, a.URL)
				if err != nil {
					return err
				}
				limit := a.MaxChars
				if limit <= 0 {
					limit = 6000
				}
				var b strings.Builder
				fmt.Fprintf(&b, "Navigated: %s\nURL: %s\n", nav.Title, nav.URL)
				if text, err := c.text(ctx, limit, false); err == nil && text.Text != "" {
					trunc := ""
					if text.Truncated {
						trunc = " (truncated)"
					}
					fmt.Fprintf(&b, "\n--- page text%s ---\n%s\n", trunc, text.Text)
				}
				if snap, err := c.snapshot(ctx); err == nil && strings.TrimSpace(snap) != "" {
					fmt.Fprintf(&b, "\n--- interactive elements ---\n%s\n", strings.TrimSpace(snap))
				}
				out = b.String()
				return nil
			})
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: out}, nil
		})

	snapshot := mk("BrowserSnapshot",
		`Return the current browser tab's interactive elements as compact lines like "[e5] link \"Sign in\"". Use the refs (e5) with BrowserAction.`,
		func(ctx context.Context, _ snapshotArgs) (coretools.Result, error) {
			var snap string
			err := br.call(ctx, func(c *pinchClient) error {
				s, err := c.snapshot(ctx)
				if err != nil {
					return err
				}
				snap = s
				return nil
			})
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: strings.TrimSpace(snap)}, nil
		})

	text := mk("BrowserText",
		`Return the current browser tab's main text content (readability extraction). Optional max_chars (default 12000); raw=true returns innerText instead of readability.`,
		func(ctx context.Context, a textArgs) (coretools.Result, error) {
			limit := a.MaxChars
			if limit <= 0 {
				limit = 12000
			}
			var out string
			err := br.call(ctx, func(c *pinchClient) error {
				res, err := c.text(ctx, limit, a.Raw)
				if err != nil {
					return err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s\n%s\n\n%s", res.Title, res.URL, res.Text)
				if res.Truncated {
					b.WriteString("\n(text truncated)")
				}
				out = b.String()
				return nil
			})
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: out}, nil
		})

	action := mk("BrowserAction",
		`Interact with the current browser tab: click an element, fill an input, or press a key. Use refs from BrowserNavigate/BrowserSnapshot (e.g. e5). kind=click needs ref; kind=fill needs ref + text; kind=press needs key (Enter, Tab, Escape, ArrowDown, ArrowUp, Backspace, Delete).`,
		func(ctx context.Context, a actionArgs) (coretools.Result, error) {
			body := map[string]string{"kind": a.Kind}
			switch a.Kind {
			case "click":
				if a.Ref == "" {
					return coretools.Result{}, fmt.Errorf("click needs a ref (from a snapshot)")
				}
				body["ref"] = a.Ref
			case "fill":
				if a.Ref == "" || a.Text == "" {
					return coretools.Result{}, fmt.Errorf("fill needs ref and text")
				}
				body["ref"], body["text"] = a.Ref, a.Text
			case "press":
				if a.Key == "" {
					return coretools.Result{}, fmt.Errorf("press needs a key (e.g. Enter)")
				}
				body["key"] = a.Key
			default:
				return coretools.Result{}, fmt.Errorf("kind must be click, fill or press, got %q", a.Kind)
			}
			var out string
			err := br.call(ctx, func(c *pinchClient) error {
				if err := c.action(ctx, body); err != nil {
					return err
				}
				// Report the post-action state so the model can chain steps.
				var b strings.Builder
				fmt.Fprintf(&b, "%s ok", a.Kind)
				if snap, err := c.snapshot(ctx); err == nil && strings.TrimSpace(snap) != "" {
					fmt.Fprintf(&b, "\n\n--- interactive elements ---\n%s", strings.TrimSpace(snap))
				}
				out = b.String()
				return nil
			})
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: out}, nil
		})

	return []coretools.Tool{navigate, snapshot, text, action}
}

// pythonTools builds the tools of a python plugin: each runs the plugin's
// embedded scripts through the uv-managed venv and relays the script's JSON
// protocol. Tool surface is declared per plugin (pdf, office).
func pythonTools(b Builtin, py *PythonRuntime, workdir string) []coretools.Tool {
	switch b.Name {
	case "pdf":
		return pdfTools(py, workdir)
	case "office":
		return officeTools(py, workdir)
	default:
		return nil
	}
}

func pdfTools(py *PythonRuntime, workdir string) []coretools.Tool {
	mk := func(name, desc string, fn any) coretools.Tool {
		t, err := coretools.FromFunc(name, desc, fn)
		if err != nil {
			panic(err)
		}
		return t
	}
	run := func(ctx context.Context, args ...string) (map[string]any, error) {
		stdout, stderr, err := py.Run(ctx, "pdf", "scripts/pdf.py", args, pluginWorkdir(ctx, workdir), 120*time.Second)
		if err != nil && stdout == "" {
			detail := strings.TrimSpace(stderr)
			if detail == "" {
				detail = err.Error()
			}
			return nil, fmt.Errorf("run pdf.py: %s", detail)
		}
		var payload map[string]any
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &payload); jerr != nil {
			return nil, fmt.Errorf("pdf.py returned invalid JSON: %v\nstdout: %s\nstderr: %s", jerr, tailLines(stdout, 5), tailLines(stderr, 5))
		}
		return payload, nil
	}

	type readArgs struct {
		File     string `json:"file"`
		Password string `json:"password,omitempty"`
		Pages    string `json:"pages,omitempty"` // e.g. "1-3,5" (1-based, inclusive)
		MaxChars int    `json:"max_chars,omitempty"`
		Op       string `json:"op,omitempty"` // text (default) | metadata
	}
	read := mk("PdfRead",
		`Read a PDF: extract text (default) or metadata (op=metadata). Optional pages selection like "1-3,5", max_chars cap (default 20000), and password for encrypted files. Returns page count plus the text.`,
		func(ctx context.Context, a readArgs) (coretools.Result, error) {
			if a.File == "" {
				return coretools.Result{}, fmt.Errorf("file is required")
			}
			op := a.Op
			if op == "" {
				op = "text"
			}
			if op != "text" && op != "metadata" {
				return coretools.Result{}, fmt.Errorf("op must be text or metadata, got %q", op)
			}
			args := []string{op, a.File}
			if a.Password != "" {
				args = append(args, "--password", a.Password)
			}
			if op == "text" && a.Pages != "" {
				args = append(args, "--pages", a.Pages)
			}
			if op == "text" && a.MaxChars > 0 {
				args = append(args, "--max-chars", strconv.Itoa(a.MaxChars))
			}
			payload, err := run(ctx, args...)
			if err != nil {
				return coretools.Result{}, err
			}
			if ok, _ := payload["ok"].(bool); !ok {
				return coretools.Result{}, fmt.Errorf("%v", payload["error"])
			}
			var b strings.Builder
			if op == "metadata" {
				fmt.Fprintf(&b, "%s — %v pages", a.File, payload["pages"])
				if enc, _ := payload["encrypted"].(bool); enc {
					b.WriteString(" (encrypted)")
				}
				if meta, ok := payload["metadata"].(map[string]any); ok && len(meta) > 0 {
					b.WriteString("\n")
					keys := make([]string, 0, len(meta))
					for k := range meta {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						fmt.Fprintf(&b, "  %s: %v\n", k, meta[k])
					}
				}
				return coretools.Result{Content: b.String()}, nil
			}
			fmt.Fprintf(&b, "%s — %v pages", a.File, payload["pages"])
			if sel, _ := payload["selected"].(float64); sel > 0 {
				fmt.Fprintf(&b, " (%v selected)", int(sel))
			}
			b.WriteString("\n\n")
			// Guarded assertions: a schema drift in pdf.py (ok=true without
			// a text field) must degrade to an empty render, not panic the
			// tool goroutine.
			text, _ := payload["text"].(string)
			b.WriteString(text)
			if trunc, _ := payload["truncated"].(bool); trunc {
				b.WriteString("\n(text truncated)")
			}
			return coretools.Result{Content: b.String()}, nil
		})

	type writeArgs struct {
		Op       string   `json:"op"` // merge | split | rotate | encrypt | decrypt
		Files    []string `json:"files"`
		Output   string   `json:"output,omitempty"`
		OutDir   string   `json:"out_dir,omitempty"` // split only
		Pages    string   `json:"pages,omitempty"`   // rotate only
		Angle    int      `json:"angle,omitempty"`   // rotate only (multiple of 90)
		Password string   `json:"password,omitempty"`
	}
	write := mk("PdfWrite",
		`Modify PDFs: op=merge (files[0..n] -> output), split (files[0] -> out_dir, one file per page), rotate (files[0], angle=90/180/270, optional pages, -> output), encrypt (files[0] + password -> output, AES-256), decrypt (files[0] + password -> output). Paths are relative to the working directory.`,
		func(ctx context.Context, a writeArgs) (coretools.Result, error) {
			var args []string
			switch a.Op {
			case "merge":
				if a.Output == "" || len(a.Files) < 2 {
					return coretools.Result{}, fmt.Errorf("merge needs output and at least two files")
				}
				args = append([]string{"merge", a.Output}, a.Files...)
			case "split":
				if len(a.Files) != 1 || a.OutDir == "" {
					return coretools.Result{}, fmt.Errorf("split needs exactly one file and out_dir")
				}
				args = []string{"split", a.Files[0], a.OutDir}
			case "rotate":
				if len(a.Files) != 1 || a.Output == "" || a.Angle == 0 {
					return coretools.Result{}, fmt.Errorf("rotate needs one file, angle (multiple of 90) and output")
				}
				args = []string{"rotate", a.Files[0], fmt.Sprintf("%d", a.Angle), a.Output}
			case "encrypt", "decrypt":
				if len(a.Files) != 1 || a.Output == "" || a.Password == "" {
					return coretools.Result{}, fmt.Errorf("%s needs one file, password and output", a.Op)
				}
				args = []string{a.Op, a.Files[0], a.Password, a.Output}
			default:
				return coretools.Result{}, fmt.Errorf("op must be merge, split, rotate, encrypt or decrypt, got %q", a.Op)
			}
			if a.Password != "" && (a.Op == "merge" || a.Op == "split" || a.Op == "rotate") {
				args = append(args, "--password", a.Password)
			}
			payload, err := run(ctx, args...)
			if err != nil {
				return coretools.Result{}, err
			}
			if ok, _ := payload["ok"].(bool); !ok {
				return coretools.Result{}, fmt.Errorf("%v", payload["error"])
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s ok", a.Op)
			if output, _ := payload["output"].(string); output != "" {
				fmt.Fprintf(&b, ": %s", output)
			}
			if pages, ok := payload["pages"].(float64); ok {
				fmt.Fprintf(&b, " (%v pages)", int(pages))
			}
			if files, ok := payload["files"].([]any); ok {
				b.WriteString("\n")
				for _, f := range files {
					fmt.Fprintf(&b, "  %v\n", f)
				}
			}
			return coretools.Result{Content: strings.TrimSpace(b.String())}, nil
		})

	return []coretools.Tool{read, write}
}

// officeTools builds the office plugin's agent tools (docx/xlsx/pptx read and
// write over the embedded office.py). Document payloads travel as a JSON spec
// argv value — same channel pdf uses for its arguments — so the subprocess
// stays compatible with sandboxed executors (no host-side temp files).
func officeTools(py *PythonRuntime, workdir string) []coretools.Tool {
	mk := func(name, desc string, fn any) coretools.Tool {
		t, err := coretools.FromFunc(name, desc, fn)
		if err != nil {
			panic(err)
		}
		return t
	}
	run := func(ctx context.Context, args ...string) (map[string]any, error) {
		stdout, stderr, err := py.Run(ctx, "office", "scripts/office.py", args, pluginWorkdir(ctx, workdir), 180*time.Second)
		if err != nil && stdout == "" {
			detail := strings.TrimSpace(stderr)
			if detail == "" {
				detail = err.Error()
			}
			return nil, fmt.Errorf("run office.py: %s", detail)
		}
		var payload map[string]any
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &payload); jerr != nil {
			return nil, fmt.Errorf("office.py returned invalid JSON: %v\nstdout: %s\nstderr: %s", jerr, tailLines(stdout, 5), tailLines(stderr, 5))
		}
		return payload, nil
	}
	payloadOrErr := func(ctx context.Context, args ...string) (map[string]any, error) {
		payload, err := run(ctx, args...)
		if err != nil {
			return nil, err
		}
		if ok, _ := payload["ok"].(bool); !ok {
			return nil, fmt.Errorf("%v", payload["error"])
		}
		return payload, nil
	}
	// maxSpecBytes keeps a single write inside platform argv limits (macOS
	// caps one argument at 256KiB); larger documents should be split into
	// multiple writes.
	const maxSpecBytes = 200 << 10

	type readArgs struct {
		File     string `json:"file"`
		MaxChars int    `json:"max_chars,omitempty"`
	}
	read := mk("OfficeRead",
		`Read an Office document as Markdown: Word (.docx) → headings/paragraphs/bullets/tables, Excel (.xlsx) → one table per sheet, PowerPoint (.pptx) → per-slide title/bullets/notes. Macro variants (.docm/.xlsm/.pptm) are readable too. Optional max_chars cap (default 20000). Requires the office plugin.`,
		func(ctx context.Context, a readArgs) (coretools.Result, error) {
			if a.File == "" {
				return coretools.Result{}, fmt.Errorf("file is required")
			}
			args := []string{"read", a.File}
			if a.MaxChars > 0 {
				args = append(args, "--max-chars", strconv.Itoa(a.MaxChars))
			}
			payload, err := payloadOrErr(ctx, args...)
			if err != nil {
				return coretools.Result{}, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s (%s)\n\n", a.File, payload["format"])
			// Guarded assertions: schema drift in office.py must degrade to
			// an empty render, not panic the tool goroutine.
			text, _ := payload["markdown"].(string)
			b.WriteString(text)
			if trunc, _ := payload["truncated"].(bool); trunc {
				b.WriteString("\n(text truncated)")
			}
			return coretools.Result{Content: b.String()}, nil
		})

	type docxBlock struct {
		Type   string     `json:"type"` // heading | paragraph | bullets | table | pagebreak
		Text   string     `json:"text,omitempty"`
		Level  int        `json:"level,omitempty"` // heading: 0=Title style, 1-9
		Items  []string   `json:"items,omitempty"` // bullets
		Header []string   `json:"header,omitempty"`
		Rows   [][]string `json:"rows,omitempty"` // table body
	}
	type docxWriteArgs struct {
		Path   string      `json:"path"`
		Title  string      `json:"title,omitempty"`
		Blocks []docxBlock `json:"blocks,omitempty"`
	}
	docxWrite := mk("DocxWrite",
		`Create a Word document at path from structured blocks: [{type:heading,text,level(0=title,1-9)},{type:paragraph,text},{type:bullets,items[]},{type:table,header[],rows[][]},{type:pagebreak}]. Optional document title. Overwrites existing files. Requires the office plugin.`,
		func(ctx context.Context, a docxWriteArgs) (coretools.Result, error) {
			if a.Path == "" {
				return coretools.Result{}, fmt.Errorf("path is required")
			}
			if a.Title == "" && len(a.Blocks) == 0 {
				return coretools.Result{}, fmt.Errorf("need a title or at least one block")
			}
			for i, blk := range a.Blocks {
				switch blk.Type {
				case "heading":
					if blk.Text == "" {
						return coretools.Result{}, fmt.Errorf("blocks[%d] heading needs text", i)
					}
				case "paragraph":
					if blk.Text == "" {
						return coretools.Result{}, fmt.Errorf("blocks[%d] paragraph needs text", i)
					}
				case "bullets":
					if len(blk.Items) == 0 {
						return coretools.Result{}, fmt.Errorf("blocks[%d] bullets needs items", i)
					}
				case "table":
					if len(blk.Header) == 0 {
						return coretools.Result{}, fmt.Errorf("blocks[%d] table needs header", i)
					}
				case "pagebreak":
				default:
					return coretools.Result{}, fmt.Errorf("blocks[%d] type must be heading, paragraph, bullets, table or pagebreak, got %q", i, blk.Type)
				}
			}
			spec, err := json.Marshal(map[string]any{"title": a.Title, "blocks": a.Blocks})
			if err != nil {
				return coretools.Result{}, err
			}
			if len(spec) > maxSpecBytes {
				return coretools.Result{}, fmt.Errorf("document too large (%d bytes > %d); split into multiple writes", len(spec), maxSpecBytes)
			}
			payload, err := payloadOrErr(ctx, "docx-write", "--output", a.Path, "--spec", string(spec))
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: fmt.Sprintf("docx written: %v (%v blocks, %v bytes)", payload["output"], payload["blocks"], payload["bytes"])}, nil
		})

	type xlsxSheet struct {
		Name      string     `json:"name,omitempty"`
		Rows      [][]string `json:"rows"`
		HeaderRow bool       `json:"header_row,omitempty"` // bold + freeze + autofilter
	}
	type xlsxWriteArgs struct {
		Path   string      `json:"path"`
		Sheets []xlsxSheet `json:"sheets"`
	}
	xlsxWrite := mk("XlsxWrite",
		`Create an Excel workbook at path with one or more sheets: [{name, rows:[["Name","Qty"],...], header_row}]. Cell strings auto-type: plain numbers become numeric, "=" prefix becomes a formula, "'" prefix forces text (keeps leading zeros). header_row bolds/freezes/filters the first row. Overwrites existing files. Requires the office plugin.`,
		func(ctx context.Context, a xlsxWriteArgs) (coretools.Result, error) {
			if a.Path == "" {
				return coretools.Result{}, fmt.Errorf("path is required")
			}
			if len(a.Sheets) == 0 {
				return coretools.Result{}, fmt.Errorf("at least one sheet is required")
			}
			rows := 0
			for i, sh := range a.Sheets {
				if len(sh.Rows) == 0 {
					return coretools.Result{}, fmt.Errorf("sheets[%d] needs at least one row", i)
				}
				rows += len(sh.Rows)
			}
			spec, err := json.Marshal(map[string]any{"sheets": a.Sheets})
			if err != nil {
				return coretools.Result{}, err
			}
			if len(spec) > maxSpecBytes {
				return coretools.Result{}, fmt.Errorf("workbook too large (%d bytes > %d); split into multiple writes", len(spec), maxSpecBytes)
			}
			payload, err := payloadOrErr(ctx, "xlsx-write", "--output", a.Path, "--spec", string(spec))
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: fmt.Sprintf("xlsx written: %v (%v sheets, %v rows, %v bytes)", payload["output"], payload["sheets"], payload["rows"], payload["bytes"])}, nil
		})

	type pptxBullet struct {
		Text  string `json:"text"`
		Level int    `json:"level,omitempty"` // 0-4 nesting
	}
	type pptxSlide struct {
		Title   string       `json:"title"`
		Bullets []pptxBullet `json:"bullets,omitempty"`
		Notes   string       `json:"notes,omitempty"`
	}
	type pptxWriteArgs struct {
		Path     string      `json:"path"`
		Title    string      `json:"title,omitempty"`    // optional cover slide
		Subtitle string      `json:"subtitle,omitempty"` // cover subtitle
		Slides   []pptxSlide `json:"slides"`
	}
	pptxWrite := mk("PptxWrite",
		`Create a PowerPoint deck at path: optional cover (title+subtitle) plus slides [{title, bullets:[{text,level 0-4}], notes}]. Overwrites existing files. Requires the office plugin.`,
		func(ctx context.Context, a pptxWriteArgs) (coretools.Result, error) {
			if a.Path == "" {
				return coretools.Result{}, fmt.Errorf("path is required")
			}
			if a.Title == "" && len(a.Slides) == 0 {
				return coretools.Result{}, fmt.Errorf("need a cover title or at least one slide")
			}
			for i, s := range a.Slides {
				if s.Title == "" && len(s.Bullets) == 0 {
					return coretools.Result{}, fmt.Errorf("slides[%d] needs a title or bullets", i)
				}
			}
			spec, err := json.Marshal(map[string]any{"title": a.Title, "subtitle": a.Subtitle, "slides": a.Slides})
			if err != nil {
				return coretools.Result{}, err
			}
			if len(spec) > maxSpecBytes {
				return coretools.Result{}, fmt.Errorf("deck too large (%d bytes > %d); split into multiple writes", len(spec), maxSpecBytes)
			}
			payload, err := payloadOrErr(ctx, "pptx-write", "--output", a.Path, "--spec", string(spec))
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: fmt.Sprintf("pptx written: %v (%v slides, %v bytes)", payload["output"], payload["slides"], payload["bytes"])}, nil
		})

	return []coretools.Tool{read, docxWrite, xlsxWrite, pptxWrite}
}
