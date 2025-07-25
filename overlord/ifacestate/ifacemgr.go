// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016-2024 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package ifacestate

import (
	"fmt"
	"sync"
	"time"

	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/backends"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/ifacestate/apparmorprompting"
	"github.com/snapcore/snapd/overlord/ifacestate/ifacerepo"
	"github.com/snapcore/snapd/overlord/ifacestate/udevmonitor"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/overlord/swfeats"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snapdenv"
	"github.com/snapcore/snapd/timings"
)

type deviceData struct {
	ifaceName  string
	hotplugKey snap.HotplugKey
}

// InterfaceManager is responsible for the maintenance of interfaces in
// the system state.  It maintains interface connections, and also observes
// installed snaps to track the current set of available plugs and slots.
type InterfaceManager struct {
	state *state.State
	repo  *interfaces.Repository

	udevMonMu           sync.Mutex
	udevMon             udevmonitor.Interface
	udevRetryTimeout    time.Time
	udevMonitorDisabled bool
	// indexed by interface name and device key. Reset to nil when enumeration is done.
	enumeratedDeviceKeys map[string]map[snap.HotplugKey]bool
	enumerationDone      bool
	// maps sysfs path -> [(interface name, device key)...]
	hotplugDevicePaths map[string][]deviceData

	// extras
	extraInterfaces []interfaces.Interface
	extraBackends   []interfaces.SecurityBackend

	// AppArmor Prompting
	useAppArmorPrompting        bool
	interfacesRequestsManagerMu sync.Mutex
	interfacesRequestsManager   *apparmorprompting.InterfacesRequestsManager

	preseed bool
}

// Manager returns a new InterfaceManager.
// Extra interfaces can be provided for testing.
func Manager(s *state.State, hookManager *hookstate.HookManager, runner *state.TaskRunner, extraInterfaces []interfaces.Interface, extraBackends []interfaces.SecurityBackend) (*InterfaceManager, error) {
	delayedCrossMgrInit()

	// NOTE: hookManager is nil only when testing.
	if hookManager != nil {
		setupHooks(hookManager)
	}

	// Leave udevRetryTimeout at the default value, so that udev is initialized on first Ensure run.
	m := &InterfaceManager{
		state: s,
		repo:  interfaces.NewRepository(),
		// note: enumeratedDeviceKeys is reset to nil when enumeration is done
		enumeratedDeviceKeys: make(map[string]map[snap.HotplugKey]bool),
		hotplugDevicePaths:   make(map[string][]deviceData),
		// extras
		extraInterfaces: extraInterfaces,
		extraBackends:   extraBackends,
		preseed:         snapdenv.Preseeding(),
	}

	exclusiveTaskKinds := map[string]bool{}
	addHandler := func(kind string, do, undo state.HandlerFunc) {
		exclusiveTaskKinds[kind] = true
		runner.AddHandler(kind, do, undo)
	}

	addHandler("connect", m.doConnect, m.undoConnect)
	addHandler("disconnect", m.doDisconnect, m.undoDisconnect)
	addHandler("setup-profiles", m.doSetupProfiles, m.undoSetupProfiles)
	addHandler("remove-profiles", m.doRemoveProfiles, m.doSetupProfiles)
	addHandler("discard-conns", m.doDiscardConns, m.undoDiscardConns)
	addHandler("auto-connect", m.doAutoConnect, m.undoAutoConnect)
	addHandler("auto-disconnect", m.doAutoDisconnect, nil)
	addHandler("hotplug-add-slot", m.doHotplugAddSlot, nil)
	addHandler("hotplug-connect", m.doHotplugConnect, nil)
	addHandler("hotplug-update-slot", m.doHotplugUpdateSlot, nil)
	addHandler("hotplug-remove-slot", m.doHotplugRemoveSlot, nil)
	addHandler("hotplug-disconnect", m.doHotplugDisconnect, nil)
	addHandler("regenerate-security-profiles", m.doRegenerateAllSecurityProfiles, nil)
	// Explicitly add "mark-preseeded" as a task which cannot run in parallel
	// with other ifacestate tasks, as they and mark-preeseded may touch or
	// modify system-key
	exclusiveTaskKinds["mark-preseeded"] = true

	// don't block on hotplug-seq-wait task
	runner.AddHandler("hotplug-seq-wait", m.doHotplugSeqWait, nil)

	// helper for ubuntu-core -> core
	addHandler("transition-ubuntu-core", m.doTransitionUbuntuCore, m.undoTransitionUbuntuCore)

	// interface tasks might touch more than the immediate task target snap, serialize them
	runner.AddBlocked(func(t *state.Task, running []*state.Task) bool {
		if !exclusiveTaskKinds[t.Kind()] {
			return false
		}

		for _, t := range running {
			if exclusiveTaskKinds[t.Kind()] {
				return true
			}
		}

		return false
	})

	return m, nil
}

