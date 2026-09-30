package sdk_test

import (
	"context"
	"fmt"
	"log"

	"github.com/projanvil/woca/sdk"
	"github.com/projanvil/woca/sdk/langfuse"
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

// ExampleQuery_withPlugins wires a custom reporting plugin and native
// Langfuse telemetry into one embedded run.
func ExampleQuery_withPlugins() {
	lf, err := langfuse.New(langfuse.Config{
		Host:      "https://cloud.langfuse.com",
		PublicKey: "pk-lf-example",
		SecretKey: "sk-lf-example",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lf.Close()

	s, err := sdk.Query(context.Background(), "list the files in this repo",
		sdk.WithModel("claude-sonnet-4-5"),
		sdk.WithPermissionMode("bypassPermissions"),
		sdk.WithMaxTurns(10),
		sdk.WithPlugins(&reportingPlugin{}),
		sdk.WithTelemetry(lf),
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
