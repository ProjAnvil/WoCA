//go:build !embeddedbridge

package main

// embeddedBridgeBytes is empty in development builds — the bundled bridge
// binary only exists when `make native-build` compiles with the
// embeddedbridge tag after placing the artifact in bridgebin/.
func embeddedBridgeBytes() []byte { return nil }