// AppArmorPromptingRunning returns true if prompting is running.
// This method may only be called after StartUp has been called on the manager.
func (m *InterfaceManager) AppArmorPromptingRunning() bool {
	return m.useAppArmorPrompting
}

// Allow m.UseAppArmorPrompting to be mocked in tests
var assessAppArmorPrompting = (*InterfaceManager).assessAppArmorPrompting

// InterfacesRequestsManager returns the interfaces requests manager associated
// with the receiver. This method may only be called after StartUp has been
// called, and will return nil if AppArmor prompting is not running.
func (m *InterfaceManager) InterfacesRequestsManager() apparmorprompting.Manager {
	irm := m.interfacesRequestsManager
	if irm == nil {
		return nil
	}
	return irm
}

// StartUp implements StateStarterUp.Startup.
func (m *InterfaceManager) StartUp() error {
	s := m.state
	perfTimings := timings.New(map[string]string{"startup": "ifacemgr"})

	s.Lock()
	defer s.Unlock()

	// Check whether AppArmor prompting is supported and enabled. It is fine to
	// do this once, as toggling the feature imposes a restart of snapd.
	if assessAppArmorPrompting(m) {
		m.useAppArmorPrompting = true
	}

	appSets, err := snapsWithSecurityProfiles(m.state)
	if err != nil {
		return err
	}
	// Before deciding about adding implicit slots to any snap we need to scan
	// the set of snaps we know about. If any of those is "snapd" then for the
	// duration of this process always add implicit slots to snapd and not to
	// any other type: os snap and use a mapper to use names core-snapd-system
	// on state, in memory and in API responses, respectively.
	m.selectInterfaceMapper(appSets)

	if err := m.addInterfaces(m.extraInterfaces); err != nil {
		return err
	}
	if err := m.addBackends(m.extraBackends); err != nil {
		return err
	}
	if err := m.addAppSets(appSets); err != nil {
		return err
	}
	if err := m.renameCorePlugConnection(); err != nil {
		return err
	}
	if err := removeStaleConnections(m.state); err != nil {
		return err
	}
	if _, err := m.reloadConnections(""); err != nil {
		return err
	}

	if m.useAppArmorPrompting {
		// Check if there is at least one snap on the system which has a
		// connection using the "snap-interfaces-requests-control" plug
		// with a "handler-service" attribute declared.
		present, err := interfacesRequestsControlHandlerServicePresent(m)
		if err != nil {
			// Internal error, should not occur
			logger.Noticef("failed to check the presence of a interfaces-requests-control handler service: %v", err)
		} else if !present {
			m.state.AddWarning(`"apparmor-prompting" feature flag enabled but no prompting client is present; requests will be auto-denied until a prompting client is installed`, nil)
		}

		// Must not hold state lock while starting interfaces requests
		// manager, so that notices can be recorded if needed.
		m.state.Unlock()
		err = m.initInterfacesRequestsManager()
		m.state.Lock()
		if err != nil {
			logger.Noticef("failed to start interfaces requests manager: %v", err)
			// Set m.useAppArmorPrompting to false so external callers
			// don't try to access nil backends.
			m.useAppArmorPrompting = false
			// This is done before profilesNeedRegeneration, so profiles
			// will only be regenerated if prompting is newly enabled and
			// the backends were successfully created.

			// Do not set "apparmor-prompting" flag to false, since the
			// user intends for prompting to be enabled, but do record a
			// warning so the user knows prompting is not current running.
			m.state.AddWarning(fmt.Sprintf("cannot start prompting backend: %v; prompting will be inactive until snapd is restarted", err), nil)
		}
	}
	if m.profilesNeedRegeneration() {
		const unlockState = false
		if err := m.regenerateAllSecurityProfiles(perfTimings, unlockState); err != nil {
			return err
		}
	}
	if hasAppArmorBackend(m.repo.Backends()) && snapdAppArmorServiceIsDisabled() {
		s.Warnf(`the snapd.apparmor service is disabled; snap applications will likely not start.
Run "systemctl enable --now snapd.apparmor" to correct this.`)
	}

	ifacerepo.Replace(s, m.repo)

	// wire late profile removal support into snapstate
	snapstate.SecurityProfilesRemoveLate = m.discardSecurityProfilesLate

	perfTimings.Save(s)

	istrings := []string{}
	for _, iface := range m.repo.AllHotplugInterfaces() {
		istrings = append(istrings, fmt.Sprintf("%s", iface))
	}
	if ok := swfeats.AddChangeKindVariants(hotplugAddSlotChangeKind, istrings); !ok {
		logger.Trace("could not add possible values for change", "change", hotplugAddSlotChangeKind)
	}
	if ok := swfeats.AddChangeKindVariants(hotplugRemoveChangeKind, istrings); !ok {
		logger.Trace("could not add possible values for change", "change", hotplugRemoveChangeKind)
	}

	return nil
}

