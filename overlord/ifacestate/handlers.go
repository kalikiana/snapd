// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016 Canonical Ltd
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
	"sort"

	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/policy"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/assertstate"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/snap"
)

func (m *InterfaceManager) setupAffectedSnaps(task *state.Task, affectingSnap string, affectedSnaps []string) error {
	st := task.State()

	// Setup security of the affected snaps.
	for _, affectedSnapName := range affectedSnaps {
		// the snap that triggered the change needs to be skipped
		if affectedSnapName == affectingSnap {
			continue
		}
		var snapst snapstate.SnapState
		if err := snapstate.Get(st, affectedSnapName, &snapst); err != nil {
			return err
		}
		affectedSnapInfo, err := snapst.CurrentInfo()
		if err != nil {
			return err
		}
		snap.AddImplicitSlots(affectedSnapInfo)
		if err := setupSnapSecurity(task, affectedSnapInfo, snapst.DevModeAllowed(), m.repo); err != nil {
			return err
		}
	}
	return nil
}

func (m *InterfaceManager) doSetupProfiles(task *state.Task, _ *tomb.Tomb) error {
	task.State().Lock()
	defer task.State().Unlock()

	// Get snap.Info from bits handed by the snap manager.
	ss, err := snapstate.TaskSnapSetup(task)
	if err != nil {
		return err
	}

	snapInfo, err := snap.ReadInfo(ss.Name(), ss.SideInfo)
	if err != nil {
		return err
	}
	snap.AddImplicitSlots(snapInfo)
	snapName := snapInfo.Name()

	// The snap may have been updated so perform the following operation to
	// ensure that we are always working on the correct state:
	//
	// - disconnect all connections to/from the given snap
	//   - remembering the snaps that were affected by this operation
	// - remove the (old) snap from the interfaces repository
	// - add the (new) snap to the interfaces repository
	// - restore connections based on what is kept in the state
	//   - if a connection cannot be restored then remove it from the state
	// - setup the security of all the affected snaps
	affectedSnaps, err := m.repo.DisconnectSnap(snapName)
	if err != nil {
		return err
	}
	// XXX: what about snap renames? We should remove the old name (or switch
	// to IDs in the interfaces repository)
	if err := m.repo.RemoveSnap(snapName); err != nil {
		return err
	}
	if err := m.repo.AddSnap(snapInfo); err != nil {
		if _, ok := err.(*interfaces.BadInterfacesError); ok {
			logger.Noticef("%s", err)
		} else {
			return err
		}
	}
	if err := m.reloadConnections(snapName); err != nil {
		return err
	}
	// FIXME: here we should not reconnect auto-connect plug/slot
	// pairs that were explicitly disconnected by the user
	if err := m.autoConnect(task, snapName, nil); err != nil {
		return err
	}
	if err := setupSnapSecurity(task, snapInfo, ss.DevModeAllowed(), m.repo); err != nil {
		return err
	}

	return m.setupAffectedSnaps(task, snapName, affectedSnaps)
}

func (m *InterfaceManager) doRemoveProfiles(task *state.Task, _ *tomb.Tomb) error {
	st := task.State()
	st.Lock()
	defer st.Unlock()

	// Get SnapSetup for this snap. This is gives us the name of the snap.
	snapSetup, err := snapstate.TaskSnapSetup(task)
	if err != nil {
		return err
	}
	snapName := snapSetup.Name()

	// Disconnect the snap entirely.
	// This is required to remove the snap from the interface repository.
	// The returned list of affected snaps will need to have its security setup
	// to reflect the change.
	affectedSnaps, err := m.repo.DisconnectSnap(snapName)
	if err != nil {
		return err
	}
	if err := m.setupAffectedSnaps(task, snapName, affectedSnaps); err != nil {
		return err
	}

	// Remove the snap from the interface repository.
	// This discards all the plugs and slots belonging to that snap.
	if err := m.repo.RemoveSnap(snapName); err != nil {
		return err
	}

	// Remove security artefacts of the snap.
	if err := removeSnapSecurity(task, snapName); err != nil {
		// TODO: how long to wait?
		return &state.Retry{}
	}

	return nil
}

func (m *InterfaceManager) doDiscardConns(task *state.Task, _ *tomb.Tomb) error {
	st := task.State()
	st.Lock()
	defer st.Unlock()

	snapSetup, err := snapstate.TaskSnapSetup(task)
	if err != nil {
		return err
	}

	snapName := snapSetup.Name()

	var snapState snapstate.SnapState
	err = snapstate.Get(st, snapName, &snapState)
	if err != nil && err != state.ErrNoState {
		return err
	}

	if err == nil && len(snapState.Sequence) != 0 {
		return fmt.Errorf("cannot discard connections for snap %q while it is present", snapName)
	}
	conns, err := getConns(st)
	if err != nil {
		return err
	}
	removed := make(map[string]connState)
	for id := range conns {
		plugRef, slotRef, err := parseConnID(id)
		if err != nil {
			return err
		}
		if plugRef.Snap == snapName || slotRef.Snap == snapName {
			removed[id] = conns[id]
			delete(conns, id)
		}
	}
	task.Set("removed", removed)
	setConns(st, conns)
	return nil
}

func (m *InterfaceManager) undoDiscardConns(task *state.Task, _ *tomb.Tomb) error {
	st := task.State()
	st.Lock()
	defer st.Unlock()

	var removed map[string]connState
	err := task.Get("removed", &removed)
	if err != nil && err != state.ErrNoState {
		return err
	}

	conns, err := getConns(st)
	if err != nil {
		return err
	}

	for id, connState := range removed {
		conns[id] = connState
	}
	setConns(st, conns)
	task.Set("removed", nil)
	return nil
}

