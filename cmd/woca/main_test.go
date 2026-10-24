package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/webgui"
)

func TestTodosTasksConversionRoundTrip(t *testing.T) {
	todos := []tools.Todo{{Content: "a", Status: "in_progress", ActiveForm: "doing a"}}
	tasks := []agent.TaskRecord{{ID: "task-3", Subject: "s", Description: "d", Status: agent.TaskRecordCompleted}}

	if got := todosFromSession(todosToSession(todos)); len(got) != 1 || got[0].Content != "a" {
		t.Fatalf("todos round-trip = %+v", got)
	}
	if got := tasksFromSession(tasksToSession(tasks)); len(got) != 1 || got[0].ID != "task-3" || got[0].Status != agent.TaskRecordCompleted {
		t.Fatalf("tasks round-trip = %+v", got)
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		addr := webgui.DefaultAddr
		for i := 1; i < len(os.Args)-1; i++ {
			if os.Args[i] == "--addr" {
				addr = os.Args[i+1]
			}
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `{"ok":true}`)
		})
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			os.Exit(1)
		}
		_ = (&http.Server{Handler: mux}).Serve(ln)
		os.Exit(0)
	}
	// Tests must never launch the real browser: start with a no-op opener and
	// let individual tests opt in via stubBrowserOpen.
	agentbuild.OpenBrowser = func(string) {}
	os.Exit(m.Run())
}
