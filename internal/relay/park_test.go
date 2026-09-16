package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
