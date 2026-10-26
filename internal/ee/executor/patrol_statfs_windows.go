//go:build windows

package executor

import "errors"

// statfsUsage is unsupported on windows: the executor's data-plane roles
// are unix deployments (the docker socket contract is unix-native). The
// patrol surfaces the probe failure rather than pretending a watermark.
func statfsUsage(path string) (float64, error) {
	return 0, errors.New("disk watermark probe unsupported on this platform: " + path)
}
