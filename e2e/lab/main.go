// The e2e lab boots real woca backends on fixed loopback ports for the
// Playwright suite (e2e/). One process hosts one topology:
//
//	enterprise — the full private-cloud chain in one process: a fake
//	  private core (real entrygw/tenant/authcore/nativeauth stores on a
//	  temp sqlite DB), the real edge handler on the public port, and a
//	  real bridge (the exact servefront edition cmd/woca-bridge serves)
//	  enrolled over the real /gw/enroll flow, apikey posture, tunnel
//	  open. Forms 2 (direct bridge web) and 4 (browser via edge) both
//	  test against it, with real logins validated at the core.
//	static — the built webdist served plain, for the native-wizard form
//	  (tests inject the window.go shim; no backend is involved).
//	relay — the personal remote-relay chain: a real woca-relay (keystore,
//	  auth, tunnel, SPA, revocation sweep) plus the real personal serve
//	  behind it, connected the way `woca relay connect` persists
//	  coordinates. The in-process twin of ci/e2e-relay.sh, with an extra
//	  pre-revoked key so the login gate's revoked-notice path is testable.
//
// The personal form runs the real `woca serve` binary directly (Playwright
// webServer), not this lab.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	topology := flag.String("topology", "", "enterprise | static")
	flag.Parse()
	switch *topology {
	case "enterprise":
		runEnterprise()
	case "static":
		runStatic()
	case "relay":
		runRelay()
	default:
		fmt.Fprintln(os.Stderr, "-topology is required (enterprise|static|relay)")
		os.Exit(2)
	}
}
