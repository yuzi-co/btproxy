//go:build linux

package bluez

import (
	"errors"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// Many connects fail within half a second with HCI reason 0x3e (connection failed to be established): the
// controller sent LE Create Connection, but the first connection events did not get through. BlueZ reports
// that as "le-connection-abort-by-local", and sometimes as an error without a message. A second try a moment
// later often succeeds, so Connect retries these, with scanning still paused, before failing. Timeouts and
// every other error fail at once.

const (
	retryDelay  = 200 * time.Millisecond
	retryFast   = 3 * time.Second // a radio failure comes within about 0.5 s; slower ones are not retried
	retryBudget = 5 * time.Second // leaves most of the time a client gives a connect to the last attempt
)

func connectRetry(retries int) ble.ConnectRetry {
	return ble.ConnectRetry{Retries: max(retries, 0), Delay: retryDelay, Fast: retryFast, Budget: retryBudget}
}

// radioFailure tells whether a connect error is one the radio may not repeat: BlueZ's
// "le-connection-abort-by-local", or a BlueZ error with an empty message (not "does not exist"). BlueZ 5.66
// names the empty one org.bluez.Failed, without the usual ".Error".
func radioFailure(err error) bool {
	var de dbus.Error
	if !errors.As(err, &de) || len(de.Body) == 0 {
		return false
	}
	msg, ok := de.Body[0].(string)
	return ok && (msg == "le-connection-abort-by-local" ||
		msg == "" && (de.Name == "org.bluez.Failed" || strings.HasPrefix(de.Name, "org.bluez.Error.")) &&
			de.Name != "org.bluez.Error.DoesNotExist")
}
