//go:build linux

package bluez

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// Many connects fail within half a second with HCI reason 0x3e (connection failed to be established): the
// controller sent LE Create Connection, but the first connection events did not get through. BlueZ reports
// that as "le-connection-abort-by-local", and sometimes as an error without a message. A second try a moment
// later often succeeds, so Connect retries these, with scanning still paused, before failing. A link can also
// come up and drop a few seconds later, before the services are resolved, which on a router next to its Wi-Fi
// radio is as common. Connect retries that too, however long it took. Timeouts and every other error fail at once.
// The wait before a retry comes from Options.ConnectRetryDelay and doubles with each retry (see ble.ConnectRetry).

// retryFast is ble.ConnectRetry.Fast here: a radio failure comes within about 0.5 s; slower ones are not retried.
const retryFast = 3 * time.Second

// errLinkLost is ble.ErrLinkLost for a link that dropped before its services were resolved.
var errLinkLost = fmt.Errorf("%w while resolving services", ble.ErrLinkLost)

// retryable tells whether a connect error is worth another attempt: a radio failure, or a link that came up
// and dropped (errLinkLost). Everything else, timeouts included, fails at once. Connect asks this through
// ble.ConnectRetry, which holds a radio failure to retryFast and a lost link to the budget.
func retryable(err error) bool { return errors.Is(err, ble.ErrLinkLost) || radioFailure(err) }

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
