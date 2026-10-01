package sdk_test

import (
	"context"
	"fmt"
	"log"

	"github.com/projanvil/woca/sdk"
)

// reportingPlugin shows the smallest useful plugin: an observe-only audit
// trail of the run lifecycle.
type reportingPlugin struct {
	runID string
}

func (p *reportingPlugin) Name() string { return "reporting" }

func (p *reportingPlugin) Observe(_ context.Context, ev sdk.RunEvent) {
	switch ev.Kind {
	case sdk.RunEventStart:
		p.runID = ev.RunID
		fmt.Println("run started:", p.runID)
	case sdk.RunEventMessage:
		if ev.Message != nil && ev.Message.Role == "assistant" && ev.Message.Content != "" {
			fmt.Println("assistant:", ev.Message.Content)
		}
	case sdk.RunEventEnd:
		fmt.Println("run finished:", ev.Result.Subtype, "cost known:", ev.Result.CostUSD != nil)
	}
}

// httpSink is a custom TelemetrySink — WithTelemetry accepts any
// implementation; the built-in OTLP/Langfuse export needs no code —
// it is enabled by the langfuse_* settings keys or WOCA_LANGFUSE_* env.
type httpSink struct{}

func (httpSink) Record(_ context.Context, sp sdk.Span) {
	fmt.Println("span:", sp.Name, sp.Kind)
}

// ExampleQuery_withPlugins wires a custom reporting plugin and a custom
// telemetry sink into one embedded run.
func ExampleQuery_withPlugins() {
	s, err := sdk.Query(context.Background(), "list the files in this repo",
		sdk.WithModel("claude-sonnet-4-5"),
		sdk.WithPermissionMode("bypassPermissions"),
		sdk.WithMaxTurns(10),
		sdk.WithPlugins(&reportingPlugin{}),
		sdk.WithTelemetry(httpSink{}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	for ev := range s.Events() {
		if ev.Type == sdk.EventResult {
			fmt.Println("done:", ev.Result.Subtype)
		}
	}
}

// ExampleNewSession shows a multi-turn conversation plus a memory-style
// model hook (the pattern the built-in ladyM memory uses internally):
// retrieved context is attached ahead of each model request, scoped to
// that request only.
func ExampleNewSession() {
	knowledge := knowledgePlugin{fetch: func(prompt string) string {
		return "quarterly revenue is up" // a real plugin retrieves this
	}}

	s, err := sdk.NewSession(context.Background(),
		sdk.WithModel("claude-sonnet-4-5"),
		sdk.WithPermissionMode("bypassPermissions"),
		sdk.WithPlugins(knowledge),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	for _, q := range []string{"read the Makefile", "summarize what you saw"} {
		evs, err := s.Ask(ctx, q)
		if err != nil {
			log.Fatal(err)
		}
		for ev := range evs {
			if ev.Type == sdk.EventResult {
				fmt.Println(q, "->", ev.Result.Subtype)
			}
		}
	}
}

// knowledgePlugin injects retrieved context ahead of every model request.
type knowledgePlugin struct {
	fetch func(prompt string) string
}

func (p knowledgePlugin) Name() string { return "knowledge" }

func (p knowledgePlugin) BeforeModel(_ context.Context, turn *sdk.ModelTurn) sdk.BeforeModelResult {
	return sdk.BeforeModelResult{PrependContext: p.fetch(turn.Prompt)}
}

func (p knowledgePlugin) AfterModel(_ context.Context, _ *sdk.ModelTurn, r *sdk.ModelReply) *sdk.ModelReply {
	return nil // keep the reply unchanged
}

func ExampleQuery() {
	s, err := sdk.Query(context.Background(), "list the files in this repo",
		sdk.WithModel("claude-sonnet-4-5"),
		sdk.WithPermissionMode("bypassPermissions"),
		sdk.WithMaxTurns(10),
	)
	if err != nil {
		panic(err)
	}
	defer s.Close()
	for ev := range s.Events() {
		switch ev.Type {
		case sdk.EventAssistant:
			fmt.Println("assistant:", ev.Message.Content)
		case sdk.EventResult:
			if ev.Result.CostUSD != nil {
				fmt.Println("cost:", *ev.Result.CostUSD)
			}
			fmt.Println("done:", ev.Result.Subtype)
		}
	}
}
