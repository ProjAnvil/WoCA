// Package edition assembles the enterprise subsystems injected into the
// shared serve path through servefront.Edition: API-key + native-client
// authentication over the tenant store, and the run-dispatch message plane
// with its config-plane waiter. It is the composition root woca-server
// wires in; the personal edition never links this package.
package edition

import (
	"fmt"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/msgplane"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/settings"

	"github.com/projanvil/woca/internal/ee/nativeauth"
)

// Authn builds the API-key authn provider and the native-client credential
// surface over the settings store's tenant tables (the same database the
// agent build opened, so tenant data and credentials live next to
// settings).
func Authn(st *settings.Store) (authn.Provider, server.NativeAuth, error) {
	if st == nil {
		return nil, nil, fmt.Errorf("api-key auth requires a settings database (set --db; it cannot be \"none\")")
	}
	ts, err := tenant.NewStore(st.DB())
	if err != nil {
		return nil, nil, fmt.Errorf("api-key auth: %w", err)
	}
	keys := tenant.NewAPIKeyProvider(ts)
	return keys, nativeauth.New(ts, keys), nil
}

// Plane builds the run-dispatch message plane reading its configuration
// live from the settings store, wrapped with the config-plane waiter so
// settings-generation pushes become immediate rebuild signals (design
// §16.3/§16.7). Console edits (mode / NATS URL / subject) apply to the
// very next dispatch without a restart.
func Plane(st *settings.Store, log zerolog.Logger) server.MsgPlane {
	p := msgplane.New(func() msgplane.Config {
		c := st.Load()
		return msgplane.Config{Mode: c.MsgPlaneMode, NATSURL: c.MsgPlaneNATSURL, Subject: c.MsgPlaneSubject}
	}, log)
	return &planeWithWaiter{plane: p, waiter: msgplane.NewWaiter(p)}
}

// planeWithWaiter adapts the message plane and its waiter to the
// server.MsgPlane seam: Notify overlays the PG queue, Wake surfaces the
// waiter's config-push channel, Close tears both down.
type planeWithWaiter struct {
	plane  *msgplane.Plane
	waiter *msgplane.Waiter
}

func (w *planeWithWaiter) Notify(runID string)   { w.plane.Notify(runID) }
func (w *planeWithWaiter) Wake() <-chan struct{} { return w.waiter.Wake() }
func (w *planeWithWaiter) Close() {
	w.waiter.Close()
	w.plane.Close()
}

// compile-time check: the adapter satisfies the server seam.
var _ server.MsgPlane = (*planeWithWaiter)(nil)