// Ensure implements StateManager.Ensure.
func (m *InterfaceManager) Ensure() error {
	// do not worry about udev monitor in preseeding mode
	if m.preseed {
		return nil
	}

	if m.udevMonitorDisabled {
		return nil
	}
	m.udevMonMu.Lock()
	udevMon := m.udevMon
	m.udevMonMu.Unlock()
	if udevMon != nil {
		return nil
	}

	// don't initialize udev monitor until we have a system snap so that we
	// can attach hotplug interfaces to it.
	if !checkSystemSnapIsPresent(m.state) {
		return nil
	}

	// retry udev monitor initialization every 5 minutes
	now := time.Now()
	if now.After(m.udevRetryTimeout) {
		err := m.initUDevMonitor()
		if err != nil {
			m.udevRetryTimeout = now.Add(udevInitRetryTimeout)
		}
		return err
	}
	return nil
}

// Stop implements StateStopper. It stops the udev monitor and prompting,
// if running.
func (m *InterfaceManager) Stop() {
	m.stopUDevMon()
	// The state lock is not held when calling any of the manager methods
	// driven by StateEngine. Thus, it is okay for stopInterfacesRequestsManager
	// to acquire the state lock in order to record notices, if needed.
	m.stopInterfacesRequestsManager()
}

func (m *InterfaceManager) stopUDevMon() {
	m.udevMonMu.Lock()
	udevMon := m.udevMon
	m.udevMonMu.Unlock()
	if udevMon == nil {
		return
	}
	if err := udevMon.Stop(); err != nil {
		logger.Noticef("Cannot stop udev monitor: %s", err)
	}
	m.udevMonMu.Lock()
	defer m.udevMonMu.Unlock()
	m.udevMon = nil
}

// interfacesRequestsManagerStop calls stop on the given manager. The state lock
// must not be held while this function is called, as the manager may need to
// record notices while it is stopping.
var interfacesRequestsManagerStop = func(interfacesRequestsManager *apparmorprompting.InterfacesRequestsManager) error {
	return interfacesRequestsManager.Stop()
}

