// idle.go surfaces the message plane's dispatch waiter as the worker idle
// seam: the scheduler role's embedded run loop (internal/ee/schedulerloop)
// parks between empty claims on the same NATS wake-up the standalone worker
// uses, instead of polling the database blind.
package edition

import (
	"github.com/projanvil/woca/internal/ee/worker"
	"github.com/projanvil/woca/internal/server"
)

// IdleWaiter returns the waiter Plane embedded, so an embedded run loop
// parked on an empty queue wakes the instant a dispatch notification lands
// (with the plane's poll safety net). Planes this package did not build —
// and a nil plane — yield no waiter; the loop falls back to its poll
// cadence.
func IdleWaiter(p server.MsgPlane) worker.IdleWaiter {
	if pw, ok := p.(*planeWithWaiter); ok && pw.waiter != nil {
		return pw.waiter
	}
	return nil
}
