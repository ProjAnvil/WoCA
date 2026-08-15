package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSubscribeAndUnsubscribe(t *testing.T) {
	var calls []struct {
		method string
		uri    string
	}
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		if method == "initialize" {
			return map[string]any{}, nil
		}
		if method == "resources/subscribe" || method == "resources/unsubscribe" {
			var p struct {
				URI string `json:"uri"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			calls = append(calls, struct {
				method string
				uri    string
			}{method, p.URI})
			return map[string]any{}, nil
		}
		return nil, nil
	})
	client := NewClient("woca", w, r, nil)
	_ = client.Initialize(context.Background())

	if err := client.Subscribe(context.Background(), "file:///a"); err != nil {
		t.Fatal(err)
	}
	if err := client.Unsubscribe(context.Background(), "file:///b"); err != nil {
		t.Fatal(err)
	}

	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].method != "resources/subscribe" || calls[0].uri != "file:///a" {
		t.Fatalf("calls[0] = %+v", calls[0])
	}
	if calls[1].method != "resources/unsubscribe" || calls[1].uri != "file:///b" {
		t.Fatalf("calls[1] = %+v", calls[1])
	}
}

func TestCreateMessageSingleBlock(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		if method == "initialize" {
			return map[string]any{}, nil
		}
		if method == "sampling/createMessage" {
			return map[string]any{
				"role":    "assistant",
				"content": map[string]any{"type": "text", "text": "hello"},
			}, nil
		}
		return nil, nil
	})
	client := NewClient("woca", w, r, nil)
	_ = client.Initialize(context.Background())

	got, err := client.CreateMessage(context.Background(), CreateMessageParams{
		Messages:  []SamplingMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Fatalf("got = %q", got)
	}
}

func TestCreateMessageArrayBlocks(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		if method == "initialize" {
			return map[string]any{}, nil
		}
		if method == "sampling/createMessage" {
			return map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": "part1"},
					{"type": "text", "text": "part2"},
				},
			}, nil
		}
		return nil, nil
	})
	client := NewClient("woca", w, r, nil)
	_ = client.Initialize(context.Background())

	got, err := client.CreateMessage(context.Background(), CreateMessageParams{
		Messages:  []SamplingMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "part1part2" {
		t.Fatalf("got = %q", got)
	}
}

func TestCreateMessagePropagatesError(t *testing.T) {
	w, r := startFakeServer(t, func(method string, params json.RawMessage) (any, error) {
		if method == "initialize" {
			return map[string]any{}, nil
		}
		if method == "sampling/createMessage" {
			return nil, &rpcError{Code: -32600, Message: "boom"}
		}
		return nil, nil
	})
	client := NewClient("woca", w, r, nil)
	_ = client.Initialize(context.Background())

	_, err := client.CreateMessage(context.Background(), CreateMessageParams{
		Messages:  []SamplingMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
