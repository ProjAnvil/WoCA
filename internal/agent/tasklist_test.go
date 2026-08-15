package agent

import "testing"

func TestTaskListCRUD(t *testing.T) {
	l := newTaskList()

	rec := l.Create("subject", "desc", "doing")
	if rec.ID == "" || rec.Status != TaskRecordPending {
		t.Fatalf("created = %+v", rec)
	}

	got, ok := l.Get(rec.ID)
	if !ok || got.Subject != "subject" {
		t.Fatalf("get = %+v ok=%v", got, ok)
	}

	upd, ok := l.Update(rec.ID, TaskRecordInProgress, "", "new desc")
	if !ok || upd.Status != TaskRecordInProgress || upd.Description != "new desc" {
		t.Fatalf("update = %+v ok=%v", upd, ok)
	}

	if got := len(l.List()); got != 1 {
		t.Fatalf("list len = %d", got)
	}

	if _, ok := l.Update("nope", TaskRecordCompleted, "", ""); ok {
		t.Fatal("Update on missing id should not be ok")
	}
}
