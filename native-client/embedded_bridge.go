//go:build embeddedbridge

package main

import _ "embed"

//go:embed bridgebin/woca-bridge
var bridgeBinary []byte

// embeddedBridgeBytes returns the bundled woca-bridge (built by
// `make build-server` and copied into bridgebin/ before the native build).
func embeddedBridgeBytes() []byte { return bridgeBinary }