func (m *InterfaceManager) doConnect(task *state.Task, _ *tomb.Tomb) error {
	st := task.State()
	st.Lock()
	defer st.Unlock()

	plugRef, slotRef, err := getPlugAndSlotRefs(task)
	if err != nil {
		return err
	}

	conns, err := getConns(st)
	if err != nil {
		return err
	}

	connRef, err := m.repo.ResolveConnect(plugRef.Snap, plugRef.Name, slotRef.Snap, slotRef.Name)
	if err != nil {
		return err
	}
	plug := m.repo.Plug(connRef.PlugRef.Snap, connRef.PlugRef.Name)
	if plug == nil {
		return fmt.Errorf("snap %q has no %q plug", connRef.PlugRef.Snap, connRef.PlugRef.Name)
	}
	var plugDecl *asserts.SnapDeclaration
	if plug.Snap.SnapID != "" {
		var err error
		plugDecl, err = assertstate.SnapDeclaration(st, plug.Snap.SnapID)
		if err != nil {
			return fmt.Errorf("cannot find snap declaration for %q: %v", plug.Snap.Name(), err)
		}
	}

	slot := m.repo.Slot(connRef.SlotRef.Snap, connRef.SlotRef.Name)
	if slot == nil {
		return fmt.Errorf("snap %q has no %q slot", connRef.SlotRef.Snap, connRef.SlotRef.Name)
	}
	var slotDecl *asserts.SnapDeclaration
	if slot.Snap.SnapID != "" {
		var err error
		slotDecl, err = assertstate.SnapDeclaration(st, slot.Snap.SnapID)
		if err != nil {
			return fmt.Errorf("cannot find snap declaration for %q: %v", slot.Snap.Name(), err)
		}
	}

	baseDecl, err := assertstate.BaseDeclaration(st)
	if err != nil {
		return fmt.Errorf("internal error: cannot find base declaration: %v", err)
	}

	// check the connection against the declarations' rules
	ic := policy.ConnectCandidate{
		Plug:                plug.PlugInfo,
		PlugSnapDeclaration: plugDecl,
		Slot:                slot.SlotInfo,
		SlotSnapDeclaration: slotDecl,
		BaseDeclaration:     baseDecl,
	}

	err = ic.Check()
	if err != nil {
		return err
	}

	err = m.repo.Connect(connRef)
	if err != nil {
		return err
	}

	var plugSnapst snapstate.SnapState
	if err := snapstate.Get(st, connRef.PlugRef.Snap, &plugSnapst); err != nil {
		return err
	}

	var slotSnapst snapstate.SnapState
	if err := snapstate.Get(st, connRef.SlotRef.Snap, &slotSnapst); err != nil {
		return err
	}

	if err := setupSnapSecurity(task, slot.Snap, slotSnapst.DevModeAllowed(), m.repo); err != nil {
		return err
	}
	if err := setupSnapSecurity(task, plug.Snap, plugSnapst.DevModeAllowed(), m.repo); err != nil {
		return err
	}

	conns[connRef.ID()] = connState{Interface: plug.Interface}
	setConns(st, conns)

	return nil
}

func snapNamesFromConns(conns []interfaces.ConnRef) []string {
	m := make(map[string]bool)
	for _, conn := range conns {
		m[conn.PlugRef.Snap] = true
		m[conn.SlotRef.Snap] = true
	}
	l := make([]string, 0, len(m))
	for name := range m {
		l = append(l, name)
	}
	sort.Strings(l)
	return l
}

func (m *InterfaceManager) doDisconnect(task *state.Task, _ *tomb.Tomb) error {
	st := task.State()
	st.Lock()
	defer st.Unlock()

	plugRef, slotRef, err := getPlugAndSlotRefs(task)
	if err != nil {
		return err
	}

	conns, err := getConns(st)
	if err != nil {
		return err
	}

	var affectedConns []interfaces.ConnRef
	if plugRef.Snap != "" && plugRef.Name != "" && slotRef.Snap != "" && slotRef.Name != "" {
		if err := m.repo.Disconnect(plugRef.Snap, plugRef.Name, slotRef.Snap, slotRef.Name); err != nil {
			return err
		}
		affectedConns = []interfaces.ConnRef{{plugRef, slotRef}}
	} else if plugRef.Name != "" && slotRef.Snap == "" && slotRef.Name == "" {
		// NOTE: plugRef.Snap can be either empty or not, Connected handles both
		affectedConns, err = m.repo.Connected(plugRef.Snap, plugRef.Name)
		if err != nil {
			return err
		}
		m.repo.DisconnectAll(affectedConns)
	} else if plugRef.Snap == "" && plugRef.Name == "" && slotRef.Name != "" {
		// Symmetrically, slotRef.Snap can be either empty or not
		affectedConns, err = m.repo.Connected(slotRef.Snap, slotRef.Name)
		if err != nil {
			return err
		}
		m.repo.DisconnectAll(affectedConns)
	} else {
		return fmt.Errorf("internal error, unhandled disconnect case plug: %q, slot: %q", plugRef, slotRef)
	}
	affectedSnaps := snapNamesFromConns(affectedConns)
	for _, snapName := range affectedSnaps {
		var snapst snapstate.SnapState
		if err := snapstate.Get(st, snapName, &snapst); err != nil {
			return err
		}
		snapInfo, err := snapst.CurrentInfo()
		if err != nil {
			return err
		}
		if err := setupSnapSecurity(task, snapInfo, snapst.DevModeAllowed(), m.repo); err != nil {
			return &state.Retry{}
		}
	}
	for _, conn := range affectedConns {
		delete(conns, conn.ID())
	}

	setConns(st, conns)
	return nil
}