func (m *InterfaceManager) stopInterfacesRequestsManager() {
	m.interfacesRequestsManagerMu.Lock()
	defer m.interfacesRequestsManagerMu.Unlock()
	// May as well hold the interfacesRequestsManager lock while stopping prompting, so that
	// we don't try to use or overwrite this prompting instance while it is
	// stopping.
	interfacesRequestsManager := m.interfacesRequestsManager
	m.interfacesRequestsManager = nil
	if interfacesRequestsManager == nil {
		return
	}
	if err := interfacesRequestsManagerStop(interfacesRequestsManager); err != nil {
		logger.Noticef("Cannot stop prompting: %s", err)
	}
}

// Repository returns the interface repository used internally by the manager.
//
// This method has two use-cases:
// - it is needed for setting up state in daemon tests
// - it is needed to return the set of known interfaces in the daemon api
//
// In the second case it is only informational and repository has internal
// locks to ensure consistency.
func (m *InterfaceManager) Repository() *interfaces.Repository {
	return m.repo
}

type ConnectionState struct {
	// Auto indicates whether the connection was established automatically
	Auto bool
	// ByGadget indicates whether the connection was trigged by the gadget
	ByGadget bool
	// Interface name of the connection
	Interface string
	// Undesired indicates whether the connection, otherwise established
	// automatically, was explicitly disconnected
	Undesired        bool
	StaticPlugAttrs  map[string]any
	DynamicPlugAttrs map[string]any
	StaticSlotAttrs  map[string]any
	DynamicSlotAttrs map[string]any
	HotplugGone      bool
}

// Active returns true if connection is not undesired and not removed by
// hotplug.
func (c ConnectionState) Active() bool {
	return !(c.Undesired || c.HotplugGone)
}

// ConnectionStates return the state of connections stored in the state.
// Note that this includes inactive connections (i.e. referring to non-
// existing plug/slots), so this map must be cross-referenced with current
// snap info if needed.
// The state must be locked by the caller.
func ConnectionStates(st *state.State) (connStateByRef map[string]ConnectionState, err error) {
	states, err := getConns(st)
	if err != nil {
		return nil, err
	}

	connStateByRef = make(map[string]ConnectionState, len(states))
	for cref, cstate := range states {
		connStateByRef[cref] = ConnectionState{
			Auto:             cstate.Auto,
			ByGadget:         cstate.ByGadget,
			Interface:        cstate.Interface,
			Undesired:        cstate.Undesired,
			StaticPlugAttrs:  cstate.StaticPlugAttrs,
			DynamicPlugAttrs: cstate.DynamicPlugAttrs,
			StaticSlotAttrs:  cstate.StaticSlotAttrs,
			DynamicSlotAttrs: cstate.DynamicSlotAttrs,
			HotplugGone:      cstate.HotplugGone,
		}
	}
	return connStateByRef, nil
}

// ConnectionStates return the state of connections tracked by the manager
func (m *InterfaceManager) ConnectionStates() (connStateByRef map[string]ConnectionState, err error) {
	m.state.Lock()
	defer m.state.Unlock()

	return ConnectionStates(m.state)
}

