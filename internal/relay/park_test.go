package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func testRequest(id string) ParkedRequest {
	return ParkedRequest{ID: id, Method: "GET", Path: "/api/sessions", Headers: map[string][]string{}}
}

func TestParkTakeComplete(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	ctx := context.Background()
	parked := make(chan *ParkedResponse, 1)
	// checked gates the client goroutine's deferred done() so it cannot
	// close streamDone before the main goroutine's assertion below: the
	// parked channel is buffered, so without this gate the goroutine runs
	// done() immediately after its send and the assertion races (and
	// loses) against the streamDone close.
	checked := make(chan struct{})
	go func() {
		resp, done, _, err := q.Park(ctx, 7, testRequest("r1"))
		if err != nil {
			return
		}
		defer done()
		parked <- resp
		<-checked
	}()
	req, err := q.Take(ctx, 7, "gen-a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if req.ID != "r1" {
		t.Fatalf("took %+v", req)
	}
	resp := &ParkedResponse{Status: http.StatusOK}
	streamDone, err := q.Complete(7, "gen-a", "r1", resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-parked; got != resp {
		t.Fatal("client did not receive the respond")
	}
	select {
	case <-streamDone:
		t.Fatal("streamDone closed before the client finished")
	default:
	}
	// Release the client goroutine: its deferred done() must now close
	// streamDone.
	close(checked)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-streamDone:
			return
		case <-deadline:
			t.Fatal("streamDone never closed after done()")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestParkQueueFull(t *testing.T) {
	q := NewParkingQueue(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 2; i++ {
		go func(i int) {
			_, _, _, _ = q.Park(ctx, 7, testRequest(fmt.Sprintf("r%d", i)))
		}(i)
	}
	deadline := time.After(time.Second)
	for q.Len(7) < 2 {
		select {
		case <-deadline:
			t.Fatal("queue never filled")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, _, _, err := q.Park(ctx, 7, testRequest("r3")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third park: %v, want ErrQueueFull (client sees 503)", err)
	}
}

func TestParkClientCancelThenRespondGone(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	ctx := context.Background()
	clientCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, _, _, err := q.Park(clientCtx, 7, testRequest("r1"))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := q.Take(ctx, 7, "gen-a", time.Second); err != nil {
		t.Fatal(err)
	}
	// The client hangs up AFTER the bridge took the request: the bridge's
	// respond must get ErrClientGone semantics (410), never a goroutine
	// leak (design §6.2).
	cancel()
	if err := <-done; err == nil {
		t.Fatal("park returned nil on client cancel")
	}
	if _, err := q.Complete(7, "gen-a", "r1", &ParkedResponse{}); !errors.Is(err, ErrClientGone) {
		t.Fatalf("respond after client cancel: %v, want ErrClientGone", err)
	}
	if n := q.Len(7); n != 0 {
		t.Fatalf("voided item left behind (n=%d)", n)
	}
}

func TestTakeTimeout(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	start := time.Now()
	_, err := q.Take(context.Background(), 7, "gen-a", 50*time.Millisecond)
	if !errors.Is(err, ErrTakeTimeout) {
		t.Fatalf("empty take: %v, want ErrTakeTimeout (204)", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("take returned before the wait window")
	}
}

// TestDoneFailAllConcurrentNoDoubleClose fires the revocation-during-stream
// race: a client's done() and a FailAll drain closing the same item's
// streamDone concurrently must not panic with "close of closed channel"
// (a panic in any goroutine crashes the test binary).
func TestDoneFailAllConcurrentNoDoubleClose(t *testing.T) {
	for i := 0; i < 100; i++ {
		q := NewParkingQueue(ParkCapacity)
		ctx := context.Background()
		parked := make(chan func(), 1)
		go func() {
			_, done, _, err := q.Park(ctx, 7, testRequest("r1"))
			if err != nil {
				parked <- nil
				return
			}
			parked <- done
		}()
		if _, err := q.Take(ctx, 7, "gen-a", time.Second); err != nil {
			t.Fatal(err)
		}
		streamDone, err := q.Complete(7, "gen-a", "r1", &ParkedResponse{Status: http.StatusOK})
		if err != nil {
			t.Fatal(err)
		}
		done := <-parked
		if done == nil {
			t.Fatal("park failed")
		}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() { defer wg.Done(); done() }()
		}
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() { defer wg.Done(); q.FailAll(7, ErrRevoked) }()
		}
		wg.Wait()
		select {
		case <-streamDone:
		default:
			t.Fatal("streamDone not closed after done + FailAll")
		}
	}
}

// TestCompleteDuplicateIsNoOp covers a bridge responding more than once to
// the same id: duplicates must be no-ops (nil error, the first respond
// wins) instead of sending into the full result channel while holding the
// queue mutex — which would freeze every Park/Take/Len.
func TestCompleteDuplicateIsNoOp(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	ctx := context.Background()
	got := make(chan *ParkedResponse, 1)
	go func() {
		resp, _, _, err := q.Park(ctx, 7, testRequest("r1"))
		if err != nil {
			return
		}
		got <- resp
	}()
	if _, err := q.Take(ctx, 7, "gen-a", time.Second); err != nil {
		t.Fatal(err)
	}
	first := &ParkedResponse{Status: http.StatusOK}
	if _, err := q.Complete(7, "gen-a", "r1", first); err != nil {
		t.Fatal(err)
	}
	// The client consumed the first respond, so nothing ever receives from
	// the item's result channel again. Duplicate responds must be no-ops
	// (nil error, first wins) — on the unfixed code the first duplicate
	// refills the result channel and the second blocks on it while holding
	// the queue mutex, freezing every Park/Take/Len.
	if resp := <-got; resp != first {
		t.Fatalf("client received %+v, want the first respond to win", resp)
	}
	second := &ParkedResponse{Status: http.StatusTeapot}
	for i := 0; i < 2; i++ {
		blocked := make(chan error, 1)
		go func() {
			_, err := q.Complete(7, "gen-a", "r1", second)
			blocked <- err
		}()
		select {
		case err := <-blocked:
			if err != nil {
				t.Fatalf("duplicate complete #%d: %v, want nil (no-op)", i+1, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("duplicate complete #%d blocked on the full result channel (queue mutex frozen)", i+1)
		}
	}
}

func TestFailAll(t *testing.T) {
	q := NewParkingQueue(ParkCapacity)
	ctx := context.Background()
	errs := make(chan error, 2)
	for _, id := range []string{"r1", "r2"} {
		go func(id string) {
			_, _, _, err := q.Park(ctx, 7, testRequest(id))
			errs <- err
		}(id)
	}
	deadline := time.After(time.Second)
	for q.Len(7) < 2 {
		select {
		case <-deadline:
			t.Fatal("queue never filled")
		case <-time.After(5 * time.Millisecond):
		}
	}
	q.FailAll(7, ErrRevoked)
	for i := 0; i < 2; i++ {
		if err := <-errs; !errors.Is(err, ErrRevoked) {
			t.Fatalf("drained park: %v, want ErrRevoked", err)
		}
	}
	if n := q.Len(7); n != 0 {
		t.Fatalf("queue len %d after FailAll", n)
	}
}

func TestCompleteWithoutTake(t *testing.T) {
	q := NewParkingQueue(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parked := make(chan error, 1)
	go func() {
		_, _, _, err := q.Park(ctx, 7, testRequest("r1"))
		parked <- err
	}()
	deadline := time.After(time.Second)
	for q.Len(7) < 1 {
		select {
		case <-deadline:
			t.Fatal("request never parked")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The id exists but no generation took it: respond is still 404-shaped.
	if _, err := q.Complete(7, "gen-1", "r1", &ParkedResponse{Status: 200}); !errors.Is(err, ErrUnknownParkedID) {
		t.Fatalf("untaken complete: %v, want ErrUnknownParkedID", err)
	}
	cancel()
	if err := <-parked; !errors.Is(err, context.Canceled) {
		t.Fatalf("park release: %v, want context.Canceled", err)
	}
}
