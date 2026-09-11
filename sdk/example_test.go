package sdk_test

import (
	"context"
	"fmt"

	"github.com/projanvil/woca/sdk"
)

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
			fmt.Println("done:", ev.Result.Subtype, "cost:", ev.Result.CostUSD)
		}
	}
}
