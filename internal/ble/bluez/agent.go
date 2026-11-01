//go:build linux

package bluez

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	ifAgent        = "org.bluez.Agent1"
	ifAgentManager = "org.bluez.AgentManager1"
	agentPath      = dbus.ObjectPath("/btproxy/agent")

	pairTimeout = 30 * time.Second
)

// RegisterAgent registers a "Just Works" pairing agent (NoInputNoOutput) and asks BlueZ to make it the default.
// Pairing works without an agent too, but BlueZ then refuses requests that need a confirmation. If another
// program holds the default agent, ours stays registered and still serves the pairings the proxy starts.
func (a *Adapter) RegisterAgent(log *slog.Logger) error {
	ag := &agent{a: a, log: log}
	// A method table rather than Export: Export looks methods up by reflection, which stops the linker from
	// dropping unused methods anywhere in the program and adds about 700 KB to the binary.
	methods := map[string]any{
		"Release": ag.Release, "RequestPinCode": ag.RequestPinCode, "DisplayPinCode": ag.DisplayPinCode,
		"RequestPasskey": ag.RequestPasskey, "DisplayPasskey": ag.DisplayPasskey,
		"RequestConfirmation": ag.RequestConfirmation, "RequestAuthorization": ag.RequestAuthorization,
		"AuthorizeService": ag.AuthorizeService, "Cancel": ag.Cancel,
	}
	if err := a.bus.ExportMethodTable(methods, agentPath, ifAgent); err != nil {
		return fmt.Errorf("export agent: %w", err)
	}
	mgr := a.obj("/org/bluez")
	if err := mgr.Call(ifAgentManager+".RegisterAgent", 0, agentPath, "NoInputNoOutput").Err; err != nil {
		a.bus.ExportMethodTable(nil, agentPath, ifAgent)
		return fmt.Errorf("register agent: %w", err)
	}
	if err := mgr.Call(ifAgentManager+".RequestDefaultAgent", 0, agentPath).Err; err != nil {
		log.Warn("pairing agent registered, but not as the default agent", "err", err)
		return nil
	}
	log.Info("pairing agent registered")
	return nil
}

// agent implements org.bluez.Agent1. It answers only for devices connected through the proxy, so a default
// agent that accepts everything does not let any device in range pair with the host.
type agent struct {
	a   *Adapter
	log *slog.Logger
}

var errRejected = dbus.NewError("org.bluez.Error.Rejected", nil)

func (g *agent) allow(dev dbus.ObjectPath, what string) *dbus.Error {
	g.a.mu.Lock()
	_, ours := g.a.conns[dev]
	g.a.mu.Unlock()
	if !ours {
		g.log.Warn("agent: rejected, device not connected through the proxy", "request", what, "device", dev)
		return errRejected
	}
	g.log.Info("agent: accepted", "request", what, "device", dev)
	return nil
}

func (g *agent) Release() *dbus.Error {
	g.log.Info("agent: released by bluetoothd")
	return nil
}

func (g *agent) RequestPinCode(dev dbus.ObjectPath) (string, *dbus.Error) {
	g.log.Warn("agent: PIN code requested, which a NoInputNoOutput agent cannot give", "device", dev)
	return "", errRejected
}

func (g *agent) DisplayPinCode(dev dbus.ObjectPath, pin string) *dbus.Error {
	g.log.Info("agent: PIN code", "device", dev, "pin", pin)
	return nil
}

func (g *agent) RequestPasskey(dev dbus.ObjectPath) (uint32, *dbus.Error) {
	g.log.Warn("agent: passkey requested, which a NoInputNoOutput agent cannot give", "device", dev)
	return 0, errRejected
}

func (g *agent) DisplayPasskey(dev dbus.ObjectPath, passkey uint32, entered uint16) *dbus.Error {
	g.log.Info("agent: passkey", "device", dev, "passkey", fmt.Sprintf("%06d", passkey), "entered", entered)
	return nil
}

func (g *agent) RequestConfirmation(dev dbus.ObjectPath, passkey uint32) *dbus.Error {
	return g.allow(dev, "confirmation")
}

func (g *agent) RequestAuthorization(dev dbus.ObjectPath) *dbus.Error {
	return g.allow(dev, "authorization")
}

func (g *agent) AuthorizeService(dev dbus.ObjectPath, uuid string) *dbus.Error {
	return g.allow(dev, "service "+uuid)
}

func (g *agent) Cancel() *dbus.Error {
	g.log.Info("agent: request cancelled")
	return nil
}

// Pair bonds with the connected device through Device1.Pair. A device that is already paired is reported as
// paired without pairing again, as ESPHome does.
func (c *conn) Pair() error {
	if v, err := c.a.obj(c.path).GetProperty(ifDevice + ".Paired"); err == nil && v.Value() == true {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairTimeout)
	defer cancel()
	err := c.a.obj(c.path).CallWithContext(ctx, ifDevice+".Pair", 0).Err
	var de dbus.Error
	if errors.As(err, &de) && de.Name == "org.bluez.Error.AlreadyExists" {
		return nil
	}
	if ctx.Err() != nil {
		c.a.obj(c.path).Call(ifDevice+".CancelPairing", 0)
	}
	return err
}

// Unpair removes the device from BlueZ, which deletes its bond and drops any connection to it: BlueZ has no
// call that removes only the bond. A device BlueZ does not know has no bond to remove.
func (a *Adapter) Unpair(addr uint64) error {
	err := a.obj(a.path).Call(ifAdapter+".RemoveDevice", 0, a.devicePath(addr)).Err
	var de dbus.Error
	if errors.As(err, &de) && de.Name == "org.bluez.Error.DoesNotExist" {
		return nil
	}
	return err
}