// ResolveDisconnect resolves potentially missing plug or slot names and
// returns a list of fully populated connection references that can be
// disconnected.
//
// It can be used in two different ways:
// 1: snap disconnect <snap>:<plug> <snap>:<slot>
// 2: snap disconnect <snap>:<plug or slot>
//
// In the first case the referenced plug and slot must be connected.  In the
// second case any matching connection are returned but it is not an error if
// there are no connections.
//
// In both cases the snap name can be omitted to implicitly refer to the core
// snap. If there's no core snap it is simply assumed to be called "core" to
// provide consistent error messages.
func (m *InterfaceManager) ResolveDisconnect(plugSnapName, plugName, slotSnapName, slotName string, forget bool) ([]*interfaces.ConnRef, error) {
	var connected func(plugSn, plug, slotSn, slot string) (bool, error)
	var connectedPlugOrSlot func(snapName, plugOrSlotName string) ([]*interfaces.ConnRef, error)

	if forget {
		conns, err := getConns(m.state)
		if err != nil {
			return nil, err
		}
		connected = func(plugSn, plug, slotSn, slot string) (bool, error) {
			cref := interfaces.ConnRef{
				PlugRef: interfaces.PlugRef{Snap: plugSn, Name: plug},
				SlotRef: interfaces.SlotRef{Snap: slotSn, Name: slot},
			}
			_, ok := conns[cref.ID()]
			return ok, nil
		}

		connectedPlugOrSlot = func(snapName, plugOrSlotName string) ([]*interfaces.ConnRef, error) {
			var refs []*interfaces.ConnRef
			for connID := range conns {
				cref, err := interfaces.ParseConnRef(connID)
				if err != nil {
					return nil, err
				}
				if cref.PlugRef.Snap == snapName && cref.PlugRef.Name == plugOrSlotName {
					refs = append(refs, cref)
				}
				if cref.SlotRef.Snap == snapName && cref.SlotRef.Name == plugOrSlotName {
					refs = append(refs, cref)
				}
			}
			return refs, nil
		}
	} else {
		connected = func(plugSn, plug, slotSn, slot string) (bool, error) {
			_, err := m.repo.Connection(&interfaces.ConnRef{
				PlugRef: interfaces.PlugRef{Snap: plugSn, Name: plug},
				SlotRef: interfaces.SlotRef{Snap: slotSn, Name: slot},
			})
			if _, notConnected := err.(*interfaces.NotConnectedError); notConnected {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return true, nil
		}

		connectedPlugOrSlot = func(snapName, plugOrSlotName string) ([]*interfaces.ConnRef, error) {
			return m.repo.Connected(snapName, plugOrSlotName)
		}
	}

	coreSnapName := SystemSnapName()

	// There are two allowed forms (see snap disconnect --help)
	switch {
	// 1: <snap>:<plug> <snap>:<slot>
	// Return exactly one plug/slot or an error if it doesn't exist.
	case plugName != "" && slotName != "":
		// The snap name can be omitted to implicitly refer to the core snap.
		if plugSnapName == "" {
			plugSnapName = coreSnapName
		}
		// The snap name can be omitted to implicitly refer to the core snap.
		if slotSnapName == "" {
			slotSnapName = coreSnapName
		}
		// Ensure that slot and plug are connected
		isConnected, err := connected(plugSnapName, plugName, slotSnapName, slotName)
		if err != nil {
			return nil, err
		}
		if !isConnected {
			if forget {
				return nil, fmt.Errorf("cannot forget connection %s:%s from %s:%s, it was not connected",
					plugSnapName, plugName, slotSnapName, slotName)
			}
			return nil, fmt.Errorf("cannot disconnect %s:%s from %s:%s, it is not connected",
				plugSnapName, plugName, slotSnapName, slotName)
		}
		return []*interfaces.ConnRef{
			{
				PlugRef: interfaces.PlugRef{Snap: plugSnapName, Name: plugName},
				SlotRef: interfaces.SlotRef{Snap: slotSnapName, Name: slotName},
			}}, nil
	// 2: <snap>:<plug or slot> (through 1st pair)
	// Return a list of connections involving specified plug or slot.
	case plugName != "" && slotName == "" && slotSnapName == "":
		// The snap name can be omitted to implicitly refer to the core snap.
		if plugSnapName == "" {
			plugSnapName = coreSnapName
		}
		return connectedPlugOrSlot(plugSnapName, plugName)
	// 2: <snap>:<plug or slot> (through 2nd pair)
	// Return a list of connections involving specified plug or slot.
	case plugSnapName == "" && plugName == "" && slotName != "":
		// The snap name can be omitted to implicitly refer to the core snap.
		if slotSnapName == "" {
			slotSnapName = coreSnapName
		}
		return connectedPlugOrSlot(slotSnapName, slotName)
	default:
		return nil, fmt.Errorf("allowed forms are <snap>:<plug> <snap>:<slot> or <snap>:<plug or slot>")
	}
}

// DisableUDevMonitor disables the instantiation of udev monitor, but has no effect
// if udev is already created; it should be called after creating InterfaceManager, before
// first Ensure.
// This method is meant for tests only.
func (m *InterfaceManager) DisableUDevMonitor() {
	if m.udevMon != nil {
		logger.Noticef("UDev Monitor already created, cannot be disabled")
		return
	}
	m.udevMonitorDisabled = true
}

var (
	udevInitRetryTimeout            = time.Minute * 5
	createUDevMonitor               = udevmonitor.New
	createInterfacesRequestsManager = apparmorprompting.New
)

func (m *InterfaceManager) initUDevMonitor() error {
	mon := createUDevMonitor(m.hotplugDeviceAdded, m.hotplugDeviceRemoved, m.hotplugEnumerationDone)
	if err := mon.Connect(); err != nil {
		return err
	}
	if err := mon.Run(); err != nil {
		return err
	}

	m.udevMonMu.Lock()
	defer m.udevMonMu.Unlock()
	m.udevMon = mon
	return nil
}

// interfacesRequestsControlHandlerServicePresent returns true if there is at
// least one snap which has a "snap-interfaces-requests-control" connection
// with an app declared by the "handler-service" attribute.
//
// The caller must ensure that the state lock is held.
var interfacesRequestsControlHandlerServicePresent = func(m *InterfaceManager) (bool, error) {
	handlers, err := InterfacesRequestsControlHandlerServices(m.state)
	if err != nil {
		return false, err
	}
	return len(handlers) > 0, nil
}

// initInterfacesRequestsManager initializes the prompting backends which make
// up the interfaces requests manager.
//
// This function should only be called if prompting is supported and enabled,
// and at least one installed snap has a "snap-interfaces-requests-control"
// connection with the "handler-service" attribute declared.
//
// The state lock must not be held when this function is called, so that
// notices can be recorded if necessary.
func (m *InterfaceManager) initInterfacesRequestsManager() error {
	m.interfacesRequestsManagerMu.Lock()
	defer m.interfacesRequestsManagerMu.Unlock()
	interfacesRequestsManager, err := createInterfacesRequestsManager(m.state)
	if err != nil {
		return err
	}
	m.interfacesRequestsManager = interfacesRequestsManager
	return nil
}

var securityBackendsOverride []interfaces.SecurityBackend

// allSecurityBackends returns a set of the available security backends or the mocked ones, ready to be initialized.
func allSecurityBackends() []interfaces.SecurityBackend {
	if securityBackendsOverride != nil {
		return securityBackendsOverride
	}
	return backends.All()
}

// MockSecurityBackends mocks the list of security backends that are used for setting up security.
//
// This function is public because it is referenced in the daemon
func MockSecurityBackends(be []interfaces.SecurityBackend) func() {
	if be == nil {
		// nil is a marker, use an empty slice instead
		be = []interfaces.SecurityBackend{}
	}
	old := securityBackendsOverride
	securityBackendsOverride = be
	return func() { securityBackendsOverride = old }
}

// MockObservedDevicePath adds the given device to the map of observed devices.
// This function is used for tests only.
func (m *InterfaceManager) MockObservedDevicePath(devPath, ifaceName string, hotplugKey snap.HotplugKey) func() {
	old := m.hotplugDevicePaths
	m.hotplugDevicePaths[devPath] = append(m.hotplugDevicePaths[devPath], deviceData{hotplugKey: hotplugKey, ifaceName: ifaceName})
	return func() { m.hotplugDevicePaths = old }
}
