package winrt

import (
	"errors"
	"fmt"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// Windows has no explicit connect: a GATT session that asks to keep the connection makes the stack connect, and
// it keeps trying until the session lets go, so a device that is not there shows up as a slow failure (the
// link does not come up by the deadline), which is not retried. A link that comes up and fails in its first
// connection events, the HCI 0x3e case BlueZ reports as le-connection-abort-by-local, shows up differently:
// the first service discovery answers Unreachable, or the link is down again by the time discovery is done.
// The stack sometimes also fails discovery at once with ERROR_GEN_FAILURE ("A device attached to the system
// is not functioning") or ERROR_DEVICE_NOT_AVAILABLE. Connect frees the device and session objects and tries
// again with fresh ones. Not found, access denied, cancelled operations, ATT errors and timeouts fail at once.
// The wait before a retry comes from Options.ConnectRetryDelay and doubles with each retry (see
// ble.ConnectRetry); Windows takes a moment to drop the failed link, so it should not be much under 500 ms.

const (
	retryFast = 5 * time.Second // lookup, session and link-up come before discovery fails
	// retryBudget is how long after the first attempt a retry may still start. A client gives a connect about
	// 20 s, so 10 s fits the doubling delays of the default retries and leaves the last attempt as much again.
	retryBudget = 10 * time.Second
)

func connectRetry(retries int, delay time.Duration) ble.ConnectRetry {
	return ble.ConnectRetry{Retries: max(retries, 0), Delay: delay, Fast: retryFast, Budget: retryBudget}
}

// errLinkLost is a link that went down between coming up and the end of service discovery.
var errLinkLost = fmt.Errorf("%w while connecting", ble.ErrLinkLost)

// HRESULTs of a GATT call that failed on the radio rather than for good.
const (
	hrGenFailure         = 0x8007001f // ERROR_GEN_FAILURE
	hrDeviceNotAvailable = 0x800710df // ERROR_DEVICE_NOT_AVAILABLE
)

// radioFailure tells whether a failed connect attempt is one the radio may not repeat.
func radioFailure(err error) bool {
	if errors.Is(err, errUnreachable) || errors.Is(err, ble.ErrLinkLost) {
		return true
	}
	var h hresult
	return errors.As(err, &h) && (h.code == hrGenFailure || h.code == hrDeviceNotAvailable)
}
