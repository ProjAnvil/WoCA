package tools

import (
	"strings"
	"testing"
)

func TestAskUserQuestionTool(t *testing.T) {
	tool := AskUserQuestionTool()
	res, err := tool.Invoke(t.Context(), map[string]any{
		"questions": []map[string]any{{"question": "Which library?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "handled by the harness") {
		t.Fatalf("AskUserQuestion = %q", res.Content)
	}
	if _, err := tool.Invoke(t.Context(), map[string]any{"questions": []map[string]any{}}); err == nil ||
		!strings.Contains(err.Error(), "at least one question") {
		t.Fatalf("empty questions error = %v", err)
	}
}

func TestPlanModeTools(t *testing.T) {
	enter := EnterPlanModeTool()
	res, err := enter.Invoke(t.Context(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "EnterPlanMode is handled by the harness") {
		t.Fatalf("EnterPlanMode = %q", res.Content)
	}
	exit := ExitPlanModeTool()
	res, err = exit.Invoke(t.Context(), map[string]any{"plan": "do it"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "ExitPlanMode is handled by the harness") {
		t.Fatalf("ExitPlanMode = %q", res.Content)
	}
}

func TestFormatQuestion(t *testing.T) {
	q := Question{
		Question: "Deploy where?",
		Header:   "Deploy",
		Options: []QuestionOption{
			{Label: "staging"},
			{Label: "prod", Description: "the real thing"},
		},
		MultiSelect: true,
	}
	got := FormatQuestion(0, q)
	for _, want := range []string{
		"Deploy\n",
		"1. Deploy where?\n",
		"   1) staging\n",
		"   2) prod — the real thing\n",
		"(select multiple, comma-separated)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatQuestion missing %q in:\n%s", want, got)
		}
	}

	plain := FormatQuestion(2, Question{Question: "Continue?", Options: []QuestionOption{{Label: "yes"}}})
	if plain != "3. Continue?\n   1) yes\n" {
		t.Errorf("plain question = %q", plain)
	}
	if !strings.Contains(plain, "3. Continue?") {
		t.Errorf("plain question missing index:\n%s", plain)
	}
}
