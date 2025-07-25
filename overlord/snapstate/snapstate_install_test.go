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

package snapstate_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	. "gopkg.in/check.v1"
	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/assertstest"
	"github.com/snapcore/snapd/asserts/snapasserts"
	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/client"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/dirs/dirstest"
	"github.com/snapcore/snapd/gadget"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/ifacetest"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord"
	"github.com/snapcore/snapd/overlord/assertstate"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/restart"
	"github.com/snapcore/snapd/snapdenv"
	"github.com/snapcore/snapd/store/storetest"

	// So it registers Configure.
	_ "github.com/snapcore/snapd/overlord/configstate"
	"github.com/snapcore/snapd/overlord/configstate/config"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/ifacestate/ifacerepo"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/snapstate/backend"
	"github.com/snapcore/snapd/overlord/snapstate/sequence"
	"github.com/snapcore/snapd/overlord/snapstate/snapstatetest"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/naming"
	"github.com/snapcore/snapd/snap/snaptest"
	"github.com/snapcore/snapd/store"
	"github.com/snapcore/snapd/testutil"
)

func expectedDoInstallTasks(typ snap.Type, opts, compOpts, discards int, startTasks []string, components []string, filterOut map[string]bool) []string {
	if !release.OnClassic || opts&isHybrid != 0 {
		switch typ {
		case snap.TypeGadget:
			opts |= updatesGadget
		case snap.TypeKernel:
			opts |= updatesGadgetAssets
		case snap.TypeSnapd:
			opts |= updatesBootConfig
		}
	}

	var tasksAfterDownload, tasksBeforePreRefreshHook, tasksAfterLinkSnap, tasksAfterPostOpHook, tasksBeforeDiscard []string
	for range components {
		compOpts |= compOptMultiCompInstall
		if opts&localSnap != 0 {
			compOpts |= compOptIsLocal
		}
		if opts&unlinkBefore != 0 {
			compOpts |= compOptIsActive | compOptDuringSnapRefresh
		}

		beforeMount, beforeLink, link, hooksAndAfter, discard := expectedComponentInstallTasksSplit(compOpts)

		tasksAfterDownload = append(tasksAfterDownload, beforeMount...)
		tasksBeforePreRefreshHook = append(tasksBeforePreRefreshHook, beforeLink...)
		tasksAfterLinkSnap = append(tasksAfterLinkSnap, link...)
		tasksAfterPostOpHook = append(tasksAfterPostOpHook, hooksAndAfter...)
		tasksBeforeDiscard = append(tasksBeforeDiscard, discard...)
	}

	if startTasks == nil {
		switch {
		case opts&localSnap != 0:
			startTasks = []string{
				"prerequisites",
				"prepare-snap",
			}
			startTasks = append(startTasks, tasksAfterDownload...)
			startTasks = append(startTasks, "mount-snap")
		case opts&localRevision != 0:
			startTasks = []string{
				"prerequisites",
				"prepare-snap",
			}
			startTasks = append(startTasks, tasksAfterDownload...)
		default:
			startTasks = []string{
				"prerequisites",
				"download-snap",
				"validate-snap",
			}
			startTasks = append(startTasks, tasksAfterDownload...)
			startTasks = append(startTasks, "mount-snap")
		}
	}
	expected := startTasks

	expected = append(expected, tasksBeforePreRefreshHook...)

	if opts&unlinkBefore != 0 {
		expected = append(expected,
			"run-hook[pre-refresh]",
			"stop-snap-services",
			"remove-aliases",
		)
	}

	if opts&unlinkBefore != 0 {
		expected = append(expected, "unlink-current-snap")
	}
	if opts&updatesGadgetAssets != 0 && opts&needsKernelSetup != 0 {
		expected = append(expected, "prepare-kernel-snap")
	}
	if opts&(updatesGadget|updatesGadgetAssets) != 0 {
		expected = append(expected, "update-gadget-assets")
	}
	if opts&updatesGadget != 0 {
		expected = append(expected, "update-gadget-cmdline")
	}

	expected = append(expected, "copy-snap-data")

	expected = append(expected, "setup-profiles", "link-snap")
	expected = append(expected, tasksAfterLinkSnap...)
	expected = append(expected, "auto-connect")
	expected = append(expected,
		"set-auto-aliases",
		"setup-aliases")
	if opts&preferInstalled != 0 {
		expected = append(expected, "prefer-aliases")
	}
	if opts&updatesBootConfig != 0 {
		expected = append(expected, "update-managed-boot-config")
	}
	if opts&unlinkBefore != 0 {
		expected = append(expected, "run-hook[post-refresh]")
	} else {
		expected = append(expected, "run-hook[install]")
	}
	if opts&updatesGadgetAssets != 0 && opts&needsKernelSetup != 0 {
		expected = append(expected, "discard-old-kernel-snap-setup")
	}

	expected = append(expected, tasksAfterPostOpHook...)

	if opts&unlinkBefore == 0 && opts&(noConfigure|runCoreConfigure) == 0 {
		expected = append(expected, "run-hook[default-configure]")
	}

	// TODO: it seems that removing this line doesn't break any tests
	expected = append(expected, tasksBeforeDiscard...)

	expected = append(expected, "start-snap-services")
	for i := 0; i < discards; i++ {
		expected = append(expected,
			"clear-snap",
			"discard-snap",
		)
	}
	if opts&cleanupAfter != 0 {
		expected = append(expected,
			"cleanup",
		)
	}
	if opts&noConfigure == 0 {
		expected = append(expected,
			"run-hook[configure]",
		)
	}
	expected = append(expected,
		"run-hook[check-health]",
	)

	filtered := make([]string, 0, len(expected))
	for _, k := range expected {
		if !filterOut[k] {
			filtered = append(filtered, k)
		}
	}

	return filtered
}

func verifyInstallTasks(c *C, typ snap.Type, opts, discards int, ts *state.TaskSet) {
	verifyInstallTasksWithComponents(c, typ, opts, discards, nil, ts)
}

func verifyInstallTasksWithComponents(c *C, typ snap.Type, opts, discards int, components []string, ts *state.TaskSet) {
	kinds := taskKinds(ts.Tasks())

	expected := expectedDoInstallTasks(typ, opts, 0, discards, nil, components, nil)
	c.Assert(kinds, DeepEquals, expected)

	if opts&noLastBeforeModificationsEdge == 0 {
		te := ts.MaybeEdge(snapstate.LastBeforeLocalModificationsEdge)
		c.Assert(te, NotNil)

		if len(components) == 0 {
			if opts&localSnap != 0 {
				c.Assert(te.Kind(), Equals, "prepare-snap")
			} else {
				c.Assert(te.Kind(), Equals, "validate-snap")
			}
		} else {
			if opts&compOptIsUnasserted == 0 {
				c.Assert(te.Kind(), Equals, "validate-component")
			} else {
				c.Assert(te.Kind(), Equals, "prepare-component")
			}
		}
	}

	te, err := ts.Edge(snapstate.SnapSetupEdge)
	c.Assert(err, IsNil)
	c.Assert(te.Has("component-setup-tasks"), Equals, true)

	var count int
	for _, t := range ts.Tasks() {
		switch t.Kind() {
		case "prepare-component", "download-component":
			if t.Change() == nil {
				// Add to a change so we can call snapstate.TaskComponentSetup
				chg := t.State().NewChange("install", "install...")
				chg.AddAll(ts)
			}

			compsup, _, err := snapstate.TaskComponentSetup(t)
			c.Assert(err, IsNil)
			c.Check(compsup.CompSideInfo.Component.ComponentName, Equals, components[count])
			count++
		case "setup-profiles":
			c.Assert(t.Has("component-setup-task"), Equals, false)
			c.Assert(t.Has("component-setup"), Equals, false)
		}
	}
	c.Check(count, Equals, len(components), Commentf("expected %d components, found %d", len(components), count))
}

func (s *snapmgrTestSuite) TestInstallDevModeConfinementFiltering(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// if a snap is devmode, you can't install it without --devmode
	opts := &snapstate.RevisionOptions{Channel: "channel-for-devmode"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, `.* requires devmode or confinement override`)

	// if a snap is devmode, you *can* install it with --devmode
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{DevMode: true})
	c.Assert(err, IsNil)

	// if a model assertion says that it's okay to install a snap (via seeding)
	// with devmode then you can install it with --devmode
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{ApplySnapDevMode: true})
	c.Assert(err, IsNil)
	// and with this, snapstate for the install tasks does not have
	// ApplySnapDevMode set, but does have DevMode set now.
	task0 := ts.Tasks()[0]
	snapsup, err := snapstate.TaskSnapSetup(task0)
	c.Assert(err, IsNil, Commentf("%#v", task0))
	c.Assert(snapsup.InstanceName(), Equals, "some-snap")
	c.Assert(snapsup.DevMode, Equals, true)
	c.Assert(snapsup.ApplySnapDevMode, Equals, false)

	// if a snap is *not* devmode, you can still install it with --devmode
	opts.Channel = "channel-for-strict"
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{DevMode: true})
	c.Assert(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallClassicConfinementFiltering(c *C) {
	restore := maybeMockClassicSupport(c)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	// if a snap is classic, you can't install it without --classic
	opts := &snapstate.RevisionOptions{Channel: "channel-for-classic"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, `.* requires classic confinement`)

	// if a snap is classic, you *can* install it with --classic
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{Classic: true})
	c.Assert(err, IsNil)

	// if a snap is *not* classic, but can install it with --classic which gets ignored
	opts.Channel = "channel-for-strict"
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{Classic: true})
	c.Assert(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallTasks(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, 0, snapstate.Flags{})
	c.Assert(err, IsNil)

	verifyInstallTasks(c, snap.TypeApp, 0, 0, ts)
	c.Assert(s.state.TaskCount(), Equals, len(ts.Tasks()))
}

func (s *snapmgrTestSuite) TestInstallAlreadyInstalled(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "some-snap", Revision: snap.R(7)},
		}),
		Current:  snap.R(7),
		SnapType: "app",
	})

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, 0, snapstate.Flags{})
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, `snap "some-snap" is already installed`)
	c.Check(err, FitsTypeOf, &snap.AlreadyInstalledError{})
}

func (s *snapmgrTestSuite) TestInstallInvalidOptions(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	t := snapstate.StoreInstallGoal(snapstate.StoreSnap{
		InstanceName: "some-snap",
		RevOpts: snapstate.RevisionOptions{
			CohortKey: "cohort",
			Revision:  snap.R(7),
		},
	})

	_, _, err := snapstate.InstallOne(context.Background(), s.state, t, snapstate.Options{})
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, `invalid revision options for snap \"some-snap\": cannot specify revision and cohort`)
}

func (s *snapmgrTestSuite) TestInstallTaskEdgesForPreseeding(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
`)
	restorePreseeding := snapdenv.MockPreseeding(true)
	defer restorePreseeding()

	for _, skipConfig := range []bool{false, true} {
		ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(8)}, mockSnap, "", "", snapstate.Flags{SkipConfigure: skipConfig}, nil)
		c.Assert(err, IsNil)

		te, err := ts.Edge(snapstate.BeginEdge)
		c.Assert(err, IsNil)
		c.Check(te.Kind(), Equals, "prerequisites")

		te, err = ts.Edge(snapstate.BeforeHooksEdge)
		c.Assert(err, IsNil)
		c.Check(te.Kind(), Equals, "setup-aliases")

		te, err = ts.Edge(snapstate.HooksEdge)
		c.Assert(err, IsNil)
		c.Assert(te.Kind(), Equals, "run-hook")

		var hsup *hookstate.HookSetup
		c.Assert(te.Get("hook-setup", &hsup), IsNil)
		c.Check(hsup.Hook, Equals, "install")
		c.Check(hsup.Snap, Equals, "some-snap")

		te, err = ts.Edge(snapstate.EndEdge)
		c.Assert(err, IsNil)
		if skipConfig {
			c.Check(te.Kind(), Equals, "start-snap-services")
		} else {
			c.Check(te.Kind(), Equals, "run-hook")
		}
	}
}

func (s *snapmgrTestSuite) TestInstallSnapdSnapTypeOnClassic(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// remove snapd snap added for snapmgrBaseTest
	snapstate.Set(s.state, "snapd", nil)

	// setup a classic model so the device context says we are on classic
	defer snapstatetest.MockDeviceModel(ClassicModel())()

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "snapd", opts, 0, snapstate.Flags{})
	c.Assert(err, IsNil)

	verifyInstallTasks(c, snap.TypeSnapd, noConfigure, 0, ts)

	snapsup, err := snapstate.TaskSnapSetup(ts.Tasks()[0])
	c.Assert(err, IsNil)
	c.Check(snapsup.Type, Equals, snap.TypeSnapd)
}

func (s *snapmgrTestSuite) TestInstallSnapdSnapTypeOnCore(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	// remove snapd snap added for snapmgrBaseTest
	snapstate.Set(s.state, "snapd", nil)

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "snapd", opts, 0, snapstate.Flags{})
	c.Assert(err, IsNil)

	verifyInstallTasks(c, snap.TypeSnapd, noConfigure, 0, ts)

	snapsup, err := snapstate.TaskSnapSetup(ts.Tasks()[0])
	c.Assert(err, IsNil)
	c.Check(snapsup.Type, Equals, snap.TypeSnapd)
}

func (s *snapmgrTestSuite) TestInstallCohortTasks(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	opts := &snapstate.RevisionOptions{Channel: "some-channel", CohortKey: "what"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, 0, snapstate.Flags{})
	c.Assert(err, IsNil)
	snapsup, err := snapstate.TaskSnapSetup(ts.Tasks()[0])
	c.Assert(err, IsNil)
	c.Check(snapsup.CohortKey, Equals, "what")

	verifyInstallTasks(c, snap.TypeApp, 0, 0, ts)
	c.Assert(s.state.TaskCount(), Equals, len(ts.Tasks()))
}

func (s *snapmgrTestSuite) TestInstallPreferTasks(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	flags := snapstate.Flags{Prefer: true}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, 0, flags)
	c.Assert(err, IsNil)

	verifyInstallTasks(c, snap.TypeApp, preferInstalled, 0, ts)
	c.Assert(s.state.TaskCount(), Equals, len(ts.Tasks()))
}

func (s *snapmgrTestSuite) TestInstallWithDeviceContext(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// unset the global store, it will need to come via the device context
	snapstate.ReplaceStore(s.state, nil)

	deviceCtx := &snapstatetest.TrivialDeviceContext{
		CtxStore:    s.fakeStore,
		DeviceModel: &asserts.Model{},
	}

	prqt := new(testPrereqTracker)

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.InstallWithDeviceContext(context.Background(), s.state, "some-snap", opts, 0, snapstate.Flags{}, prqt, deviceCtx, "")
	c.Assert(err, IsNil)

	verifyInstallTasks(c, snap.TypeApp, 0, 0, ts)
	c.Assert(s.state.TaskCount(), Equals, len(ts.Tasks()))

	c.Assert(prqt.infos, HasLen, 1)
	c.Check(prqt.infos[0].SnapName(), Equals, "some-snap")
	c.Check(prqt.missingProviderContentTagsCalls, Equals, 1)
}

func (s *snapmgrTestSuite) TestInstallPathWithDeviceContext(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// unset the global store, it will need to come via the device context
	snapstate.ReplaceStore(s.state, nil)

	deviceCtx := &snapstatetest.TrivialDeviceContext{
		CtxStore:    s.fakeStore,
		DeviceModel: &asserts.Model{},
	}

	si := &snap.SideInfo{RealName: "some-snap", Revision: snap.R(7)}
	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
`)

	prqt := new(testPrereqTracker)

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.InstallPathWithDeviceContext(s.state, si, mockSnap, "some-snap", opts, 0, snapstate.Flags{}, prqt, deviceCtx, "")
	c.Assert(err, IsNil)

	verifyInstallTasks(c, snap.TypeApp, localSnap, 0, ts)
	c.Assert(s.state.TaskCount(), Equals, len(ts.Tasks()))

	c.Assert(prqt.infos, HasLen, 1)
	c.Check(prqt.infos[0].SnapName(), Equals, "some-snap")
	c.Check(prqt.missingProviderContentTagsCalls, Equals, 1)
}

func (s *snapmgrTestSuite) TestInstallPathWithDeviceContextBadFile(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// unset the global store, it will need to come via the device context
	snapstate.ReplaceStore(s.state, nil)

	deviceCtx := &snapstatetest.TrivialDeviceContext{CtxStore: s.fakeStore}

	si := &snap.SideInfo{RealName: "some-snap", Revision: snap.R(7)}
	path := filepath.Join(c.MkDir(), "some-snap_7.snap")
	err := os.WriteFile(path, []byte(""), 0644)
	c.Assert(err, IsNil)

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.InstallPathWithDeviceContext(s.state, si, path, "some-snap", opts, 0, snapstate.Flags{}, nil, deviceCtx, "")
	c.Assert(err, ErrorMatches, `cannot open snap file: cannot process snap or snapdir: cannot read ".*/some-snap_7.snap": EOF`)
	c.Assert(ts, IsNil)
}

func (s *snapmgrTestSuite) TestInstallWithDeviceContextNoRemodelConflict(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	// remove snapd snap added for snapmgrBaseTest
	snapstate.Set(s.state, "snapd", nil)

	// unset the global store, it will need to come via the device context
	snapstate.ReplaceStore(s.state, nil)

	tugc := s.state.NewTask("update-managed-boot-config", "update managed boot config")
	chg := s.state.NewChange("remodel", "remodel")
	chg.AddTask(tugc)

	deviceCtx := &snapstatetest.TrivialDeviceContext{CtxStore: s.fakeStore, DeviceModel: MakeModel20("brand-gadget", nil)}

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.InstallWithDeviceContext(context.Background(), s.state, "brand-gadget", opts, 0, snapstate.Flags{}, nil, deviceCtx, chg.ID())
	c.Assert(err, IsNil)
	verifyInstallTasks(c, snap.TypeGadget, 0, 0, ts)

	ts, err = snapstate.InstallWithDeviceContext(context.Background(), s.state, "snapd", opts, 0, snapstate.Flags{}, nil, deviceCtx, chg.ID())
	c.Assert(err, IsNil)
	verifyInstallTasks(c, snap.TypeSnapd, noConfigure, 0, ts)
}

func (s *snapmgrTestSuite) TestInstallWithDeviceContextRemodelKernel24(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	// unset the global store, it will need to come via the device context
	snapstate.ReplaceStore(s.state, nil)

	tugc := s.state.NewTask("update-kernel", "update kernel")
	chg := s.state.NewChange("remodel", "remodel")
	chg.AddTask(tugc)

	deviceCtx := &snapstatetest.TrivialDeviceContext{CtxStore: s.fakeStore,
		DeviceModel: MakeModel20("brand-gadget", map[string]any{"base": "core24"})}

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.InstallWithDeviceContext(context.Background(), s.state,
		"some-kernel", opts, 0, snapstate.Flags{}, nil, deviceCtx, chg.ID())
	c.Assert(err, IsNil)
	verifyInstallTasks(c, snap.TypeKernel, needsKernelSetup, 0, ts)
}

func (s *snapmgrTestSuite) TestInstallWithDeviceContextRemodelConflict(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	// unset the global store, it will need to come via the device context
	snapstate.ReplaceStore(s.state, nil)

	tugc := s.state.NewTask("update-managed-boot-config", "update managed boot config")
	chg := s.state.NewChange("remodel", "remodel")
	chg.AddTask(tugc)

	deviceCtx := &snapstatetest.TrivialDeviceContext{CtxStore: s.fakeStore, DeviceModel: &asserts.Model{}}

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.InstallWithDeviceContext(context.Background(), s.state, "brand-gadget", opts, 0, snapstate.Flags{}, nil, deviceCtx, "")
	c.Assert(err.Error(), Equals, "remodeling in progress, no other changes allowed until this is done")
	c.Assert(ts, IsNil)
}

func (s *snapmgrTestSuite) TestInstallHookNotRunForInstalledSnap(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "some-snap", Revision: snap.R(7)},
		}),
		Current:  snap.R(7),
		SnapType: "app",
	})

	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
epoch: 1*
`)
	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(8)}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)

	runHooks := tasksWithKind(ts, "run-hook")
	// no install hook task
	c.Assert(taskKinds(runHooks), DeepEquals, []string{
		"run-hook[pre-refresh]",
		"run-hook[post-refresh]",
		"run-hook[configure]",
		"run-hook[check-health]",
	})
}

func (s *snapmgrTestSuite) TestInstallFailsOnDisabledSnap(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapst := &snapstate.SnapState{
		Active:          false,
		TrackingChannel: "channel/stable",
		Sequence:        snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(2)}}),
		Current:         snap.R(2),
		SnapType:        "app",
	}
	snapsup := snapstate.SnapSetup{SideInfo: &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(1)}}
	_, err := snapstate.DoInstall(s.state, snapst, snapsup, nil, 0, "", nil, nil)
	c.Assert(err, ErrorMatches, `cannot update disabled snap "some-snap"`)
}

func inUseCheck(snap.Type) (boot.InUseFunc, error) {
	return func(string, snap.Revision) bool {
		return false
	}, nil
}

func (s *snapmgrTestSuite) TestInstallFailsOnBusySnap(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// With the refresh-app-awareness feature enabled.
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.refresh-app-awareness", true)
	tr.Commit()

	// With a snap state indicating a snap is already installed.
	snapst := &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(1)},
		}),
		Current:  snap.R(1),
		SnapType: "app",
	}
	snapstate.Set(s.state, "some-snap", snapst)

	// With a snap info indicating it has an application called "app"
	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		if name != "some-snap" {
			return s.fakeBackend.ReadInfo(name, si)
		}
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		info.Apps = map[string]*snap.AppInfo{
			"app": {Snap: info, Name: "app"},
		}
		return info, nil
	})

	// mock that "some-snap" has an app and that this app has pids running
	restore := snapstate.MockPidsOfSnap(func(instanceName string) (map[string][]int, error) {
		c.Assert(instanceName, Equals, "some-snap")
		return map[string][]int{
			"snap.some-snap.app": {1234},
		}, nil
	})
	defer restore()

	// Attempt to install revision 2 of the snap.
	snapsup := snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(2)},
	}

	// And observe that we cannot refresh because the snap is busy.
	_, err := snapstate.DoInstall(s.state, snapst, snapsup, nil, 0, "", inUseCheck, nil)
	c.Assert(err, ErrorMatches, `snap "some-snap" has running apps \(app\), pids: 1234`)

	// Don't record time since it wasn't a failed refresh
	err = snapstate.Get(s.state, "some-snap", snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.RefreshInhibitedTime, IsNil)
}

func (s *snapmgrTestSuite) TestInstallWithIgnoreRunningProceedsOnBusySnap(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// With the refresh-app-awareness feature enabled.
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.refresh-app-awareness", true)
	tr.Commit()

	// With a snap state indicating a snap is already installed.
	snapst := &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "pkg", SnapID: "pkg-id", Revision: snap.R(1)},
		}),
		Current:  snap.R(1),
		SnapType: "app",
	}
	snapstate.Set(s.state, "pkg", snapst)

	// With a snap info indicating it has an application called "app"
	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		if name != "pkg" {
			return s.fakeBackend.ReadInfo(name, si)
		}
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		info.Apps = map[string]*snap.AppInfo{
			"app": {Snap: info, Name: "app"},
		}
		return info, nil
	})

	// With an app belonging to the snap that is apparently running.
	restore := snapstate.MockPidsOfSnap(func(instanceName string) (map[string][]int, error) {
		c.Assert(instanceName, Equals, "pkg")
		return map[string][]int{
			"snap.pkg.app": {1234},
		}, nil
	})
	defer restore()

	var called bool
	restore = snapstate.MockExcludeFromRefreshAppAwareness(func(t snap.Type) bool {
		called = true
		c.Check(t, Equals, snap.TypeApp)
		return false
	})
	defer restore()

	// Attempt to install revision 2 of the snap, with the IgnoreRunning flag set.
	snapsup := snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{RealName: "pkg", SnapID: "pkg-id", Revision: snap.R(2)},
		Flags:    snapstate.Flags{IgnoreRunning: true},
		Type:     "app",
	}

	// And observe that we do so despite the running app.
	_, err := snapstate.DoInstall(s.state, snapst, snapsup, nil, 0, "", inUseCheck, nil)
	c.Assert(err, IsNil)

	// The state confirms that the refresh operation was not postponed.
	err = snapstate.Get(s.state, "pkg", snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.RefreshInhibitedTime, IsNil)
	c.Check(called, Equals, true)
}

func (s *snapmgrTestSuite) TestInstallDespiteBusySnap(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// With the refresh-app-awareness feature enabled.
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.refresh-app-awareness", true)
	tr.Commit()

	// With a snap state indicating a snap is already installed and it failed
	// to refresh over a week ago. Use UTC and Round to have predictable
	// behaviour across time-zones and with enough precision loss to be
	// compatible with the serialization format.
	var longAgo = time.Now().UTC().Round(time.Second).Add(-time.Hour * 24 * 8)
	snapst := &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(1)},
		}),
		Current:              snap.R(1),
		SnapType:             "app",
		RefreshInhibitedTime: &longAgo,
	}
	snapstate.Set(s.state, "some-snap", snapst)

	// With a snap info indicating it has an application called "app"
	snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		if name != "some-snap" {
			return s.fakeBackend.ReadInfo(name, si)
		}
		info := &snap.Info{SuggestedName: name, SideInfo: *si, SnapType: snap.TypeApp}
		info.Apps = map[string]*snap.AppInfo{
			"app": {Snap: info, Name: "app"},
		}
		return info, nil
	})
	// And with cgroup information indicating the app has a process with pid 1234.
	restore := snapstate.MockPidsOfSnap(func(instanceName string) (map[string][]int, error) {
		c.Assert(instanceName, Equals, "some-snap")
		return map[string][]int{
			"snap.some-snap.some-app": {1234},
		}, nil
	})
	defer restore()

	// Attempt to install revision 2 of the snap.
	snapsup := snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(2)},
	}

	// And observe that refresh occurred regardless of the running process.
	_, err := snapstate.DoInstall(s.state, snapst, snapsup, nil, 0, "", inUseCheck, nil)
	c.Assert(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallFailsOnSystem(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapsup := snapstate.SnapSetup{SideInfo: &snap.SideInfo{RealName: "system", SnapID: "some-snap-id", Revision: snap.R(1)}}
	_, err := snapstate.DoInstall(s.state, nil, snapsup, nil, 0, "", nil, nil)
	c.Assert(err, ErrorMatches, `cannot install reserved snap name 'system'`)
}

func (s *snapmgrTestSuite) TestDoInstallChannelDefault(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, 0, snapstate.Flags{})
	c.Assert(err, IsNil)

	var snapsup snapstate.SnapSetup
	err = ts.Tasks()[0].Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)

	c.Check(snapsup.Channel, Equals, "stable")
}

func (s *snapmgrTestSuite) TestInstallRevision(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	opts := &snapstate.RevisionOptions{Revision: snap.R(7)}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, 0, snapstate.Flags{})
	c.Assert(err, IsNil)

	var snapsup snapstate.SnapSetup
	err = ts.Tasks()[0].Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)

	c.Check(snapsup.Revision(), Equals, snap.R(7))
}

func (s *snapmgrTestSuite) TestInstallTooEarly(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	s.state.Set("seeded", nil)

	_, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, 0, snapstate.Flags{})
	c.Check(err, testutil.ErrorIs, &snapstate.ChangeConflictError{})
	c.Assert(err, ErrorMatches, `.*too early for operation, device not yet seeded or device model not acknowledged`)
}

func (s *snapmgrTestSuite) TestInstallConflict(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, 0, snapstate.Flags{})
	c.Assert(err, IsNil)
	// need a change to make the tasks visible
	s.state.NewChange("install", "...").AddAll(ts)

	_, err = snapstate.Install(context.Background(), s.state, "some-snap", nil, 0, snapstate.Flags{})
	c.Check(err, FitsTypeOf, &snapstate.ChangeConflictError{})
	c.Assert(err, ErrorMatches, `snap "some-snap" has "install" change in progress`)
}

func (s *snapmgrTestSuite) TestGadgetInstallConflict(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tugc := s.state.NewTask("update-managed-boot-config", "update managed boot config")
	chg := s.state.NewChange("snapd-update", "snapd update")
	chg.AddTask(tugc)

	_, err := snapstate.Install(context.Background(), s.state, "brand-gadget",
		nil, 0, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "boot config is being updated, no change in kernel command line is allowed meanwhile")
}

func (s *snapmgrTestSuite) TestInstallNoRestartBoundaries(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	r := snapstatetest.MockDeviceModel(DefaultModel())
	defer r()

	snapsup := snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "brand-gadget",
			SnapID:   "brand-gadget",
			Revision: snap.R(2),
		},
		Type: snap.TypeGadget,
	}

	// Ensure that restart boundaries were set on 'link-snap' as a part of doInstall
	// when the flag noRestartBoundaries is not set
	ts1, err := snapstate.DoInstall(s.state, &snapstate.SnapState{}, snapsup, nil, 0, "", inUseCheck, nil)
	c.Assert(err, IsNil)

	linkSnap1 := ts1.MaybeEdge(snapstate.MaybeRebootEdge)
	c.Assert(linkSnap1, NotNil)

	var boundary restart.RestartBoundaryDirection
	c.Check(linkSnap1.Get("restart-boundary", &boundary), IsNil)

	// Ensure that restart boundaries are not set when we do provide the noRestartBoundaries flag
	ts2, err := snapstate.DoInstall(s.state, &snapstate.SnapState{}, snapsup, nil, snapstate.NoRestartBoundaries, "", inUseCheck, nil)
	c.Assert(err, IsNil)

	linkSnap2 := ts2.MaybeEdge(snapstate.MaybeRebootEdge)
	c.Assert(linkSnap2, NotNil)
	c.Check(linkSnap2.Get("restart-boundary", &boundary), ErrorMatches, `no state entry for key "restart-boundary"`)
}

func (s *snapmgrTestSuite) TestInstallSnapdConflict(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	// remove snapd snap added for snapmgrBaseTest
	snapstate.Set(s.state, "snapd", nil)

	tugc := s.state.NewTask("update-gadget-cmdline", "update gadget cmdline")
	chg := s.state.NewChange("optional-kernel-cmdline", "optional kernel cmdline")
	chg.AddTask(tugc)

	_, err := snapstate.Install(context.Background(), s.state, "snapd",
		nil, 0, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "kernel command line already being updated, no additional changes for it allowed meanwhile")
}

func (s *snapmgrTestSuite) TestInstallAliasConflict(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "otherfoosnap", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "otherfoosnap", Revision: snap.R(30)},
		}),
		Current: snap.R(30),
		Active:  true,
		Aliases: map[string]*snapstate.AliasTarget{
			"foo.bar": {Manual: "bar"},
		},
		SnapType: "app",
	})

	_, err := snapstate.Install(context.Background(), s.state, "foo", nil, 0, snapstate.Flags{})
	c.Assert(err, ErrorMatches, `snap "foo" command namespace conflicts with alias "foo\.bar" for "otherfoosnap" snap`)
}

func (s *snapmgrTestSuite) TestInstallStrictIgnoresClassic(c *C) {
	restore := maybeMockClassicSupport(c)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	opts := &snapstate.RevisionOptions{Channel: "channel-for-strict"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{Classic: true})
	c.Assert(err, IsNil)

	chg := s.state.NewChange("install", "install snap")
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	// verify snap is *not* classic
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.TrackingChannel, Equals, "channel-for-strict/stable")
	c.Check(snapst.Classic, Equals, false)
}

func (s *snapmgrTestSuite) TestInstallSnapWithDefaultTrack(c *C) {
	restore := maybeMockClassicSupport(c)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	opts := &snapstate.RevisionOptions{Channel: "candidate"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap-with-default-track", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	chg := s.state.NewChange("install", "install snap")
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	// verify snap is in the 2.0 track
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap-with-default-track", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.TrackingChannel, Equals, "2.0/candidate")
}

func (s *snapmgrTestSuite) TestInstallIgnoreValidation(c *C) {
	restore := maybeMockClassicSupport(c)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, s.user.ID, snapstate.Flags{IgnoreValidation: true})
	c.Assert(err, IsNil)

	chg := s.state.NewChange("install", "install snap")
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.IgnoreValidation, Equals, true)
}

func (s *snapmgrTestSuite) TestInstallManySnapOneWithDefaultTrack(c *C) {
	restore := maybeMockClassicSupport(c)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	snapNames := []string{"some-snap", "some-snap-with-default-track"}
	installed, tss, err := snapstate.InstallMany(s.state, snapNames, nil, s.user.ID, nil)
	c.Assert(err, IsNil)
	c.Assert(installed, DeepEquals, snapNames)

	chg := s.state.NewChange("install", "install two snaps")
	for _, ts := range tss {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	// verify snap is in the 2.0 track
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap-with-default-track", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.TrackingChannel, Equals, "2.0/stable")

	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.TrackingChannel, Equals, "latest/stable")
}

// A sneakyStore changes the state when called
type sneakyStore struct {
	*fakeStore
	state  *state.State
	remove bool
}

func (s sneakyStore) SnapAction(ctx context.Context, currentSnaps []*store.CurrentSnap, actions []*store.SnapAction, assertQuery store.AssertionQuery, user *auth.UserState, opts *store.RefreshOptions) ([]store.SnapActionResult, []store.AssertionResult, error) {
	s.state.Lock()
	if s.remove {
		snapstate.Set(s.state, "some-snap", nil)
	} else {
		snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
			Active:          true,
			TrackingChannel: "latest/edge",
			Sequence:        snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(1)}}),
			Current:         snap.R(1),
			SnapType:        "app",
		})
	}
	s.state.Unlock()
	return s.fakeStore.SnapAction(ctx, currentSnaps, actions, assertQuery, user, opts)
}

func (s *snapmgrTestSuite) TestInstallStateConflict(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, sneakyStore{fakeStore: s.fakeStore, state: s.state})

	_, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, 0, snapstate.Flags{})
	c.Check(err, testutil.ErrorIs, &snapstate.ChangeConflictError{})
	c.Assert(err, ErrorMatches, `snap "some-snap" has changes in progress`)
}

func (s *snapmgrTestSuite) TestInstallPathTooEarly(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	r := snapstatetest.MockDeviceModel(nil)
	defer r()

	mockSnap := makeTestSnap(c, "name: some-snap\nversion: 1.0")
	t := snapstate.PathInstallGoal(snapstate.PathSnap{
		Path:     mockSnap,
		SideInfo: &snap.SideInfo{RealName: "some-snap"},
	})
	_, _, err := snapstate.InstallWithGoal(context.Background(), s.state, t, snapstate.Options{
		Seed: true,
	})
	c.Check(err, testutil.ErrorIs, &snapstate.ChangeConflictError{})
	c.Assert(err, ErrorMatches, `.*too early for operation, device model not yet acknowledged`)
}

func (s *snapmgrTestSuite) TestInstallPathConflict(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, 0, snapstate.Flags{})
	c.Assert(err, IsNil)
	// need a change to make the tasks visible
	s.state.NewChange("install", "...").AddAll(ts)

	mockSnap := makeTestSnap(c, "name: some-snap\nversion: 1.0")
	_, _, err = snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap"}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, ErrorMatches, `snap "some-snap" has "install" change in progress`)
}

func (s *snapmgrTestSuite) TestInstallPathMissingName(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	mockSnap := makeTestSnap(c, "name: some-snap\nversion: 1.0")
	_, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, ErrorMatches, fmt.Sprintf(`internal error: snap name to install %q not provided`, mockSnap))
}

func (s *snapmgrTestSuite) TestInstallPathSnapIDRevisionUnset(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	mockSnap := makeTestSnap(c, "name: some-snap\nversion: 1.0")
	_, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap", SnapID: "snapididid"}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, ErrorMatches, fmt.Sprintf(`internal error: snap id set to install %q but revision is unset`, mockSnap))
}

func (s *snapmgrTestSuite) TestInstallPathValidateFlags(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
confinement: devmode
`)
	_, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap"}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, ErrorMatches, `.* requires devmode or confinement override`)
}

func (s *snapmgrTestSuite) TestInstallPathStrictIgnoresClassic(c *C) {
	restore := maybeMockClassicSupport(c)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
confinement: strict
`)

	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap"}, mockSnap, "", "", snapstate.Flags{Classic: true}, nil)
	c.Assert(err, IsNil)

	chg := s.state.NewChange("install", "install snap")
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	// verify snap is *not* classic
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.Classic, Equals, false)
}

func (s *snapmgrTestSuite) TestInstallPathAsRefresh(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active: true,
		Flags:  snapstate.Flags{DevMode: true},
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(1)},
		}),
		Current:         snap.R(1),
		SnapType:        "app",
		TrackingChannel: "wibbly/stable",
	})

	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
epoch: 1
`)

	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap"}, mockSnap, "", "edge", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)

	chg := s.state.NewChange("install", "install snap")
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	// verify snap is *not* classic
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.TrackingChannel, Equals, "wibbly/edge")
}

func (s *snapmgrTestSuite) TestParallelInstanceInstallNotAllowed(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, sneakyStore{fakeStore: s.fakeStore, state: s.state})

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.parallel-instances", true)
	tr.Commit()

	_, err := snapstate.Install(context.Background(), s.state, "core_foo", nil, 0, snapstate.Flags{})
	c.Check(err, ErrorMatches, `cannot install snap of type os as "core_foo"`)

	_, err = snapstate.Install(context.Background(), s.state, "some-base_foo", nil, 0, snapstate.Flags{})
	c.Check(err, ErrorMatches, `cannot install snap of type base as "some-base_foo"`)

	_, err = snapstate.Install(context.Background(), s.state, "some-gadget_foo", nil, 0, snapstate.Flags{})
	c.Check(err, ErrorMatches, `cannot install snap of type gadget as "some-gadget_foo"`)

	_, err = snapstate.Install(context.Background(), s.state, "some-kernel_foo", nil, 0, snapstate.Flags{})
	c.Check(err, ErrorMatches, `cannot install snap of type kernel as "some-kernel_foo"`)

	_, err = snapstate.Install(context.Background(), s.state, "some-snapd_foo", nil, 0, snapstate.Flags{})
	c.Check(err, ErrorMatches, `cannot install snap of type snapd as "some-snapd_foo"`)
}

func (s *snapmgrTestSuite) TestInstallPathFailsEarlyOnEpochMismatch(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// have epoch 1* installed
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:          true,
		TrackingChannel: "latest/edge",
		Sequence:        snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{{RealName: "some-snap", Revision: snap.R(7)}}),
		Current:         snap.R(7),
	})

	// try to install epoch 42
	mockSnap := makeTestSnap(c, "name: some-snap\nversion: 1.0\nepoch: 42\n")
	_, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap"}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, ErrorMatches, `cannot refresh "some-snap" to local snap with epoch 42, because it can't read the current epoch of 1\*`)
}

func (s *snapmgrTestSuite) TestInstallRunThrough(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// we start without the auxiliary store info
	c.Check(backend.AuxStoreInfoFilename("some-snap-id"), testutil.FileAbsent)

	iconContents := []byte("I'm an svg")
	// set up the downloadIcon callback to write the fake icon to the icons pool
	var downloadIconCalls int
	s.fakeStore.downloadIconCallback = func(targetPath string) {
		downloadIconCalls++
		c.Assert(downloadIconCalls, Equals, 1)

		c.Assert(os.MkdirAll(filepath.Dir(targetPath), 0o755), IsNil)
		c.Assert(os.WriteFile(targetPath, iconContents, 0o644), IsNil)
	}

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "channel-for-media"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	c.Check(snapstate.Installing(s.state), Equals, false)
	c.Check(s.fakeStore.downloads, DeepEquals, []fakeDownload{{
		macaroon: s.user.StoreMacaroon,
		name:     "some-snap",
		target:   filepath.Join(dirs.SnapBlobDir, "some-snap_11.snap"),
	}})
	c.Check(s.fakeStore.iconDownloads, DeepEquals, []fakeIconDownload{{
		name:   "some-snap",
		target: backend.IconDownloadFilename("some-snap-id"),
		url:    "http://example.com/icon.png",
	}})
	c.Check(s.fakeStore.seenPrivacyKeys["privacy-key"], Equals, true, Commentf("salts seen: %v", s.fakeStore.seenPrivacyKeys))
	expected := fakeOps{
		{
			op:     "storesvc-snap-action",
			userID: 1,
		},
		{
			op: "storesvc-snap-action:action",
			action: store.SnapAction{
				Action:       "install",
				InstanceName: "some-snap",
				Channel:      "channel-for-media",
			},
			revno:  snap.R(11),
			userID: 1,
		},
		{
			op:   "storesvc-download",
			name: "some-snap",
		},
		{
			op:   "storesvc-download-icon",
			name: "some-snap",
		},
		{
			op:    "validate-snap:Doing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op:  "current",
			old: "<no-current>",
		},
		{
			op:   "open-snap-file",
			path: filepath.Join(dirs.SnapBlobDir, "some-snap_11.snap"),
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Channel:  "channel-for-media",
				Revision: snap.R(11),
			},
		},
		{
			op:    "setup-snap",
			name:  "some-snap",
			path:  filepath.Join(dirs.SnapBlobDir, "some-snap_11.snap"),
			revno: snap.R(11),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/11"),
			old:  "<no-old>",
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "some-snap"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Channel:  "channel-for-media",
				Revision: snap.R(11),
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/11"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op: "update-aliases",
		},
		{
			op:    "cleanup-trash",
			name:  "some-snap",
			revno: snap.R(11),
		},
	}
	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Assert(s.fakeBackend.ops, DeepEquals, expected)

	// check progress
	ta := ts.Tasks()
	task := ta[1]
	_, cur, total := task.Progress()
	c.Assert(cur, Equals, s.fakeStore.fakeCurrentProgress)
	c.Assert(total, Equals, s.fakeStore.fakeTotalProgress)
	c.Check(task.Summary(), Equals, `Download snap "some-snap" (11) from channel "channel-for-media"`)

	// check install-record present
	mountTask := ta[len(ta)-12]
	c.Check(mountTask.Kind(), Equals, "mount-snap")
	var installRecord backend.InstallRecord
	c.Assert(mountTask.Get("install-record", &installRecord), IsNil)
	c.Check(installRecord.TargetSnapExisted, Equals, false)

	// check link/start snap summary
	linkTask := ta[len(ta)-9]
	c.Check(linkTask.Summary(), Equals, `Make snap "some-snap" (11) available to the system`)
	startTask := ta[len(ta)-3]
	c.Check(startTask.Summary(), Equals, `Start snap "some-snap" (11) services`)

	// verify snap-setup in the task state
	var snapsup snapstate.SnapSetup
	err = task.Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)

	c.Assert(snapsup.Channel, Equals, "channel-for-media")
	c.Assert(snapsup.UserID, Equals, s.user.ID)
	c.Assert(snapsup.SnapPath, Matches, `.*some-snap_11.snap`)
	c.Assert(snapsup.DownloadInfo, DeepEquals, &snap.DownloadInfo{
		DownloadURL: "https://some-server.com/some/path.snap",
		Size:        5,
	})
	c.Assert(snapsup.SideInfo, DeepEquals, snapsup.SideInfo)
	c.Assert(snapsup.Type, Equals, snap.TypeApp)
	c.Assert(snapsup.Version, Equals, "some-snapVer")
	c.Assert(snapsup.PlugsOnly, Equals, true)

	c.Assert(snapsup.SideInfo, DeepEquals, &snap.SideInfo{
		RealName: "some-snap",
		Channel:  "channel-for-media",
		Revision: snap.R(11),
		SnapID:   "some-snap-id",
	})

	// verify snaps in the system state
	var snaps map[string]*snapstate.SnapState
	err = s.state.Get("snaps", &snaps)
	c.Assert(err, IsNil)

	snapst := snaps["some-snap"]
	c.Assert(snapst, NotNil)
	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.TrackingChannel, Equals, "channel-for-media/stable")
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: "some-snap",
		SnapID:   "some-snap-id",
		Channel:  "channel-for-media",
		Revision: snap.R(11),
	}, nil))
	c.Assert(snapst.Required, Equals, false)

	info := snap.Info{
		SideInfo: snap.SideInfo{
			SnapID:   "some-snap-id",
			RealName: "some-snap",
		},
	}
	err = backend.RetrieveAuxStoreInfo(&info)
	c.Assert(err, IsNil)

	c.Assert(info.Media, DeepEquals, snap.MediaInfos{
		snap.MediaInfo{
			Type:   "icon",
			URL:    "http://example.com/icon.png",
			Width:  100,
			Height: 100,
		},
		snap.MediaInfo{
			Type: "website",
			URL:  "http://example.com",
		},
	})
	c.Check(info.StoreURL, Equals, "https://snapcraft.io/example-snap")

	c.Check(backend.IconInstallFilename("some-snap-id"), testutil.FileEquals, iconContents)
}

func (s *snapmgrTestSuite) testParallelInstanceInstallRunThrough(c *C, inputFlags, expectedFlags snapstate.Flags) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.parallel-instances", true)
	tr.Commit()

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap_instance", opts, s.user.ID, inputFlags)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	c.Check(snapstate.Installing(s.state), Equals, false)
	c.Check(s.fakeStore.downloads, DeepEquals, []fakeDownload{{
		macaroon: s.user.StoreMacaroon,
		name:     "some-snap",
		target:   filepath.Join(dirs.SnapBlobDir, "some-snap_instance_11.snap"),
	}})
	c.Check(s.fakeStore.seenPrivacyKeys["privacy-key"], Equals, true, Commentf("salts seen: %v", s.fakeStore.seenPrivacyKeys))
	expected := fakeOps{
		{
			op:     "storesvc-snap-action",
			userID: 1,
		},
		{
			op: "storesvc-snap-action:action",
			action: store.SnapAction{
				Action:       "install",
				InstanceName: "some-snap_instance",
				Channel:      "some-channel",
			},
			revno:  snap.R(11),
			userID: 1,
		},
		{
			op:   "storesvc-download",
			name: "some-snap",
		},
		{
			op:    "validate-snap:Doing",
			name:  "some-snap_instance",
			revno: snap.R(11),
		},
		{
			op:  "current",
			old: "<no-current>",
		},
		{
			op:   "open-snap-file",
			path: filepath.Join(dirs.SnapBlobDir, "some-snap_instance_11.snap"),
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Channel:  "some-channel",
				Revision: snap.R(11),
			},
		},
		{
			op:    "setup-snap",
			name:  "some-snap_instance",
			path:  filepath.Join(dirs.SnapBlobDir, "some-snap_instance_11.snap"),
			revno: snap.R(11),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "some-snap_instance/11"),
			old:  "<no-old>",
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "some-snap_instance"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "some-snap_instance",
			revno: snap.R(11),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Channel:  "some-channel",
				Revision: snap.R(11),
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "some-snap_instance/11"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "some-snap_instance",
			revno: snap.R(11),
		},
		{
			op: "update-aliases",
		},
	}
	if inputFlags.Prefer {
		expected = append(expected, fakeOp{op: "update-aliases"})
	}
	expected = append(expected, fakeOp{
		op:    "cleanup-trash",
		name:  "some-snap_instance",
		revno: snap.R(11),
	})
	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Assert(s.fakeBackend.ops, DeepEquals, expected)

	// check progress
	ta := ts.Tasks()
	task := ta[1]
	_, cur, total := task.Progress()
	c.Assert(cur, Equals, s.fakeStore.fakeCurrentProgress)
	c.Assert(total, Equals, s.fakeStore.fakeTotalProgress)
	c.Check(task.Summary(), Equals, `Download snap "some-snap_instance" (11) from channel "some-channel"`)

	// check link/start snap summary
	linkTaskOffset := 9
	if inputFlags.Prefer {
		linkTaskOffset = 10
	}
	linkTask := ta[len(ta)-linkTaskOffset]
	c.Check(linkTask.Summary(), Equals, `Make snap "some-snap_instance" (11) available to the system`)
	startTask := ta[len(ta)-3]
	c.Check(startTask.Summary(), Equals, `Start snap "some-snap_instance" (11) services`)

	// verify snap-setup in the task state
	var snapsup snapstate.SnapSetup
	err = task.Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)
	c.Assert(snapsup, DeepEquals, snapstate.SnapSetup{
		Channel:  "some-channel",
		UserID:   s.user.ID,
		SnapPath: filepath.Join(dirs.SnapBlobDir, "some-snap_instance_11.snap"),
		DownloadInfo: &snap.DownloadInfo{
			DownloadURL: "https://some-server.com/some/path.snap",
			Size:        5,
		},
		SideInfo:    snapsup.SideInfo,
		Type:        snap.TypeApp,
		Version:     "some-snapVer",
		PlugsOnly:   true,
		InstanceKey: "instance",
		Flags:       expectedFlags,
	})
	c.Assert(snapsup.SideInfo, DeepEquals, &snap.SideInfo{
		RealName: "some-snap",
		Channel:  "some-channel",
		Revision: snap.R(11),
		SnapID:   "some-snap-id",
	})

	// verify snaps in the system state
	var snaps map[string]*snapstate.SnapState
	err = s.state.Get("snaps", &snaps)
	c.Assert(err, IsNil)

	snapst := snaps["some-snap_instance"]
	c.Assert(snapst, NotNil)
	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.TrackingChannel, Equals, "some-channel/stable")
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: "some-snap",
		SnapID:   "some-snap-id",
		Channel:  "some-channel",
		Revision: snap.R(11),
	}, nil))
	c.Assert(snapst.Required, Equals, false)
	c.Assert(snapst.InstanceKey, Equals, "instance")

	runHooks := tasksWithKind(ts, "run-hook")
	c.Assert(taskKinds(runHooks), DeepEquals, []string{"run-hook[install]", "run-hook[default-configure]", "run-hook[configure]", "run-hook[check-health]"})
	for _, hookTask := range runHooks {
		c.Assert(hookTask.Kind(), Equals, "run-hook")
		var hooksup hookstate.HookSetup
		err = hookTask.Get("hook-setup", &hooksup)
		c.Assert(err, IsNil)
		c.Assert(hooksup.Snap, Equals, "some-snap_instance")
	}
}

func (s *snapmgrTestSuite) TestParallelInstanceInstallRunThrough(c *C) {
	// parallel installs should implicitly pass --unaliased
	inputFlags := snapstate.Flags{}
	expectedFlags := snapstate.Flags{Unaliased: true}
	s.testParallelInstanceInstallRunThrough(c, inputFlags, expectedFlags)
}

func (s *snapmgrTestSuite) TestParallelInstanceInstallPreferRunThrough(c *C) {
	// --prefer should prevent --unaliased from being passed implicitly
	inputFlags := snapstate.Flags{Prefer: true}
	expectedFlags := snapstate.Flags{Prefer: true, Unaliased: false}
	s.testParallelInstanceInstallRunThrough(c, inputFlags, expectedFlags)
}

func (s *snapmgrTestSuite) TestInstallUndoRunThroughJustOneSnap(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	tasks := ts.Tasks()
	last := tasks[len(tasks)-1]
	// validity
	c.Assert(last.Lanes(), HasLen, 1)
	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(last)
	terr.JoinLane(last.Lanes()[0])
	chg.AddTask(terr)

	s.settle(c)

	mountTask := tasks[len(tasks)-12]
	c.Assert(mountTask.Kind(), Equals, "mount-snap")
	var installRecord backend.InstallRecord
	c.Assert(mountTask.Get("install-record", &installRecord), IsNil)
	c.Check(installRecord.TargetSnapExisted, Equals, false)

	// ensure all our tasks ran
	c.Check(s.fakeStore.downloads, DeepEquals, []fakeDownload{{
		macaroon: s.user.StoreMacaroon,
		name:     "some-snap",
		target:   filepath.Join(dirs.SnapBlobDir, "some-snap_11.snap"),
	}})
	expected := fakeOps{
		{
			op:     "storesvc-snap-action",
			userID: 1,
		},
		{
			op: "storesvc-snap-action:action",
			action: store.SnapAction{
				Action:       "install",
				InstanceName: "some-snap",
				Channel:      "some-channel",
			},
			revno:  snap.R(11),
			userID: 1,
		},
		{
			op:   "storesvc-download",
			name: "some-snap",
		},
		{
			op:    "validate-snap:Doing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op:  "current",
			old: "<no-current>",
		},
		{
			op:   "open-snap-file",
			path: filepath.Join(dirs.SnapBlobDir, "some-snap_11.snap"),
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Channel:  "some-channel",
				Revision: snap.R(11),
			},
		},
		{
			op:    "setup-snap",
			name:  "some-snap",
			path:  filepath.Join(dirs.SnapBlobDir, "some-snap_11.snap"),
			revno: snap.R(11),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/11"),
			old:  "<no-old>",
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "some-snap"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Channel:  "some-channel",
				Revision: snap.R(11),
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/11"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op: "update-aliases",
		},
		{
			op: "update-aliases",
		},
		{
			op:    "auto-connect:Undoing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op:   "discard-namespace",
			name: "some-snap",
		},
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/11"),

			unlinkFirstInstallUndo: true,
		},
		{
			op:    "setup-profiles:Undoing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op:   "undo-copy-snap-data",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/11"),
			old:  "<no-old>",
		},
		{
			op:   "undo-setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "some-snap"),
			old:  "<no-old>",
		},
		{
			op:   "remove-snap-data-dir",
			name: "some-snap",
			path: filepath.Join(dirs.SnapDataDir, "some-snap"),
		},
		{
			op:    "undo-setup-snap",
			name:  "some-snap",
			stype: "app",
			path:  filepath.Join(dirs.SnapMountDir, "some-snap/11"),
		},
		{
			op:   "remove-snap-dir",
			name: "some-snap",
			path: filepath.Join(dirs.SnapMountDir, "some-snap"),
		},
	}
	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Assert(s.fakeBackend.ops, DeepEquals, expected)
}

func (s *snapmgrTestSuite) TestInstallWithCohortRunThrough(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel", CohortKey: "scurries"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	c.Check(snapstate.Installing(s.state), Equals, false)
	c.Check(s.fakeStore.downloads, DeepEquals, []fakeDownload{{
		macaroon: s.user.StoreMacaroon,
		name:     "some-snap",
		target:   filepath.Join(dirs.SnapBlobDir, "some-snap_666.snap"),
	}})
	expected := fakeOps{
		{
			op:     "storesvc-snap-action",
			userID: 1,
		},
		{
			op: "storesvc-snap-action:action",
			action: store.SnapAction{
				Action:       "install",
				InstanceName: "some-snap",
				CohortKey:    "scurries",
				Channel:      "some-channel",
			},
			revno:  snap.R(666),
			userID: 1,
		},
		{
			op:   "storesvc-download",
			name: "some-snap",
		},
		{
			op:    "validate-snap:Doing",
			name:  "some-snap",
			revno: snap.R(666),
		},
		{
			op:  "current",
			old: "<no-current>",
		},
		{
			op:   "open-snap-file",
			path: filepath.Join(dirs.SnapBlobDir, "some-snap_666.snap"),
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Revision: snap.R(666),
				Channel:  "some-channel",
			},
		},
		{
			op:    "setup-snap",
			name:  "some-snap",
			path:  filepath.Join(dirs.SnapBlobDir, "some-snap_666.snap"),
			revno: snap.R(666),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/666"),
			old:  "<no-old>",
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "some-snap"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "some-snap",
			revno: snap.R(666),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Revision: snap.R(666),
				Channel:  "some-channel",
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/666"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "some-snap",
			revno: snap.R(666),
		},
		{
			op: "update-aliases",
		},
		{
			op:    "cleanup-trash",
			name:  "some-snap",
			revno: snap.R(666),
		},
	}
	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Assert(s.fakeBackend.ops, DeepEquals, expected)

	// check progress
	ta := ts.Tasks()
	task := ta[1]
	_, cur, total := task.Progress()
	c.Assert(cur, Equals, s.fakeStore.fakeCurrentProgress)
	c.Assert(total, Equals, s.fakeStore.fakeTotalProgress)
	c.Check(task.Summary(), Equals, `Download snap "some-snap" (666) from channel "some-channel"`)

	// check link/start snap summary
	linkTask := ta[len(ta)-9]
	c.Check(linkTask.Summary(), Equals, `Make snap "some-snap" (666) available to the system`)
	startTask := ta[len(ta)-3]
	c.Check(startTask.Summary(), Equals, `Start snap "some-snap" (666) services`)

	// verify snap-setup in the task state
	var snapsup snapstate.SnapSetup
	err = task.Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)
	c.Assert(snapsup, DeepEquals, snapstate.SnapSetup{
		Channel:  "some-channel",
		UserID:   s.user.ID,
		SnapPath: filepath.Join(dirs.SnapBlobDir, "some-snap_666.snap"),
		DownloadInfo: &snap.DownloadInfo{
			DownloadURL: "https://some-server.com/some/path.snap",
			Size:        5,
		},
		SideInfo:  snapsup.SideInfo,
		Type:      snap.TypeApp,
		Version:   "some-snapVer",
		PlugsOnly: true,
		CohortKey: "scurries",
	})
	c.Assert(snapsup.SideInfo, DeepEquals, &snap.SideInfo{
		RealName: "some-snap",
		Revision: snap.R(666),
		SnapID:   "some-snap-id",
		Channel:  "some-channel",
	})

	// verify snaps in the system state
	var snaps map[string]*snapstate.SnapState
	err = s.state.Get("snaps", &snaps)
	c.Assert(err, IsNil)

	snapst := snaps["some-snap"]
	c.Assert(snapst, NotNil)
	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.TrackingChannel, Equals, "some-channel/stable")
	c.Assert(snapst.CohortKey, Equals, "scurries")
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: "some-snap",
		SnapID:   "some-snap-id",
		Revision: snap.R(666),
		Channel:  "some-channel",
	}, nil))
	c.Assert(snapst.Required, Equals, false)
}

func (s *snapmgrTestSuite) TestInstallWithRevisionRunThrough(c *C) {
	const (
		channel      = ""
		snapName     = "some-snap"
		defaultTrack = ""
	)
	s.testInstallWithRevisionRunThrough(c, snapName, channel, defaultTrack)
}

func (s *snapmgrTestSuite) TestInstallWithRevisionRunThroughChannel(c *C) {
	const (
		channel      = "some-channel/stable"
		snapName     = "some-snap"
		defaultTrack = ""
	)
	s.testInstallWithRevisionRunThrough(c, snapName, channel, defaultTrack)
}

func (s *snapmgrTestSuite) TestInstallWithRevisionRunThroughDefaultTrackWithChannel(c *C) {
	const (
		channel      = "edge"
		snapName     = "some-snap-with-default-track"
		defaultTrack = "2.0/edge"
	)
	s.testInstallWithRevisionRunThrough(c, snapName, channel, defaultTrack)
}

func (s *snapmgrTestSuite) testInstallWithRevisionRunThrough(c *C, snapName, requestedChannel, defaultTrack string) {
	s.state.Lock()
	defer s.state.Unlock()

	snapID := snapName + "-id"
	snapFileName := snapName + "_42.snap"

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: requestedChannel, Revision: snap.R(42)}
	ts, err := snapstate.Install(context.Background(), s.state, snapName, opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	c.Check(snapstate.Installing(s.state), Equals, false)
	c.Check(s.fakeStore.downloads, DeepEquals, []fakeDownload{{
		macaroon: s.user.StoreMacaroon,
		name:     snapName,
		target:   filepath.Join(dirs.SnapBlobDir, snapFileName),
	}})
	expected := fakeOps{
		{
			op:     "storesvc-snap-action",
			userID: 1,
		},
		{
			op: "storesvc-snap-action:action",
			action: store.SnapAction{
				Action:       "install",
				InstanceName: snapName,
				Revision:     snap.R(42),
				Channel:      requestedChannel,
			},
			revno:  snap.R(42),
			userID: 1,
		},
		{
			op:   "storesvc-download",
			name: snapName,
		},
		{
			op:    "validate-snap:Doing",
			name:  snapName,
			revno: snap.R(42),
		},
		{
			op:  "current",
			old: "<no-current>",
		},
		{
			op:   "open-snap-file",
			path: filepath.Join(dirs.SnapBlobDir, snapFileName),
			sinfo: snap.SideInfo{
				RealName: snapName,
				SnapID:   snapID,
				Revision: snap.R(42),
				Channel:  requestedChannel,
			},
		},
		{
			op:    "setup-snap",
			name:  snapName,
			path:  filepath.Join(dirs.SnapBlobDir, snapFileName),
			revno: snap.R(42),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, filepath.Join(snapName, "42")),
			old:  "<no-old>",
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, snapName),
		},
		{
			op:    "setup-profiles:Doing",
			name:  snapName,
			revno: snap.R(42),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: snapName,
				SnapID:   snapID,
				Revision: snap.R(42),
				Channel:  requestedChannel,
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, filepath.Join(snapName, "42")),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  snapName,
			revno: snap.R(42),
		},
		{
			op: "update-aliases",
		},
		{
			op:    "cleanup-trash",
			name:  snapName,
			revno: snap.R(42),
		},
	}
	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Assert(s.fakeBackend.ops, DeepEquals, expected)

	var setupChannel, trackedChannel string
	switch {
	case defaultTrack != "":
		trackedChannel = defaultTrack
		setupChannel = defaultTrack
	case requestedChannel != "":
		trackedChannel = requestedChannel
		setupChannel = requestedChannel
	default:
		trackedChannel = "latest/stable"
		setupChannel = "stable"
	}

	// check progress
	ta := ts.Tasks()
	task := ta[1]
	_, cur, total := task.Progress()
	c.Assert(cur, Equals, s.fakeStore.fakeCurrentProgress)
	c.Assert(total, Equals, s.fakeStore.fakeTotalProgress)
	c.Check(task.Summary(), Equals, fmt.Sprintf(`Download snap "%s" (42) from channel "%s"`, snapName, setupChannel))

	// check link/start snap summary
	linkTask := ta[len(ta)-9]
	c.Check(linkTask.Summary(), Equals, fmt.Sprintf(`Make snap "%s" (42) available to the system`, snapName))
	startTask := ta[len(ta)-3]
	c.Check(startTask.Summary(), Equals, fmt.Sprintf(`Start snap "%s" (42) services`, snapName))

	// verify snap-setup in the task state
	var snapsup snapstate.SnapSetup
	err = task.Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)
	c.Assert(snapsup, DeepEquals, snapstate.SnapSetup{
		Channel:  setupChannel,
		UserID:   s.user.ID,
		SnapPath: filepath.Join(dirs.SnapBlobDir, snapFileName),
		DownloadInfo: &snap.DownloadInfo{
			DownloadURL: "https://some-server.com/some/path.snap",
			Size:        5,
		},
		SideInfo:  snapsup.SideInfo,
		Type:      snap.TypeApp,
		Version:   snapName + "Ver",
		PlugsOnly: true,
	})
	c.Assert(snapsup.SideInfo, DeepEquals, &snap.SideInfo{
		RealName: snapName,
		Revision: snap.R(42),
		SnapID:   snapID,
		Channel:  requestedChannel,
	})

	// verify snaps in the system state
	var snaps map[string]*snapstate.SnapState
	err = s.state.Get("snaps", &snaps)
	c.Assert(err, IsNil)

	snapst := snaps[snapName]
	c.Assert(snapst, NotNil)
	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.TrackingChannel, Equals, trackedChannel)
	c.Assert(snapst.CohortKey, Equals, "")
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: snapName,
		SnapID:   snapID,
		Revision: snap.R(42),
		Channel:  requestedChannel,
	}, nil))
	c.Assert(snapst.Required, Equals, false)
}

func (s *snapmgrTestSuite) TestInstallStartOrder(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "services-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	c.Check(snapstate.Installing(s.state), Equals, false)
	op := s.fakeBackend.ops.First("start-snap-services")
	c.Assert(op, NotNil)
	c.Assert(op, DeepEquals, &fakeOp{
		op:   "start-snap-services",
		path: filepath.Join(dirs.SnapMountDir, "services-snap/11"),
		// ordered to preserve after/before relation
		services: []string{"svc1", "svc3", "svc2"},
	})
}

func (s *snapmgrTestSuite) TestInstalling(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	c.Check(snapstate.Installing(s.state), Equals, false)

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel", Revision: snap.R(42)}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, 0, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	c.Check(snapstate.Installing(s.state), Equals, true)
}

func (s *snapmgrTestSuite) TestInstallFirstLocalRunThrough(c *C) {
	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	restoreInstallSize := snapstate.MockInstallSize(func(st *state.State, snaps []snapstate.MinimalInstallInfo, userID int, prqt snapstate.PrereqTracker) (uint64, error) {
		c.Fatalf("installSize shouldn't be hit with local install")
		return 0, nil
	})
	defer restoreInstallSize()

	s.state.Lock()
	defer s.state.Unlock()

	prqt := new(testPrereqTracker)

	mockSnap := makeTestSnap(c, `name: mock
version: 1.0`)
	chg := s.state.NewChange("install", "install a local snap")
	ts, info, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "mock"}, mockSnap, "", "", snapstate.Flags{}, prqt)
	c.Assert(err, IsNil)

	c.Assert(prqt.infos, HasLen, 1)
	c.Check(prqt.infos[0], DeepEquals, info)
	c.Check(prqt.missingProviderContentTagsCalls, Equals, 1)

	chg.AddAll(ts)

	// ensure the returned info is correct
	c.Check(info.SideInfo.RealName, Equals, "mock")
	c.Check(info.Version, Equals, "1.0")

	s.settle(c)

	expected := fakeOps{
		{
			// only local install was run, i.e. first actions are pseudo-action current
			op:  "current",
			old: "<no-current>",
		},
		{
			// and setup-snap
			op:    "setup-snap",
			name:  "mock",
			path:  mockSnap,
			revno: snap.R("x1"),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "mock/x1"),
			old:  "<no-old>",
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "mock"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "mock",
			revno: snap.R("x1"),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "mock",
				Revision: snap.R("x1"),
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "mock/x1"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "mock",
			revno: snap.R("x1"),
		},
		{
			op: "update-aliases",
		},
		{
			op:    "cleanup-trash",
			name:  "mock",
			revno: snap.R("x1"),
		},
	}

	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// verify snapSetup info
	var snapsup snapstate.SnapSetup
	task := ts.Tasks()[1]
	err = task.Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)
	c.Assert(snapsup, DeepEquals, snapstate.SnapSetup{
		SnapPath:  mockSnap,
		SideInfo:  snapsup.SideInfo,
		Type:      snap.TypeApp,
		Version:   "1.0",
		PlugsOnly: true,
	})
	c.Assert(snapsup.SideInfo, DeepEquals, &snap.SideInfo{
		RealName: "mock",
		Revision: snap.R(-1),
	})

	// verify snaps in the system state
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "mock", &snapst)
	c.Assert(err, IsNil)

	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: "mock",
		Channel:  "",
		Revision: snap.R(-1),
	}, nil))
	c.Assert(snapst.LocalRevision(), Equals, snap.R(-1))
}

func (s *snapmgrTestSuite) testInstallSubsequentLocalRunThrough(c *C, refreshAppAwarenessUX bool) {
	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "mock", &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "mock", Revision: snap.R(-2)},
		}),
		Current:  snap.R(-2),
		SnapType: "app",
	})

	mockSnap := makeTestSnap(c, `name: mock
version: 1.0
epoch: 1*
`)
	chg := s.state.NewChange("install", "install a local snap")
	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "mock"}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	expected := fakeOps{
		{
			op:  "current",
			old: filepath.Join(dirs.SnapMountDir, "mock/x2"),
		},
		{
			op:    "setup-snap",
			name:  "mock",
			path:  mockSnap,
			revno: snap.R("x3"),
		},
	}
	// aliases removal is skipped when refresh-app-awareness-ux is enabled
	if !refreshAppAwarenessUX {
		expected = append(expected, fakeOp{
			op:   "remove-snap-aliases",
			name: "mock",
		})
	}
	expected = append(expected, fakeOps{
		{
			op:          "run-inhibit-snap-for-unlink",
			name:        "mock",
			inhibitHint: "refresh",
		},
		{
			op:                 "unlink-snap",
			path:               filepath.Join(dirs.SnapMountDir, "mock/x2"),
			unlinkSkipBinaries: refreshAppAwarenessUX,
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "mock/x3"),
			old:  filepath.Join(dirs.SnapMountDir, "mock/x2"),
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "mock"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "mock",
			revno: snap.R(-3),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "mock",
				Revision: snap.R(-3),
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "mock/x3"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "mock",
			revno: snap.R("x3"),
		},
		{
			op: "update-aliases",
		},
		{
			op:    "cleanup-trash",
			name:  "mock",
			revno: snap.R("x3"),
		},
	}...)

	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	// verify snapSetup info
	var snapsup snapstate.SnapSetup
	task := ts.Tasks()[1]
	err = task.Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)
	c.Assert(snapsup, DeepEquals, snapstate.SnapSetup{
		SnapPath:                        mockSnap,
		SideInfo:                        snapsup.SideInfo,
		Type:                            snap.TypeApp,
		Version:                         "1.0",
		PlugsOnly:                       true,
		PreUpdateKernelModuleComponents: []*snap.ComponentSideInfo{},
	})
	c.Assert(snapsup.SideInfo, DeepEquals, &snap.SideInfo{
		RealName: "mock",
		Revision: snap.R(-3),
	})

	// verify snaps in the system state
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "mock", &snapst)
	c.Assert(err, IsNil)

	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.Sequence.Revisions, HasLen, 2)
	c.Assert(snapst.CurrentSideInfo(), DeepEquals, &snap.SideInfo{
		RealName: "mock",
		Channel:  "",
		Revision: snap.R(-3),
	})
	c.Assert(snapst.LocalRevision(), Equals, snap.R(-3))
}

func (s *snapmgrTestSuite) TestInstallSubsequentLocalRunThrough(c *C) {
	s.testInstallSubsequentLocalRunThrough(c, false)
}

func (s *snapmgrTestSuite) TestInstallSubsequentLocalRunThroughSkipBinaries(c *C) {
	s.enableRefreshAppAwarenessUX()
	s.testInstallSubsequentLocalRunThrough(c, true)
}

func (s *snapmgrTestSuite) TestInstallOldSubsequentLocalRunThrough(c *C) {
	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "mock", &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "mock", Revision: snap.R(100001)},
		}),
		Current:  snap.R(100001),
		SnapType: "app",
	})

	mockSnap := makeTestSnap(c, `name: mock
version: 1.0
epoch: 1*
`)
	chg := s.state.NewChange("install", "install a local snap")
	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "mock"}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	expected := fakeOps{
		{
			// ensure only local install was run, i.e. first action is pseudo-action current
			op:  "current",
			old: filepath.Join(dirs.SnapMountDir, "mock/100001"),
		},
		{
			// and setup-snap
			op:    "setup-snap",
			name:  "mock",
			path:  mockSnap,
			revno: snap.R("x1"),
		},
		{
			op:   "remove-snap-aliases",
			name: "mock",
		},
		{
			op:          "run-inhibit-snap-for-unlink",
			name:        "mock",
			inhibitHint: "refresh",
		},
		{
			op:   "unlink-snap",
			path: filepath.Join(dirs.SnapMountDir, "mock/100001"),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "mock/x1"),
			old:  filepath.Join(dirs.SnapMountDir, "mock/100001"),
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "mock"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "mock",
			revno: snap.R("x1"),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "mock",
				Revision: snap.R("x1"),
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "mock/x1"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "mock",
			revno: snap.R("x1"),
		},
		{
			op: "update-aliases",
		},
		{
			// and cleanup
			op:    "cleanup-trash",
			name:  "mock",
			revno: snap.R("x1"),
		},
	}
	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Check(s.fakeBackend.ops, DeepEquals, expected)

	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "mock", &snapst)
	c.Assert(err, IsNil)

	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.Sequence.Revisions, HasLen, 2)
	c.Assert(snapst.CurrentSideInfo(), DeepEquals, &snap.SideInfo{
		RealName: "mock",
		Channel:  "",
		Revision: snap.R(-1),
	})
	c.Assert(snapst.LocalRevision(), Equals, snap.R(-1))
}

func (s *snapmgrTestSuite) TestInstallPathWithMetadataRunThrough(c *C) {
	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	s.state.Lock()
	defer s.state.Unlock()

	someSnap := makeTestSnap(c, `name: orig-name
version: 1.0`)
	chg := s.state.NewChange("install", "install a local snap")

	si := &snap.SideInfo{
		RealName: "some-snap",
		SnapID:   "some-snap-id",
		Revision: snap.R(42),
	}
	ts, _, err := snapstate.InstallPath(s.state, si, someSnap, "", "", snapstate.Flags{Required: true}, nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure only local install was run, i.e. first actions are pseudo-action current
	c.Assert(s.fakeBackend.ops.Ops(), HasLen, 11)
	c.Check(s.fakeBackend.ops[0].op, Equals, "current")
	c.Check(s.fakeBackend.ops[0].old, Equals, "<no-current>")
	// and setup-snap
	c.Check(s.fakeBackend.ops[1].op, Equals, "setup-snap")
	c.Check(s.fakeBackend.ops[1].name, Equals, "some-snap")
	c.Check(s.fakeBackend.ops[1].path, Matches, `.*/orig-name_1.0_all.snap`)
	c.Check(s.fakeBackend.ops[1].revno, Equals, snap.R(42))

	c.Check(s.fakeBackend.ops[5].op, Equals, "candidate")
	c.Check(s.fakeBackend.ops[5].sinfo, DeepEquals, *si)
	c.Check(s.fakeBackend.ops[6].op, Equals, "link-snap")
	c.Check(s.fakeBackend.ops[6].path, Equals, filepath.Join(dirs.SnapMountDir, "some-snap/42"))
	c.Check(s.fakeBackend.ops[7].op, Equals, "maybe-set-next-boot")

	// verify snapSetup info
	var snapsup snapstate.SnapSetup
	task := ts.Tasks()[0]
	err = task.Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)
	c.Assert(snapsup, DeepEquals, snapstate.SnapSetup{
		SnapPath: someSnap,
		SideInfo: snapsup.SideInfo,
		Flags: snapstate.Flags{
			Required: true,
		},
		Type:      snap.TypeApp,
		Version:   "1.0",
		PlugsOnly: true,
	})
	c.Assert(snapsup.SideInfo, DeepEquals, si)

	// verify snaps in the system state
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)

	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.TrackingChannel, Equals, "")
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(si, nil))
	c.Assert(snapst.LocalRevision().Unset(), Equals, true)
	c.Assert(snapst.Required, Equals, true)
}

func (s *snapmgrTestSuite) TestInstallPathSkipConfigure(c *C) {
	r := release.MockOnClassic(false)
	defer r()

	makeInstalledMockCoreSnap(c)

	// using MockSnap, we want to read the bits on disk
	snapstate.MockSnapReadInfo(snap.ReadInfo)

	s.state.Lock()
	defer s.state.Unlock()

	s.prepareGadget(c)

	snapPath := makeTestSnap(c, "name: some-snap\nversion: 1.0")

	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(1)}, snapPath, "", "edge", snapstate.Flags{SkipConfigure: true}, nil)
	c.Assert(err, IsNil)

	snapsup, err := snapstate.TaskSnapSetup(ts.Tasks()[0])
	c.Assert(err, IsNil)
	// SkipConfigure is consumed and consulted when creating the taskset
	// but is not copied into SnapSetup
	c.Check(snapsup.Flags.SkipConfigure, Equals, false)
}

func (s *snapmgrTestSuite) TestInstallWithoutCoreRunThrough1(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// pretend we don't have core
	snapstate.Set(s.state, "core", nil)

	chg := s.state.NewChange("install", "install a snap on a system without core")
	opts := &snapstate.RevisionOptions{Channel: "some-channel", Revision: snap.R(42)}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	c.Check(s.fakeStore.downloads, DeepEquals, []fakeDownload{
		{
			macaroon: s.user.StoreMacaroon,
			name:     "core",
			target:   filepath.Join(dirs.SnapBlobDir, "core_11.snap"),
		},
		{
			macaroon: s.user.StoreMacaroon,
			name:     "some-snap",
			target:   filepath.Join(dirs.SnapBlobDir, "some-snap_42.snap"),
		}})
	expected := fakeOps{
		// we check the snap
		{
			op:     "storesvc-snap-action",
			userID: 1,
		},
		{
			op: "storesvc-snap-action:action",
			action: store.SnapAction{
				Action:       "install",
				InstanceName: "some-snap",
				Revision:     snap.R(42),
				Channel:      "some-channel",
			},
			revno:  snap.R(42),
			userID: 1,
		},
		// then we check core because its not installed already
		// and continue with that
		{
			op:     "storesvc-snap-action",
			userID: 1,
		},
		{
			op: "storesvc-snap-action:action",
			action: store.SnapAction{
				Action:       "install",
				InstanceName: "core",
				Channel:      "stable",
			},
			revno:  snap.R(11),
			userID: 1,
		},
		{
			op:   "storesvc-download",
			name: "core",
		},
		{
			op:    "validate-snap:Doing",
			name:  "core",
			revno: snap.R(11),
		},
		{
			op:  "current",
			old: "<no-current>",
		},
		{
			op:   "open-snap-file",
			path: filepath.Join(dirs.SnapBlobDir, "core_11.snap"),
			sinfo: snap.SideInfo{
				RealName: "core",
				Channel:  "stable",
				SnapID:   "core-id",
				Revision: snap.R(11),
			},
		},
		{
			op:    "setup-snap",
			name:  "core",
			path:  filepath.Join(dirs.SnapBlobDir, "core_11.snap"),
			revno: snap.R(11),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "core/11"),
			old:  "<no-old>",
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "core"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "core",
			revno: snap.R(11),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "core",
				Channel:  "stable",
				SnapID:   "core-id",
				Revision: snap.R(11),
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "core/11"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "core",
			revno: snap.R(11),
		},
		{
			op: "update-aliases",
		},
		// after core is in place continue with the snap
		{
			op:   "storesvc-download",
			name: "some-snap",
		},
		{
			op:    "validate-snap:Doing",
			name:  "some-snap",
			revno: snap.R(42),
		},
		{
			op:  "current",
			old: "<no-current>",
		},
		{
			op:   "open-snap-file",
			path: filepath.Join(dirs.SnapBlobDir, "some-snap_42.snap"),
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Revision: snap.R(42),
				Channel:  "some-channel",
			},
		},
		{
			op:    "setup-snap",
			name:  "some-snap",
			path:  filepath.Join(dirs.SnapBlobDir, "some-snap_42.snap"),
			revno: snap.R(42),
		},
		{
			op:   "copy-data",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/42"),
			old:  "<no-old>",
		},
		{
			op:   "setup-snap-save-data",
			path: filepath.Join(dirs.SnapDataSaveDir, "some-snap"),
		},
		{
			op:    "setup-profiles:Doing",
			name:  "some-snap",
			revno: snap.R(42),
		},
		{
			op: "candidate",
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Revision: snap.R(42),
				Channel:  "some-channel",
			},
		},
		{
			op:   "link-snap",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/42"),
		},
		{
			op: "maybe-set-next-boot",
		},
		{
			op:    "auto-connect:Doing",
			name:  "some-snap",
			revno: snap.R(42),
		},
		{
			op: "update-aliases",
		},
		// cleanups order is random
		{
			op:    "cleanup-trash",
			name:  "core",
			revno: snap.R(42),
		},
		{
			op:    "cleanup-trash",
			name:  "some-snap",
			revno: snap.R(42),
		},
	}
	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	// compare the details without the cleanup tasks, the order is random
	// as they run in parallel
	opsLenWithoutCleanups := len(s.fakeBackend.ops) - 2
	c.Assert(s.fakeBackend.ops[:opsLenWithoutCleanups], DeepEquals, expected[:opsLenWithoutCleanups])

	// verify core in the system state
	var snaps map[string]*snapstate.SnapState
	err = s.state.Get("snaps", &snaps)
	c.Assert(err, IsNil)

	snapst := snaps["core"]
	c.Assert(snapst, NotNil)
	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.TrackingChannel, Equals, "latest/stable")
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: "core",
		Channel:  "stable",
		SnapID:   "core-id",
		Revision: snap.R(11),
	}, nil))
}

func (s *snapmgrTestSuite) TestInstallWithoutCoreTwoSnapsRunThrough(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	restore := snapstate.MockPrerequisitesRetryTimeout(10 * time.Millisecond)
	defer restore()

	// pretend we don't have core
	snapstate.Set(s.state, "core", nil)

	chg1 := s.state.NewChange("install", "install snap 1")
	opts := &snapstate.RevisionOptions{Channel: "some-channel", Revision: snap.R(42)}
	ts1, err := snapstate.Install(context.Background(), s.state, "snap1", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg1.AddAll(ts1)

	chg2 := s.state.NewChange("install", "install snap 2")
	opts = &snapstate.RevisionOptions{Channel: "some-other-channel", Revision: snap.R(21)}
	ts2, err := snapstate.Install(context.Background(), s.state, "snap2", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg2.AddAll(ts2)

	s.settle(c)

	// ensure all our tasks ran and core was only installed once
	c.Assert(chg1.Err(), IsNil)
	c.Assert(chg2.Err(), IsNil)

	c.Assert(chg1.IsReady(), Equals, true)
	c.Assert(chg2.IsReady(), Equals, true)

	// order in which the changes run is random
	if len(chg1.Tasks()) < len(chg2.Tasks()) {
		chg1, chg2 = chg2, chg1
	}
	c.Assert(taskKinds(chg1.Tasks()), HasLen, 29)
	c.Assert(taskKinds(chg2.Tasks()), HasLen, 15)

	// FIXME: add helpers and do a DeepEquals here for the operations
}

func (s *snapmgrTestSuite) TestInstallWithoutCoreTwoSnapsWithFailureRunThrough(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// slightly longer retry timeout to avoid deadlock when we
	// trigger a retry quickly that the link snap for core does
	// not have a chance to run
	restore := snapstate.MockPrerequisitesRetryTimeout(40 * time.Millisecond)
	defer restore()

	// Two changes are created, the first will fails, the second will
	// be fine. The order of what change runs first is random, the
	// first change will also install core in its own lane. This test
	// ensures that core gets installed and there are no conflicts
	// even if core already got installed from the first change.
	//
	// It runs multiple times so that both possible cases get a chance
	// to run
	for i := 0; i < 5; i++ {
		// start clean
		snapstate.Set(s.state, "core", nil)
		snapstate.Set(s.state, "snap2", nil)

		// chg1 has an error
		chg1 := s.state.NewChange("install", "install snap 1")
		opts := &snapstate.RevisionOptions{Channel: "some-channel", Revision: snap.R(42)}
		ts1, err := snapstate.Install(context.Background(), s.state, "snap1", opts, s.user.ID, snapstate.Flags{})
		c.Assert(err, IsNil)
		chg1.AddAll(ts1)

		tasks := ts1.Tasks()
		last := tasks[len(tasks)-1]
		terr := s.state.NewTask("error-trigger", "provoking total undo")
		terr.WaitFor(last)
		chg1.AddTask(terr)

		// chg2 is good
		chg2 := s.state.NewChange("install", "install snap 2")
		opts = &snapstate.RevisionOptions{Channel: "some-other-channel", Revision: snap.R(21)}
		ts2, err := snapstate.Install(context.Background(), s.state, "snap2", opts, s.user.ID, snapstate.Flags{})
		c.Assert(err, IsNil)
		chg2.AddAll(ts2)

		s.state.Unlock()
		defer s.state.Lock()

		// we use our own settle as we need a bigger timeout
		err = s.o.Settle(testutil.HostScaledTimeout(15 * time.Second))
		c.Assert(err, IsNil)

		s.state.Lock()
		defer s.state.Unlock()

		// ensure expected change states
		c.Check(chg1.Status(), Equals, state.ErrorStatus)
		c.Check(chg2.Status(), Equals, state.DoneStatus)

		// ensure we have both core and snap2
		var snapst snapstate.SnapState
		err = snapstate.Get(s.state, "core", &snapst)
		c.Assert(err, IsNil)
		c.Assert(snapst.Active, Equals, true)
		c.Assert(snapst.Sequence.Revisions, HasLen, 1)
		c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
			RealName: "core",
			SnapID:   "core-id",
			Channel:  "stable",
			Revision: snap.R(11),
		}, nil))

		var snapst2 snapstate.SnapState
		err = snapstate.Get(s.state, "snap2", &snapst2)
		c.Assert(err, IsNil)
		c.Assert(snapst2.Active, Equals, true)
		c.Assert(snapst2.Sequence.Revisions, HasLen, 1)
		c.Assert(snapst2.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
			RealName: "snap2",
			SnapID:   "snap2-id",
			Channel:  "some-other-channel",
			Revision: snap.R(21),
		}, nil))
	}
}

type behindYourBackStore struct {
	*fakeStore
	state *state.State

	coreInstallRequested bool
	coreInstalled        bool
	chg                  *state.Change
}

func (s *behindYourBackStore) SnapAction(ctx context.Context, currentSnaps []*store.CurrentSnap, actions []*store.SnapAction, assertQuery store.AssertionQuery, user *auth.UserState, opts *store.RefreshOptions) ([]store.SnapActionResult, []store.AssertionResult, error) {
	if assertQuery != nil {
		panic("no assertion query support")
	}

	if len(actions) == 1 && actions[0].Action == "install" && actions[0].InstanceName == "core" {
		s.state.Lock()
		if !s.coreInstallRequested {
			s.coreInstallRequested = true
			snapsup := &snapstate.SnapSetup{
				SideInfo: &snap.SideInfo{
					RealName: "core",
				},
			}
			t := s.state.NewTask("prepare", "prepare core")
			t.Set("snap-setup", snapsup)
			s.chg = s.state.NewChange("install", "install core")
			s.chg.AddAll(state.NewTaskSet(t))
		}
		if s.chg != nil && !s.coreInstalled {
			// marks change ready but also
			// tasks need to also be marked cleaned
			for _, t := range s.chg.Tasks() {
				t.SetStatus(state.DoneStatus)
				t.SetClean()
			}
			snapstate.Set(s.state, "core", &snapstate.SnapState{
				Active: true,
				Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
					{RealName: "core", Revision: snap.R(1)},
				}),
				Current:  snap.R(1),
				SnapType: "os",
			})
			s.coreInstalled = true
		}
		s.state.Unlock()
	}

	return s.fakeStore.SnapAction(ctx, currentSnaps, actions, nil, user, opts)
}

// this test the scenario that some-snap gets installed and during the
// install (when unlocking for the store info call for core) an
// explicit "snap install core" happens. In this case the snapstate
// will return a change conflict. we handle this via a retry, ensure
// this is actually what happens.
func (s *snapmgrTestSuite) TestInstallWithoutCoreConflictingInstall(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	restore := snapstate.MockPrerequisitesRetryTimeout(10 * time.Millisecond)
	defer restore()

	snapstate.ReplaceStore(s.state, &behindYourBackStore{fakeStore: s.fakeStore, state: s.state})

	// pretend we don't have core
	snapstate.Set(s.state, "core", nil)

	// now install a snap that will pull in core
	chg := s.state.NewChange("install", "install a snap on a system without core")
	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	prereq := ts.Tasks()[0]
	c.Assert(prereq.Kind(), Equals, "prerequisites")
	c.Check(prereq.AtTime().IsZero(), Equals, true)

	s.state.Unlock()
	defer s.state.Lock()

	// start running the change, this will trigger the
	// prerequisites task, which will trigger the install of core
	// and also call our mock store which will generate a parallel
	// change
	s.se.Ensure()
	s.se.Wait()

	// change is not ready yet, because the prerequisites triggered
	// a state.Retry{} because of the conflicting change
	c.Assert(chg.IsReady(), Equals, false)

	s.state.Lock()
	defer s.state.Unlock()

	// marked for retry
	c.Check(prereq.AtTime().IsZero(), Equals, false)
	c.Check(prereq.Status().Ready(), Equals, false)

	// retry interval is 10ms so 20ms should be plenty of time
	time.Sleep(20 * time.Millisecond)
	s.settle(c)
	// chg got retried, core is now installed, things are good
	c.Assert(chg.IsReady(), Equals, true)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	// verify core in the system state
	var snaps map[string]*snapstate.SnapState
	err = s.state.Get("snaps", &snaps)
	c.Assert(err, IsNil)

	snapst := snaps["core"]
	c.Assert(snapst, NotNil)
	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: "core",
		Revision: snap.R(1),
	}, nil))

	snapst = snaps["some-snap"]
	c.Assert(snapst, NotNil)
	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: "some-snap",
		SnapID:   "some-snap-id",
		Channel:  "some-channel",
		Revision: snap.R(11),
	}, nil))
}

func (s *snapmgrTestSuite) TestInstallDefaultProviderRunThrough(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, contentStore{fakeStore: s.fakeStore, state: s.state})

	repo := interfaces.NewRepository()
	ifacerepo.Replace(s.state, repo)

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "stable", Revision: snap.R(42)}
	ts, err := snapstate.Install(context.Background(), s.state, "snap-content-plug", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	expected := fakeOps{{
		op:     "storesvc-snap-action",
		userID: 1,
	}, {
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:       "install",
			InstanceName: "snap-content-plug",
			Revision:     snap.R(42),
			Channel:      "stable",
		},
		revno:  snap.R(42),
		userID: 1,
	}, {
		op:     "storesvc-snap-action",
		userID: 1,
	}, {
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:       "install",
			InstanceName: "snap-content-slot",
			Channel:      "stable",
		},
		revno:  snap.R(11),
		userID: 1,
	}, {
		op:   "storesvc-download",
		name: "snap-content-slot",
	}, {
		op:    "validate-snap:Doing",
		name:  "snap-content-slot",
		revno: snap.R(11),
	}, {
		op:  "current",
		old: "<no-current>",
	}, {
		op:   "open-snap-file",
		path: filepath.Join(dirs.SnapBlobDir, "snap-content-slot_11.snap"),
		sinfo: snap.SideInfo{
			RealName: "snap-content-slot",
			Channel:  "stable",
			SnapID:   "snap-content-slot-id",
			Revision: snap.R(11),
		},
	}, {
		op:    "setup-snap",
		name:  "snap-content-slot",
		path:  filepath.Join(dirs.SnapBlobDir, "snap-content-slot_11.snap"),
		revno: snap.R(11),
	}, {
		op:   "copy-data",
		path: filepath.Join(dirs.SnapMountDir, "snap-content-slot/11"),
		old:  "<no-old>",
	}, {
		op:   "setup-snap-save-data",
		path: filepath.Join(dirs.SnapDataSaveDir, "snap-content-slot"),
	}, {
		op:    "setup-profiles:Doing",
		name:  "snap-content-slot",
		revno: snap.R(11),
	}, {
		op: "candidate",
		sinfo: snap.SideInfo{
			RealName: "snap-content-slot",
			Channel:  "stable",
			SnapID:   "snap-content-slot-id",
			Revision: snap.R(11),
		},
	}, {
		op:   "link-snap",
		path: filepath.Join(dirs.SnapMountDir, "snap-content-slot/11"),
	}, {
		op: "maybe-set-next-boot",
	}, {
		op:    "auto-connect:Doing",
		name:  "snap-content-slot",
		revno: snap.R(11),
	}, {
		op: "update-aliases",
	}, {
		op:   "storesvc-download",
		name: "snap-content-plug",
	}, {
		op:    "validate-snap:Doing",
		name:  "snap-content-plug",
		revno: snap.R(42),
	}, {
		op:  "current",
		old: "<no-current>",
	}, {
		op:   "open-snap-file",
		path: filepath.Join(dirs.SnapBlobDir, "snap-content-plug_42.snap"),
		sinfo: snap.SideInfo{
			RealName: "snap-content-plug",
			SnapID:   "snap-content-plug-id",
			Revision: snap.R(42),
			Channel:  "stable",
		},
	}, {
		op:    "setup-snap",
		name:  "snap-content-plug",
		path:  filepath.Join(dirs.SnapBlobDir, "snap-content-plug_42.snap"),
		revno: snap.R(42),
	}, {
		op:   "copy-data",
		path: filepath.Join(dirs.SnapMountDir, "snap-content-plug/42"),
		old:  "<no-old>",
	}, {
		op:   "setup-snap-save-data",
		path: filepath.Join(dirs.SnapDataSaveDir, "snap-content-plug"),
	}, {
		op:    "setup-profiles:Doing",
		name:  "snap-content-plug",
		revno: snap.R(42),
	}, {
		op: "candidate",
		sinfo: snap.SideInfo{
			RealName: "snap-content-plug",
			SnapID:   "snap-content-plug-id",
			Revision: snap.R(42),
			Channel:  "stable",
		},
	}, {
		op:   "link-snap",
		path: filepath.Join(dirs.SnapMountDir, "snap-content-plug/42"),
	}, {
		op: "maybe-set-next-boot",
	}, {
		op:    "auto-connect:Doing",
		name:  "snap-content-plug",
		revno: snap.R(42),
	}, {
		op: "update-aliases",
	}, {
		op:    "cleanup-trash",
		name:  "snap-content-plug",
		revno: snap.R(42),
	}, {
		op:    "cleanup-trash",
		name:  "snap-content-slot",
		revno: snap.R(11),
	},
	}
	// snap and default provider are installed in parallel so we can't
	// do a simple c.Check(ops, DeepEquals, fakeOps{...})
	c.Check(len(s.fakeBackend.ops), Equals, len(expected))
	for _, op := range expected {
		c.Assert(s.fakeBackend.ops, testutil.DeepContains, op)
	}
	for _, op := range s.fakeBackend.ops {
		c.Assert(expected, testutil.DeepContains, op)
	}
}

func (s *snapmgrTestSuite) TestInstallDefaultProviderCompat(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, contentStore{fakeStore: s.fakeStore, state: s.state})

	repo := interfaces.NewRepository()
	ifacerepo.Replace(s.state, repo)

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel", Revision: snap.R(42)}
	ts, err := snapstate.Install(context.Background(), s.state, "snap-content-plug-compat", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	// and both circular snaps got linked
	c.Check(s.fakeBackend.ops, testutil.DeepContains, fakeOp{
		op:   "link-snap",
		path: filepath.Join(dirs.SnapMountDir, "snap-content-plug-compat/42"),
	})
	c.Check(s.fakeBackend.ops, testutil.DeepContains, fakeOp{
		op:   "link-snap",
		path: filepath.Join(dirs.SnapMountDir, "snap-content-slot/11"),
	})
}

func (s *snapmgrTestSuite) TestInstallDiskSpaceError(c *C) {
	restore := snapstate.MockOsutilCheckFreeSpace(func(string, uint64) error { return &osutil.NotEnoughDiskSpaceError{} })
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.check-disk-space-install", true)
	tr.Commit()

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	diskSpaceErr := err.(*snapstate.InsufficientSpaceError)
	c.Assert(diskSpaceErr, ErrorMatches, `insufficient space in .* to perform "install" change for the following snaps: some-snap`)
	c.Check(diskSpaceErr.Path, Equals, filepath.Join(dirs.GlobalRootDir, "/var/lib/snapd"))
	c.Check(diskSpaceErr.Snaps, DeepEquals, []string{"some-snap"})
}

func (s *snapmgrTestSuite) TestInstallSizeError(c *C) {
	restore := snapstate.MockInstallSize(func(st *state.State, snaps []snapstate.MinimalInstallInfo, userID int, prqt snapstate.PrereqTracker) (uint64, error) {
		return 0, fmt.Errorf("boom")
	})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.check-disk-space-install", true)
	tr.Commit()

	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Check(err, ErrorMatches, `boom`)
}

func (s *snapmgrTestSuite) TestInstallPathWithLayoutsChecksFeatureFlag(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// When layouts are disabled we cannot install a local snap depending on the feature.
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.layouts", false)
	tr.Commit()

	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
layout:
 /usr:
  bind: $SNAP/usr
`)
	_, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(8)}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.layouts' to true")

	// When layouts are enabled we can install a local snap depending on the feature.
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.layouts", true)
	tr.Commit()

	_, _, err = snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(8)}, mockSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallPathWithMetadataChannelSwitchKernel(c *C) {
	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	s.state.Lock()
	defer s.state.Unlock()

	// snapd cannot be installed unless the model uses a base snap
	r := snapstatetest.MockDeviceModel(ModelWithKernelTrack("18"))
	defer r()
	snapstate.Set(s.state, "kernel", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "kernel", Revision: snap.R(11)},
		}),
		TrackingChannel: "18/stable",
		Current:         snap.R(11),
		Active:          true,
	})

	someSnap := makeTestSnap(c, `name: kernel
version: 1.0`)
	si := &snap.SideInfo{
		RealName: "kernel",
		SnapID:   "kernel-id",
		Revision: snap.R(42),
		Channel:  "some-channel",
	}
	_, _, err := snapstate.InstallPath(s.state, si, someSnap, "", "some-channel", snapstate.Flags{Required: true}, nil)
	c.Assert(err, ErrorMatches, `cannot switch from kernel track "18" as specified for the \(device\) model to "some-channel"`)
}

func (s *snapmgrTestSuite) TestInstallPathWithMetadataChannelSwitchGadget(c *C) {
	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	s.state.Lock()
	defer s.state.Unlock()

	// snapd cannot be installed unless the model uses a base snap
	r := snapstatetest.MockDeviceModel(ModelWithGadgetTrack("18"))
	defer r()
	snapstate.Set(s.state, "brand-gadget", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "brand-gadget", Revision: snap.R(11)},
		}),
		TrackingChannel: "18/stable",
		Current:         snap.R(11),
		Active:          true,
	})

	someSnap := makeTestSnap(c, `name: brand-gadget
version: 1.0`)
	si := &snap.SideInfo{
		RealName: "brand-gadget",
		SnapID:   "brand-gadget-id",
		Revision: snap.R(42),
		Channel:  "some-channel",
	}
	_, _, err := snapstate.InstallPath(s.state, si, someSnap, "", "some-channel", snapstate.Flags{Required: true}, nil)
	c.Assert(err, ErrorMatches, `cannot switch from gadget track "18" as specified for the \(device\) model to "some-channel"`)
}

func (s *snapmgrTestSuite) TestInstallLayoutsChecksFeatureFlag(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// Layouts are now enabled by default.
	opts := &snapstate.RevisionOptions{Channel: "channel-for-layout"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	// Layouts can be explicitly disabled.
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.layouts", false)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.layouts' to true")

	// Layouts can be explicitly enabled.
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.layouts", true)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	// The default empty value now means "enabled".
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.layouts", "")
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	// Layouts are enabled when the controlling flag is reset to nil.
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.layouts", nil)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallUserDaemonsChecksFeatureFlag(c *C) {
	if release.ReleaseInfo.ID == "ubuntu" && release.ReleaseInfo.VersionID == "14.04" {
		c.Skip("Ubuntu 14.04 does not support user daemons")
	}

	s.state.Lock()
	defer s.state.Unlock()

	// User daemons are disabled by default.
	opts := &snapstate.RevisionOptions{Channel: "channel-for-user-daemon"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.user-daemons' to true")

	// User daemons can be explicitly enabled.
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.user-daemons", true)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	// User daemons can be explicitly disabled.
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.user-daemons", false)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.user-daemons' to true")

	// The default empty value means "disabled"".
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.user-daemons", "")
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.user-daemons' to true")

	// User daemons are disabled when the controlling flag is reset to nil.
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.user-daemons", nil)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.user-daemons' to true")
}

func (s *snapmgrTestSuite) TestInstallUserDaemonsUnsupportedOnTrusty(c *C) {
	restore := release.MockReleaseInfo(&release.OS{ID: "ubuntu", VersionID: "14.04"})
	defer restore()
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.user-daemons", true)
	tr.Commit()

	// Even with the experimental.user-daemons flag set, user
	// daemons are not supported on Trusty
	opts := &snapstate.RevisionOptions{Channel: "channel-for-user-daemon"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "user session daemons are not supported on this release")
}

func (s *snapmgrTestSuite) TestInstallUserDaemonsFirmwareUpdater(c *C) {
	restore := release.MockReleaseInfo(&release.OS{ID: "ubuntu", VersionID: "22.04"})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.user-daemons", false)
	tr.Commit()

	// Installing snapd-desktop-integration is possible even when
	// user-daemons is disabled.
	opts := &snapstate.RevisionOptions{Channel: "channel-for-user-daemon"}
	_, err := snapstate.Install(context.Background(), s.state, "firmware-updater", opts, s.user.ID, snapstate.Flags{})
	c.Check(err, IsNil)

	// However, it will still fail on systems that do not support
	// the user-daemons feature at all.
	restore = release.MockReleaseInfo(&release.OS{ID: "ubuntu", VersionID: "14.04"})
	defer restore()
	_, err = snapstate.Install(context.Background(), s.state, "firmware-updater", opts, s.user.ID, snapstate.Flags{})
	c.Check(err, ErrorMatches, "user session daemons are not supported on this release")
}

func (s *snapmgrTestSuite) TestInstallUserDaemonsSnapdDesktopIntegration(c *C) {
	restore := release.MockReleaseInfo(&release.OS{ID: "ubuntu", VersionID: "22.04"})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.user-daemons", false)
	tr.Commit()

	// Installing snapd-desktop-integration is possible even when
	// user-daemons is disabled.
	opts := &snapstate.RevisionOptions{Channel: "channel-for-user-daemon"}
	_, err := snapstate.Install(context.Background(), s.state, "snapd-desktop-integration", opts, s.user.ID, snapstate.Flags{})
	c.Check(err, IsNil)

	// However, it will still fail on systems that do not support
	// the user-daemons feature at all.
	restore = release.MockReleaseInfo(&release.OS{ID: "ubuntu", VersionID: "14.04"})
	defer restore()
	_, err = snapstate.Install(context.Background(), s.state, "snapd-desktop-integration", opts, s.user.ID, snapstate.Flags{})
	c.Check(err, ErrorMatches, "user session daemons are not supported on this release")
}

func (s *snapmgrTestSuite) TestInstallUserDaemonsPromptingClient(c *C) {
	restore := release.MockReleaseInfo(&release.OS{ID: "ubuntu", VersionID: "22.04"})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.user-daemons", false)
	tr.Commit()

	// Installing prompting-client is possible even when
	// user-daemons is disabled.
	opts := &snapstate.RevisionOptions{Channel: "channel-for-user-daemon"}
	_, err := snapstate.Install(context.Background(), s.state, "prompting-client", opts, s.user.ID, snapstate.Flags{})
	c.Check(err, IsNil)

	// However, it will still fail on systems that do not support
	// the user-daemons feature at all.
	restore = release.MockReleaseInfo(&release.OS{ID: "ubuntu", VersionID: "14.04"})
	defer restore()
	_, err = snapstate.Install(context.Background(), s.state, "prompting-client", opts, s.user.ID, snapstate.Flags{})
	c.Check(err, ErrorMatches, "user session daemons are not supported on this release")
}

func (s *snapmgrTestSuite) TestInstallDbusActivationChecksFeatureFlag(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// D-Bus activation is disabled by default.
	opts := &snapstate.RevisionOptions{Channel: "channel-for-dbus-activation"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	// D-Bus activation can be explicitly enabled.
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.dbus-activation", true)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	// D-Bus activation can be explicitly disabled.
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.dbus-activation", false)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.dbus-activation' to true")

	// The default empty value means "enabled"
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.dbus-activation", "")
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	// D-Bus activation is enabled when the controlling flag is reset to nil.
	tr = config.NewTransaction(s.state)
	tr.Set("core", "experimental.dbus-activation", nil)
	tr.Commit()
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallValidatesInstanceNames(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	_, err := snapstate.Install(context.Background(), s.state, "foo--invalid", nil, 0, snapstate.Flags{})
	c.Assert(err, ErrorMatches, `invalid instance name: invalid snap name: "foo--invalid"`)

	_, err = snapstate.Install(context.Background(), s.state, "foo_123_456", nil, 0, snapstate.Flags{})
	c.Assert(err, ErrorMatches, `invalid instance name: invalid instance key: "123_456"`)

	_, _, err = snapstate.InstallMany(s.state, []string{"foo--invalid"}, nil, 0, nil)
	c.Assert(err, ErrorMatches, `invalid instance name: invalid snap name: "foo--invalid"`)

	_, _, err = snapstate.InstallMany(s.state, []string{"foo_123_456"}, nil, 0, nil)
	c.Assert(err, ErrorMatches, `invalid instance name: invalid instance key: "123_456"`)

	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
epoch: 1*
`)
	si := snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(8)}
	_, _, err = snapstate.InstallPath(s.state, &si, mockSnap, "some-snap_123_456", "", snapstate.Flags{}, nil)
	c.Assert(err, ErrorMatches, `invalid instance name: invalid instance key: "123_456"`)
}

func (s *snapmgrTestSuite) TestInstallFailsWhenClassicSnapsAreNotSupported(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// this needs doing because dirs depends on the release info
	c.Check(os.RemoveAll(dirs.SnapMountDir), IsNil)
	dirstest.MustMockAltSnapMountDir(dirs.GlobalRootDir)
	dirs.SetRootDir(dirs.GlobalRootDir)

	opts := &snapstate.RevisionOptions{Channel: "channel-for-classic"}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{Classic: true})
	c.Assert(err, ErrorMatches, "classic confinement requires snaps under /snap or symlink from /snap to "+dirs.SnapMountDir)
}

func (s *snapmgrTestSuite) TestInstallUndoRunThroughUndoContextOptional(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap-no-install-record", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	tasks := ts.Tasks()
	last := tasks[len(tasks)-1]
	// validity
	c.Assert(last.Lanes(), HasLen, 1)
	terr := s.state.NewTask("error-trigger", "provoking total undo")
	terr.WaitFor(last)
	terr.JoinLane(last.Lanes()[0])
	chg.AddTask(terr)

	s.settle(c)

	mountTask := tasks[len(tasks)-12]
	c.Assert(mountTask.Kind(), Equals, "mount-snap")
	var installRecord backend.InstallRecord
	c.Assert(mountTask.Get("install-record", &installRecord), testutil.ErrorIs, state.ErrNoState)
}

func (s *snapmgrTestSuite) TestInstallDefaultProviderCircular(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, contentStore{fakeStore: s.fakeStore, state: s.state})

	repo := interfaces.NewRepository()
	ifacerepo.Replace(s.state, repo)

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel", Revision: snap.R(42)}
	ts, err := snapstate.Install(context.Background(), s.state, "snap-content-circular1", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	// ensure all our tasks ran
	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)
	// and both circular snaps got linked
	c.Check(s.fakeBackend.ops, testutil.DeepContains, fakeOp{
		op:   "link-snap",
		path: filepath.Join(dirs.SnapMountDir, "snap-content-circular1/42"),
	})
	c.Check(s.fakeBackend.ops, testutil.DeepContains, fakeOp{
		op:   "link-snap",
		path: filepath.Join(dirs.SnapMountDir, "snap-content-circular2/11"),
	})
}

func (s *snapmgrTestSuite) TestParallelInstallInstallPathExperimentalSwitch(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	mockSnap := makeTestSnap(c, `name: some-snap
version: 1.0
`)
	si := &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(8)}
	_, _, err := snapstate.InstallPath(s.state, si, mockSnap, "some-snap_foo", "", snapstate.Flags{}, nil)
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.parallel-instances' to true")

	// enable parallel instances
	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.parallel-instances", true)
	tr.Commit()

	_, _, err = snapstate.InstallPath(s.state, si, mockSnap, "some-snap_foo", "", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallMany(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	installed, tts, err := snapstate.InstallMany(s.state, []string{"one", "two"}, nil, 0, nil)
	c.Assert(err, IsNil)
	c.Assert(tts, HasLen, 2)
	c.Check(installed, DeepEquals, []string{"one", "two"})

	c.Check(s.fakeStore.seenPrivacyKeys["privacy-key"], Equals, true)

	for i, ts := range tts {
		verifyInstallTasks(c, snap.TypeApp, 0, 0, ts)
		// check that tasksets are in separate lanes
		for _, t := range ts.Tasks() {
			c.Assert(t.Lanes(), DeepEquals, []int{i + 1})
			if t.Kind() == "prerequisites" {
				sup, err := snapstate.TaskSnapSetup(t)
				c.Assert(err, IsNil)
				c.Check(sup.Version, Equals, sup.SnapName()+"Ver")
			}
		}
	}
}

func (s *snapmgrTestSuite) TestInstallManyDevMode(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapNames := []string{"one", "two"}
	installed, tts, err := snapstate.InstallMany(s.state, snapNames, nil, 0, &snapstate.Flags{DevMode: true})
	c.Assert(err, IsNil)
	c.Assert(tts, HasLen, 2)
	c.Check(installed, DeepEquals, snapNames)

	c.Check(s.fakeStore.seenPrivacyKeys["privacy-key"], Equals, true)

	for i, ts := range tts {
		verifyInstallTasks(c, snap.TypeApp, 0, 0, ts)
		// check that tasksets are in separate lanes
		for _, t := range ts.Tasks() {
			c.Assert(t.Lanes(), DeepEquals, []int{i + 1})
		}
	}
	for i := range snapNames {
		snapsup, err := snapstate.TaskSnapSetup(tts[i].Tasks()[0])
		c.Assert(err, IsNil)
		c.Check(snapsup.DevMode, Equals, true)
	}
}

func (s *snapmgrTestSuite) TestInstallManyTransactionally(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	installed, tts, err := snapstate.InstallMany(s.state, []string{"one", "two"}, nil, 0,
		&snapstate.Flags{Transaction: client.TransactionAllSnaps})
	c.Assert(err, IsNil)
	c.Assert(tts, HasLen, 2)
	c.Check(installed, DeepEquals, []string{"one", "two"})

	for _, ts := range tts {
		verifyInstallTasks(c, snap.TypeApp, 0, 0, ts)
		// check that tasksets are all in one lane
		for _, t := range ts.Tasks() {
			c.Assert(t.Lanes(), DeepEquals, []int{1})
		}
	}
}

func (s *snapmgrTestSuite) TestInstallManyWithPrereqsTransactionally(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	restore := snapstate.MockPrerequisitesRetryTimeout(10 * time.Millisecond)
	defer restore()

	// pretend we don't have core
	snapstate.Set(s.state, "core", nil)

	snapsToInstall := []string{"snap1", "snap2"}
	installed, tts, err := snapstate.InstallMany(s.state, snapsToInstall, nil, 0,
		&snapstate.Flags{Transaction: client.TransactionAllSnaps})
	c.Assert(err, IsNil)
	c.Assert(tts, HasLen, 2)
	c.Check(installed, DeepEquals, snapsToInstall)
	numTasksBeforePrereq := 0

	// Check that all tasks are in the same lane
	for _, ts := range tts {
		verifyInstallTasks(c, snap.TypeApp, 0, 0, ts)
		prereq := ts.Tasks()[0]
		c.Assert(prereq.Kind(), Equals, "prerequisites")
		for _, t := range ts.Tasks() {
			c.Assert(t.Lanes(), DeepEquals, []int{1})
			numTasksBeforePrereq++
		}
	}

	// Create change with tasks and run
	chg := s.state.NewChange("install", "install some snaps")
	for _, ts := range tts {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	// Check that all tasks in the change are in the same lane
	for _, t := range chg.Tasks() {
		c.Assert(t.Lanes(), DeepEquals, []int{1})
	}
	// Check that we have actually added new tasks to install the base
	c.Assert(numTasksBeforePrereq < len(chg.Tasks()), Equals, true)

	// verify core in the system state
	var snaps map[string]*snapstate.SnapState
	err = s.state.Get("snaps", &snaps)
	c.Assert(err, IsNil)

	snapst := snaps["core"]
	c.Assert(snapst, NotNil)
	c.Assert(snapst.Active, Equals, true)
	c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: "core",
		SnapID:   "core-id",
		Revision: snap.R(11),
		Channel:  "stable",
	}, nil))

	// Verify the snaps we wanted to install
	for _, s := range snapsToInstall {
		snapst = snaps[s]
		c.Assert(snapst, NotNil)
		c.Assert(snapst.Active, Equals, true)
		c.Assert(snapst.Sequence.Revisions[0], DeepEquals, sequence.NewRevisionSideState(&snap.SideInfo{
			RealName: s,
			SnapID:   s + "-id",
			Channel:  "stable",
			Revision: snap.R(11),
		}, nil))
	}
}

func (s *snapmgrTestSuite) TestInstallManyTransactionallyFails(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// trigger download error on content provider
	s.fakeStore.downloadError["some-other-snap"] = fmt.Errorf("boom")

	snapstate.ReplaceStore(s.state,
		contentStore{fakeStore: s.fakeStore, state: s.state})

	repo := interfaces.NewRepository()
	ifacerepo.Replace(s.state, repo)

	chg := s.state.NewChange("install", "install some snaps")
	installed, tts, err := snapstate.InstallMany(s.state,
		[]string{"some-snap", "some-other-snap"}, nil, 0,
		&snapstate.Flags{Transaction: client.TransactionAllSnaps})
	c.Assert(err, IsNil)
	c.Check(installed, testutil.DeepUnsortedMatches, []string{"some-snap", "some-other-snap"})
	for _, ts := range tts {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), ErrorMatches, "cannot perform the following tasks:\n.*Download snap \"some-other-snap\" \\(11\\) from channel \"stable\" \\(boom\\).*")
	c.Assert(chg.IsReady(), Equals, true)

	var snapSt snapstate.SnapState
	// some-other-snap not installed due to download failure
	c.Assert(snapstate.Get(s.state, "some-other-snap", &snapSt), testutil.ErrorIs, state.ErrNoState)
	// some-snap not installed as this was a transactional install
	c.Assert(snapstate.Get(s.state, "some-snap", &snapSt), testutil.ErrorIs, state.ErrNoState)
}

func (s *snapmgrTestSuite) TestInstallManyDiskSpaceError(c *C) {
	restore := snapstate.MockOsutilCheckFreeSpace(func(string, uint64) error { return &osutil.NotEnoughDiskSpaceError{} })
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.check-disk-space-install", true)
	tr.Commit()

	_, _, err := snapstate.InstallMany(s.state, []string{"one", "two"}, nil, 0, nil)
	diskSpaceErr := err.(*snapstate.InsufficientSpaceError)
	c.Assert(diskSpaceErr, ErrorMatches, `insufficient space in .* to perform "install" change for the following snaps: one, two`)
	c.Check(diskSpaceErr.Path, Equals, filepath.Join(dirs.GlobalRootDir, "/var/lib/snapd"))
	c.Check(diskSpaceErr.Snaps, DeepEquals, []string{"one", "two"})
	c.Check(diskSpaceErr.ChangeKind, Equals, "install")
}

func (s *snapmgrTestSuite) TestInstallManyDiskCheckDisabled(c *C) {
	restore := snapstate.MockOsutilCheckFreeSpace(func(string, uint64) error { return &osutil.NotEnoughDiskSpaceError{} })
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.check-disk-space-install", false)
	tr.Commit()

	_, _, err := snapstate.InstallMany(s.state, []string{"one", "two"}, nil, 0, nil)
	c.Check(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallManyTooEarly(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	s.state.Set("seeded", nil)

	_, _, err := snapstate.InstallMany(s.state, []string{"one", "two"}, nil, 0, nil)
	c.Check(err, testutil.ErrorIs, &snapstate.ChangeConflictError{})
	c.Assert(err, ErrorMatches, `.*too early for operation, device not yet seeded or device model not acknowledged`)
}

func (s *snapmgrTestSuite) TestInstallManyChecksPreconditions(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	_, _, err := snapstate.InstallMany(s.state, []string{"some-snap-now-classic"}, nil, 0, nil)
	c.Assert(err, NotNil)
	c.Check(err, DeepEquals, &snapstate.SnapNeedsClassicError{Snap: "some-snap-now-classic"})

	_, _, err = snapstate.InstallMany(s.state, []string{"some-snap_foo"}, nil, 0, nil)
	c.Assert(err, ErrorMatches, "experimental feature disabled - test it by setting 'experimental.parallel-instances' to true")
}

func verifyStopReason(c *C, ts *state.TaskSet, reason string) {
	tl := tasksWithKind(ts, "stop-snap-services")
	c.Check(tl, HasLen, 1)

	var stopReason string
	err := tl[0].Get("stop-reason", &stopReason)
	c.Assert(err, IsNil)
	c.Check(stopReason, Equals, reason)

}

func (s *snapmgrTestSuite) TestUndoMountSnapFailsInCopyData(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "some-channel"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.fakeBackend.copySnapDataFailTrigger = filepath.Join(dirs.SnapMountDir, "some-snap/11")

	s.settle(c)

	expected := fakeOps{
		{
			op:     "storesvc-snap-action",
			userID: 1,
		},
		{
			op: "storesvc-snap-action:action",
			action: store.SnapAction{
				Action:       "install",
				InstanceName: "some-snap",
				Channel:      "some-channel",
			},
			revno:  snap.R(11),
			userID: 1,
		},
		{
			op:   "storesvc-download",
			name: "some-snap",
		},
		{
			op:    "validate-snap:Doing",
			name:  "some-snap",
			revno: snap.R(11),
		},
		{
			op:  "current",
			old: "<no-current>",
		},
		{
			op:   "open-snap-file",
			path: filepath.Join(dirs.SnapBlobDir, "some-snap_11.snap"),
			sinfo: snap.SideInfo{
				RealName: "some-snap",
				SnapID:   "some-snap-id",
				Channel:  "some-channel",
				Revision: snap.R(11),
			},
		},
		{
			op:    "setup-snap",
			name:  "some-snap",
			path:  filepath.Join(dirs.SnapBlobDir, "some-snap_11.snap"),
			revno: snap.R(11),
		},
		{
			op:   "copy-data.failed",
			path: filepath.Join(dirs.SnapMountDir, "some-snap/11"),
			old:  "<no-old>",
		},
		{
			op:   "remove-snap-data-dir",
			name: "some-snap",
			path: filepath.Join(dirs.SnapDataDir, "some-snap"),
		},
		{
			op:    "undo-setup-snap",
			name:  "some-snap",
			path:  filepath.Join(dirs.SnapMountDir, "some-snap/11"),
			stype: "app",
		},
		{
			op:   "remove-snap-dir",
			name: "some-snap",
			path: filepath.Join(dirs.SnapMountDir, "some-snap"),
		},
	}
	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Assert(s.fakeBackend.ops, DeepEquals, expected)
}

func (s *snapmgrTestSuite) TestSideInfoPaid(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	opts := &snapstate.RevisionOptions{Channel: "channel-for-paid"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	chg := s.state.NewChange("install", "install paid snap")
	chg.AddAll(ts)

	s.settle(c)

	// verify snap has paid sideinfo
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.CurrentSideInfo().Paid, Equals, true)
	c.Check(snapst.CurrentSideInfo().Private, Equals, false)
}

func (s *snapmgrTestSuite) TestSideInfoPrivate(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	opts := &snapstate.RevisionOptions{Channel: "channel-for-private"}
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	chg := s.state.NewChange("install", "install private snap")
	chg.AddAll(ts)

	s.settle(c)

	// verify snap has private sideinfo
	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)
	c.Check(snapst.CurrentSideInfo().Private, Equals, true)
	c.Check(snapst.CurrentSideInfo().Paid, Equals, false)
}

func (s *snapmgrTestSuite) TestGadgetDefaultsInstalled(c *C) {
	makeInstalledMockCoreSnap(c)

	// using MockSnap, we want to read the bits on disk
	snapstate.MockSnapReadInfo(snap.ReadInfo)

	s.state.Lock()
	defer s.state.Unlock()

	s.prepareGadget(c)

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(1)}}),
		Current:  snap.R(1),
		SnapType: "app",
	})

	snapPath := makeTestSnap(c, "name: some-snap\nversion: 1.0")

	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(2)}, snapPath, "", "edge", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)

	var m map[string]any
	runHooks := tasksWithKind(ts, "run-hook")

	c.Assert(runHooks[0].Kind(), Equals, "run-hook")
	err = runHooks[0].Get("hook-context", &m)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)
}

func makeInstalledMockCoreSnap(c *C) {
	coreSnapYaml := `name: core
version: 1.0
type: os
`
	snaptest.MockSnap(c, coreSnapYaml, &snap.SideInfo{
		RealName: "core",
		Revision: snap.R(1),
	})
}

func (s *snapmgrTestSuite) TestGadgetDefaults(c *C) {
	r := release.MockOnClassic(false)
	defer r()

	makeInstalledMockCoreSnap(c)

	// using MockSnap, we want to read the bits on disk
	snapstate.MockSnapReadInfo(snap.ReadInfo)

	s.state.Lock()
	defer s.state.Unlock()

	s.prepareGadget(c)

	snapPath := makeTestSnap(c, "name: some-snap\nversion: 1.0")

	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "some-snap", SnapID: "some-snap-id", Revision: snap.R(1)}, snapPath, "", "edge", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)

	var m map[string]any
	runHooks := tasksWithKind(ts, "run-hook")

	c.Assert(taskKinds(runHooks), DeepEquals, []string{
		"run-hook[install]",
		"run-hook[default-configure]",
		"run-hook[configure]",
		"run-hook[check-health]",
	})
	// default-configure always uses defaults, not required to explicitly indicate this within the hook context data
	err = runHooks[1].Get("hook-context", &m)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)

	err = runHooks[2].Get("hook-context", &m)
	c.Assert(err, IsNil)
	c.Assert(m, DeepEquals, map[string]any{"use-defaults": true})
}

func (s *snapmgrTestSuite) TestGadgetDefaultsNotForOS(c *C) {
	r := release.MockOnClassic(false)
	defer r()

	// using MockSnap, we want to read the bits on disk
	snapstate.MockSnapReadInfo(snap.ReadInfo)

	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "core", nil)

	s.prepareGadget(c)

	const coreSnapYaml = `
name: core
type: os
version: 1.0
`
	snapPath := makeTestSnap(c, coreSnapYaml)

	ts, _, err := snapstate.InstallPath(s.state, &snap.SideInfo{RealName: "core", SnapID: "core-id", Revision: snap.R(1)}, snapPath, "", "edge", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)

	var m map[string]any
	runHooks := tasksWithKind(ts, "run-hook")

	c.Assert(taskKinds(runHooks), DeepEquals, []string{
		"run-hook[install]",
		"run-hook[configure]",
		"run-hook[check-health]",
	})
	// use-defaults flag is part of hook-context which isn't set
	err = runHooks[1].Get("hook-context", &m)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)
}

func (s *snapmgrTestSuite) TestGadgetDefaultsAreNormalizedForConfigHook(c *C) {
	var mockGadgetSnapYaml = `
name: canonical-pc
type: gadget
`
	var mockGadgetYaml = []byte(`
defaults:
  otheridididididididididididididi:
    foo:
      bar: baz
      num: 1.305

volumes:
    volume-id:
        bootloader: grub
`)

	info := snaptest.MockSnap(c, mockGadgetSnapYaml, &snap.SideInfo{Revision: snap.R(2)})
	err := os.WriteFile(filepath.Join(info.MountDir(), "meta", "gadget.yaml"), mockGadgetYaml, 0644)
	c.Assert(err, IsNil)

	gi, err := gadget.ReadInfo(info.MountDir(), nil)
	c.Assert(err, IsNil)
	c.Assert(gi, NotNil)

	snapName := "some-snap"
	hooksup := &hookstate.HookSetup{
		Snap:        snapName,
		Hook:        "configure",
		Optional:    true,
		IgnoreError: false,
	}

	contextData := map[string]any{"patch": gi.Defaults}

	s.state.Lock()
	defer s.state.Unlock()
	c.Assert(hookstate.HookTask(s.state, "", hooksup, contextData), NotNil)
}

func (s *snapmgrTestSuite) TestInstallContentProviderDownloadFailure(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// trigger download error on content provider
	s.fakeStore.downloadError["snap-content-slot"] = fmt.Errorf("boom")

	snapstate.ReplaceStore(s.state, contentStore{fakeStore: s.fakeStore, state: s.state})

	repo := interfaces.NewRepository()
	ifacerepo.Replace(s.state, repo)

	chg := s.state.NewChange("install", "install a snap")
	opts := &snapstate.RevisionOptions{Channel: "stable", Revision: snap.R(42)}
	ts, err := snapstate.Install(context.Background(), s.state, "snap-content-plug", opts, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), ErrorMatches, "cannot perform the following tasks:\n.*Download snap \"snap-content-slot\" \\(11\\) from channel \"stable\" \\(boom\\).*")
	c.Assert(chg.IsReady(), Equals, true)

	var snapSt snapstate.SnapState
	// content provider not installed due to download failure
	c.Assert(snapstate.Get(s.state, "snap-content-slot", &snapSt), testutil.ErrorIs, state.ErrNoState)

	// but content consumer gets installed
	c.Assert(snapstate.Get(s.state, "snap-content-plug", &snapSt), IsNil)
	c.Check(snapSt.Current, Equals, snap.R(42))
}

type validationSetsSuite struct {
	snapmgrBaseTest
	storeSigning *assertstest.StoreStack
	dev1acct     *asserts.Account
	acct1Key     *asserts.AccountKey
	dev1Signing  *assertstest.SigningDB
}

var _ = Suite(&validationSetsSuite{})

func (s *validationSetsSuite) SetUpTest(c *C) {
	s.snapmgrBaseTest.SetUpTest(c)

	s.storeSigning = assertstest.NewStoreStack("can0nical", nil)
	s.dev1acct = assertstest.NewAccount(s.storeSigning, "developer1", nil, "")
	c.Assert(s.storeSigning.Add(s.dev1acct), IsNil)
	dev1PrivKey, _ := assertstest.GenerateKey(752)
	s.acct1Key = assertstest.NewAccountKey(s.storeSigning, s.dev1acct, nil, dev1PrivKey.PublicKey(), "")
	s.dev1Signing = assertstest.NewSigningDB(s.dev1acct.AccountID(), dev1PrivKey)
	c.Assert(s.storeSigning.Add(s.acct1Key), IsNil)
}

func (s *validationSetsSuite) mockValidationSetAssert(c *C, name, sequence string, snaps ...any) asserts.Assertion {
	headers := map[string]any{
		"authority-id": "foo",
		"account-id":   "foo",
		"name":         name,
		"series":       "16",
		"sequence":     sequence,
		"revision":     "5",
		"timestamp":    "2030-11-06T09:16:26Z",
		"snaps":        snaps,
	}
	vs, err := s.dev1Signing.Sign(asserts.ValidationSetType, headers, nil, "")
	c.Assert(err, IsNil)
	return vs
}

func (s *validationSetsSuite) installSnapReferencedByValidationSet(c *C, presence, requiredRev string, installRev snap.Revision, cohort string, flags *snapstate.Flags) error {
	if flags == nil {
		flags = &snapstate.Flags{}
	}

	restore := snapstate.MockEnforcedValidationSets(func(st *state.State, extraVss ...*asserts.ValidationSet) (*snapasserts.ValidationSets, error) {
		vs := snapasserts.NewValidationSets()
		someSnap := map[string]any{
			"id":       "yOqKhntON3vR7kwEbVPsILm7bUViPDzx",
			"name":     "some-snap",
			"presence": presence,
		}
		if requiredRev != "" {
			someSnap["revision"] = requiredRev
		}
		vsa1 := s.mockValidationSetAssert(c, "bar", "1", someSnap)
		vs.Add(vsa1.(*asserts.ValidationSet))
		return vs, nil
	})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tr := assertstate.ValidationSetTracking{
		AccountID: "foo",
		Name:      "bar",
		Mode:      assertstate.Enforce,
		Current:   1,
	}
	assertstate.UpdateValidationSet(s.state, &tr)

	opts := &snapstate.RevisionOptions{}
	if !installRev.Unset() {
		opts.Revision = installRev
	}
	if cohort != "" {
		opts.CohortKey = cohort
	}
	_, err := snapstate.Install(context.Background(), s.state, "some-snap", opts, 0, *flags)
	return err
}

func (s *validationSetsSuite) TestInstallSnapInvalidForValidationSetRefused(c *C) {
	err := s.installSnapReferencedByValidationSet(c, "invalid", "", snap.R(0), "", nil)
	c.Assert(err, ErrorMatches, `cannot install snap "some-snap" due to enforcing rules of validation set 16/foo/bar/1`)
}

func (s *validationSetsSuite) TestInstallSnapOptionalForValidationSetOK(c *C) {
	err := s.installSnapReferencedByValidationSet(c, "optional", "", snap.R(0), "", nil)
	c.Assert(err, IsNil)
}

func (s *validationSetsSuite) TestInstallSnapRequiredForValidationSet(c *C) {
	err := s.installSnapReferencedByValidationSet(c, "required", "", snap.R(0), "", nil)
	c.Assert(err, IsNil)
	c.Assert(s.fakeBackend.ops, HasLen, 2)
	expectedOp := fakeOp{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			InstanceName:   "some-snap",
			Channel:        "stable",
			ValidationSets: []snapasserts.ValidationSetKey{"16/foo/bar/1"},
		},
		revno: snap.R(11),
	}
	c.Assert(s.fakeBackend.ops[1], DeepEquals, expectedOp)
}

func (s *validationSetsSuite) TestInstallSnapRequiredForValidationSetAtRevision(c *C) {
	err := s.installSnapReferencedByValidationSet(c, "required", "2", snap.R(2), "", nil)
	c.Assert(err, IsNil)
	c.Assert(s.fakeBackend.ops, HasLen, 2)
	expectedOp := fakeOp{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			Revision:       snap.R(2),
			InstanceName:   "some-snap",
			ValidationSets: []snapasserts.ValidationSetKey{"16/foo/bar/1"},
		},
		revno: snap.R(2),
	}
	c.Assert(s.fakeBackend.ops[1], DeepEquals, expectedOp)
}

func (s *validationSetsSuite) TestInstallSnapRequiredForValidationSetCohortIgnored(c *C) {
	err := s.installSnapReferencedByValidationSet(c, "required", "2", snap.R(0), "cohortkey", nil)
	c.Assert(err, IsNil)
	c.Assert(s.fakeBackend.ops, HasLen, 2)
	expectedOp := fakeOp{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			Revision:       snap.R(2),
			InstanceName:   "some-snap",
			ValidationSets: []snapasserts.ValidationSetKey{"16/foo/bar/1"},
		},
		revno: snap.R(2),
	}
	c.Assert(s.fakeBackend.ops[1], DeepEquals, expectedOp)
}

func (s *validationSetsSuite) TestInstallSnapReferencedByValidationSetWrongRevision(c *C) {
	err := s.installSnapReferencedByValidationSet(c, "required", "3", snap.R(2), "", nil)
	c.Assert(err, ErrorMatches, `cannot install snap "some-snap" at revision 2 without --ignore-validation, revision 3 is required by validation sets: 16/foo/bar/1`)
}

func (s *validationSetsSuite) installManySnapReferencedByValidationSet(c *C, snapOnePresence, snapOneRequiredRev, snapTwoPresence, snapTwoRequiredRev string) error {
	restore := snapstate.MockEnforcedValidationSets(func(st *state.State, extraVss ...*asserts.ValidationSet) (*snapasserts.ValidationSets, error) {
		vs := snapasserts.NewValidationSets()
		snapOne := map[string]any{
			"id":       "yOqKhntON3vR7kwEbVPsILm7bUViPDzx",
			"name":     "one",
			"presence": snapOnePresence,
		}
		if snapOneRequiredRev != "" {
			snapOne["revision"] = snapOneRequiredRev
		}
		snapTwo := map[string]any{
			"id":       "xxxahntON3vR7kwEbVPsILm7bUViPDzx",
			"name":     "two",
			"presence": snapTwoPresence,
		}
		if snapTwoRequiredRev != "" {
			snapTwo["revision"] = snapTwoRequiredRev
		}
		vsa1 := s.mockValidationSetAssert(c, "bar", "1", snapOne, snapTwo)
		vs.Add(vsa1.(*asserts.ValidationSet))
		return vs, nil
	})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tr := assertstate.ValidationSetTracking{
		AccountID: "foo",
		Name:      "bar",
		Mode:      assertstate.Enforce,
		Current:   1,
	}
	assertstate.UpdateValidationSet(s.state, &tr)

	_, _, err := snapstate.InstallMany(s.state, []string{"one", "two"}, nil, 0, nil)
	return err
}

func (s *validationSetsSuite) TestInstallManyWithRevisionOpts(c *C) {
	enforced := snapasserts.NewValidationSets()
	err := enforced.Add(s.mockValidationSetAssert(c, "bar", "1", map[string]any{
		"id":       "yOqKhntON3vR7kwEbVPsILm7bUViPDzx",
		"name":     "some-snap",
		"presence": "invalid",
	}).(*asserts.ValidationSet))
	c.Assert(err, IsNil)

	restore := snapstate.MockEnforcedValidationSets(func(st *state.State, extraVss ...*asserts.ValidationSet) (*snapasserts.ValidationSets, error) {
		return enforced, nil
	})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tr := assertstate.ValidationSetTracking{
		AccountID: "foo",
		Name:      "bar",
		Mode:      assertstate.Enforce,
		Current:   1,
	}
	assertstate.UpdateValidationSet(s.state, &tr)

	provided := snapasserts.NewValidationSets()
	err = provided.Add(s.mockValidationSetAssert(c, "bar", "1", map[string]any{
		"id":       "yOqKhntON3vR7kwEbVPsILm7bUViPDzx",
		"name":     "some-snap",
		"presence": "optional",
	}).(*asserts.ValidationSet))
	c.Assert(err, IsNil)

	// installing "some-snap" with revision opts should succeed because provided
	// validation sets should be used, rather than the currently enforced ones
	revOpts := []*snapstate.RevisionOptions{{Revision: snap.R(2), ValidationSets: provided}}
	affected, tss, err := snapstate.InstallMany(s.state, []string{"some-snap"}, revOpts, 0, nil)
	c.Assert(err, IsNil)
	c.Assert(affected, DeepEquals, []string{"some-snap"})

	chg := s.state.NewChange("install", "")
	for _, ts := range tss {
		chg.AddAll(ts)
	}

	s.settle(c)
	c.Check(chg.Err(), IsNil)
	c.Check(chg.Status(), Equals, state.DoneStatus)
}

func (s *validationSetsSuite) TestInstallManyInvalidForValidationSetRefused(c *C) {
	err := s.installManySnapReferencedByValidationSet(c, "invalid", "", "optional", "")
	c.Assert(err, ErrorMatches, `cannot install snap "one" due to enforcing rules of validation set 16/foo/bar/1`)
}

func (s *validationSetsSuite) TestInstallManyRequiredForValidationSetOK(c *C) {
	err := s.installManySnapReferencedByValidationSet(c, "required", "", "optional", "")
	c.Assert(err, IsNil)

	c.Assert(s.fakeBackend.ops, HasLen, 3)
	expectedOps := fakeOps{{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			InstanceName:   "one",
			Channel:        "stable",
			ValidationSets: []snapasserts.ValidationSetKey{"16/foo/bar/1"},
		},
		revno: snap.R(11),
	}, {
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:       "install",
			InstanceName: "two",
			Channel:      "stable",
		},
		revno: snap.R(11),
	}}
	c.Assert(s.fakeBackend.ops[1:], DeepEquals, expectedOps)
}

func (s *validationSetsSuite) TestInstallManyRequiredForValidationSetWithOptionalRevisionOK(c *C) {
	err := s.installManySnapReferencedByValidationSet(c, "required", "", "optional", "12")
	c.Assert(err, IsNil)

	c.Assert(s.fakeBackend.ops, HasLen, 3)
	expectedOps := fakeOps{{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			InstanceName:   "one",
			Channel:        "stable",
			ValidationSets: []snapasserts.ValidationSetKey{"16/foo/bar/1"},
		},
		revno: snap.R(11),
	}, {
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			InstanceName:   "two",
			Revision:       snap.R(12),
			ValidationSets: []snapasserts.ValidationSetKey{"16/foo/bar/1"},
		},
		revno: snap.R(12),
	}}
	c.Assert(s.fakeBackend.ops[1:], DeepEquals, expectedOps)
}

func (s *validationSetsSuite) TestInstallManyRequiredRevisionForValidationSetOK(c *C) {
	err := s.installManySnapReferencedByValidationSet(c, "required", "11", "required", "2")
	c.Assert(err, IsNil)

	c.Assert(s.fakeBackend.ops, HasLen, 3)
	// note, Channel not present when revisions are set
	expectedOps := fakeOps{{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			InstanceName:   "one",
			Revision:       snap.R(11),
			ValidationSets: []snapasserts.ValidationSetKey{"16/foo/bar/1"},
		},
		revno: snap.R(11),
	}, {
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			InstanceName:   "two",
			Revision:       snap.R(2),
			ValidationSets: []snapasserts.ValidationSetKey{"16/foo/bar/1"},
		},
		revno: snap.R(2),
	}}
	c.Assert(s.fakeBackend.ops[1:], DeepEquals, expectedOps)
}

func (s *validationSetsSuite) testInstallSnapRequiredByValidationSetWithBase(c *C, presenceForBase string) error {
	restore := snapstate.MockEnforcedValidationSets(func(st *state.State, extraVss ...*asserts.ValidationSet) (*snapasserts.ValidationSets, error) {
		vs := snapasserts.NewValidationSets()
		someSnap := map[string]any{
			"id":       "yOqKhntON3vR7kwEbVPsILm7bUViPDzx",
			"name":     "some-snap-with-base",
			"presence": "required",
		}
		// base snap is invalid
		someBase := map[string]any{
			"id":       "aOqKhntON3vR7kwEbVPsILm7bUViPDzx",
			"name":     "some-base",
			"presence": presenceForBase,
		}
		vsa1 := s.mockValidationSetAssert(c, "bar", "1", someSnap, someBase)
		vs.Add(vsa1.(*asserts.ValidationSet))
		return vs, nil
	})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	tr := assertstate.ValidationSetTracking{
		AccountID: "foo",
		Name:      "bar",
		Mode:      assertstate.Enforce,
		Current:   1,
	}
	assertstate.UpdateValidationSet(s.state, &tr)

	ts, err := snapstate.Install(context.Background(), s.state, "some-snap-with-base", &snapstate.RevisionOptions{Channel: "channel-for-base/stable"}, 0, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg := s.state.NewChange("install", "...")
	chg.AddAll(ts)

	s.state.Unlock()
	defer s.state.Lock()

	err = s.o.Settle(testutil.HostScaledTimeout(5 * time.Second))
	c.Assert(err, IsNil)

	s.state.Lock()
	defer s.state.Unlock()

	return chg.Err()
}

func (s *validationSetsSuite) TestInstallSnapRequiredByValidationSetWithInvalidBase(c *C) {
	err := s.testInstallSnapRequiredByValidationSetWithBase(c, "invalid")
	c.Check(err, ErrorMatches, `cannot perform the following tasks:
.*Ensure prerequisites for "some-snap-with-base" are available \(cannot install snap base "some-base": cannot install snap "some-base" due to enforcing rules of validation set 16/foo/bar/1\)`)
}

func (s *validationSetsSuite) TestInstallSnapRequiredByValidationSetWithRequiredBase(c *C) {
	err := s.testInstallSnapRequiredByValidationSetWithBase(c, "required")
	c.Check(err, IsNil)
}

func (s *snapmgrTestSuite) TestInstallWithOutdatedPrereq(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, contentStore{fakeStore: s.fakeStore, state: s.state})

	info := &snap.SideInfo{
		Revision: snap.R(1),
		SnapID:   "snap-content-slot-id",
		RealName: "snap-content-slot",
	}
	snapstate.Set(s.state, "snap-content-slot", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{info}),
		Current:  info.Revision,
		Active:   true,
	})

	chg := s.state.NewChange("install", "install a snap")
	ts, err := snapstate.Install(context.Background(), s.state, "snap-content-plug", nil, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.Status(), Equals, state.DoneStatus)

	c.Check(ts.Tasks(), NotNil)
	c.Check(s.fakeStore.downloads, DeepEquals, []fakeDownload{
		{macaroon: s.user.StoreMacaroon, name: "snap-content-plug", target: filepath.Join(dirs.SnapBlobDir, "snap-content-plug_11.snap")},
		{macaroon: s.user.StoreMacaroon, name: "snap-content-slot", target: filepath.Join(dirs.SnapBlobDir, "snap-content-slot_11.snap")},
	})
}

func (s *snapmgrTestSuite) TestHasAllContentAttributes(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	mySnap := &snap.Info{
		SuggestedName: "some-snap",
		Version:       "1",
		Slots:         make(map[string]*snap.SlotInfo, 3),
	}

	// create slots (content type and others) that the snap will provide
	slots := []*snap.SlotInfo{
		{
			Name:      "some-content-slot",
			Snap:      mySnap,
			Interface: "content",
			Attrs:     map[string]any{"content": "some"},
		},
		{
			Name:      "wrong-tag-slot",
			Snap:      mySnap,
			Interface: "content",
			Attrs:     map[string]any{"stuff": "wrong-tag"},
		},
		{
			Name:      "wrong-iface-slot",
			Snap:      mySnap,
			Interface: "diff",
			Attrs:     map[string]any{"content": "wrong-iface"},
		},
	}

	for _, slot := range slots {
		mySnap.Slots[slot.Name] = slot
	}

	// add slots to repo
	repo := interfaces.NewRepository()
	c.Assert(repo.AddInterface(&ifacetest.TestInterface{InterfaceName: "content"}), IsNil)
	c.Assert(repo.AddInterface(&ifacetest.TestInterface{InterfaceName: "diff"}), IsNil)
	ifacerepo.Replace(s.state, repo)

	mySnapAppSet, err := interfaces.NewSnapAppSet(mySnap, nil)
	c.Assert(err, IsNil)

	c.Assert(repo.AddAppSet(mySnapAppSet), IsNil)

	// check snap provides all content tags required
	ok, err := snapstate.HasAllContentAttrs(s.state, "some-snap", []string{"some"})
	c.Check(err, IsNil)
	c.Assert(ok, Equals, true)

	// shouldn't find "wrong-iface" because interface type isn't 'content'
	ok, err = snapstate.HasAllContentAttrs(s.state, "some-snap", []string{"some", "wrong-iface"})
	c.Check(err, IsNil)
	c.Assert(ok, Equals, false)

	// shouldn't find "wrong-tag" because it's not keyed by "content" attr
	ok, err = snapstate.HasAllContentAttrs(s.state, "some-snap", []string{"some", "wrong-tag"})
	c.Check(err, IsNil)
	c.Assert(ok, Equals, false)

	// check that non-existent snap returns false
	ok, err = snapstate.HasAllContentAttrs(s.state, "other-snap", []string{"some"})
	c.Check(err, IsNil)
	c.Assert(ok, Equals, false)

	// check that content attr of non-string type returns error
	err = repo.AddSlot(&snap.SlotInfo{
		Name:      "bad-content-slot",
		Snap:      mySnap,
		Interface: "content",
		Attrs:     map[string]any{"content": 123},
	})
	c.Assert(err, IsNil)

	_, err = snapstate.HasAllContentAttrs(s.state, "some-snap", []string{"some"})
	c.Assert(err.Error(), Equals, `expected 'content' attribute of slot 'bad-content-slot' (snap: 'some-snap') to be string but was int`)
}

func (s *snapmgrTestSuite) TestInstallPrereqIgnoreConflictInSameChange(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, contentStore{fakeStore: s.fakeStore, state: s.state})

	repo := interfaces.NewRepository()
	ifacerepo.Replace(s.state, repo)

	prodInfo := &snap.SideInfo{
		RealName: "snap-content-slot",
		SnapID:   "snap-content-slot-id",
		Revision: snap.R(1),
	}

	chg := s.state.NewChange("install", "")

	// To make the test deterministic, we inject a conflicting task to simulate
	// an InstallMany({snap-content-plug, snap-content-slot}) with a failing snap-content-slot
	conflTask := s.state.NewTask("conflicting-task", "test: conflicting task")
	conflTask.Set("snap-setup", &snapstate.SnapSetup{SideInfo: prodInfo})
	chg.AddTask(conflTask)

	installTasks, err := snapstate.Install(context.Background(), s.state, "snap-content-plug", nil, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	c.Check(installTasks.Tasks(), Not(HasLen), 0)
	chg.AddAll(installTasks)

	s.settle(c)

	// check that the prereq task wasn't retried
	prereqTask := findStrictlyOnePrereqTask(c, chg)
	c.Check(prereqTask.Status(), Equals, state.DoneStatus)
	c.Assert(prereqTask.AtTime().IsZero(), Equals, true)
}

func (s *validationSetsSuite) TestInstallSnapReferencedByValidationSetWrongRevisionIgnoreValidationOK(c *C) {
	// validity check: fails with validation
	err := s.installSnapReferencedByValidationSet(c, "required", "3", snap.R(11), "", &snapstate.Flags{IgnoreValidation: false})
	c.Assert(err, ErrorMatches, `cannot install snap "some-snap" at revision 11 without --ignore-validation, revision 3 is required by validation sets: 16/foo/bar/1`)

	// but doesn't fail with ignore-validation flag
	err = s.installSnapReferencedByValidationSet(c, "required", "3", snap.R(11), "", &snapstate.Flags{IgnoreValidation: true})
	c.Assert(err, IsNil)

	// validation sets are not set on the action
	expectedOp := fakeOp{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:       "install",
			Revision:     snap.R(11),
			InstanceName: "some-snap",
			Flags:        store.SnapActionIgnoreValidation,
		},
		revno: snap.R(11),
	}
	c.Assert(s.fakeBackend.ops[1], DeepEquals, expectedOp)
}

func (s *validationSetsSuite) TestInstallSnapInvalidByValidationSetIgnoreValidationOK(c *C) {
	// doesn't fail with ignore-validation flag
	err := s.installSnapReferencedByValidationSet(c, "invalid", "", snap.R(0), "", &snapstate.Flags{IgnoreValidation: true})
	c.Assert(err, IsNil)

	// validation sets are not set on the action
	expectedOp := fakeOp{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:       "install",
			Channel:      "stable",
			InstanceName: "some-snap",
			Flags:        store.SnapActionIgnoreValidation,
		},
		revno: snap.R(11),
	}
	c.Assert(s.fakeBackend.ops[1], DeepEquals, expectedOp)
}

func (s *validationSetsSuite) TestInstallSnapWithValidationSets(c *C) {
	restore := snapstate.MockEnforcedValidationSets(func(st *state.State, extraVss ...*asserts.ValidationSet) (*snapasserts.ValidationSets, error) {
		return nil, fmt.Errorf("unexpected")
	})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	vsets := snapasserts.NewValidationSets()
	bar := s.mockValidationSetAssert(c, "bar", "1", map[string]any{
		"id":       "yOqKhntON3vR7kwEbVPsILm7bUViPDzx",
		"name":     "some-snap",
		"presence": "optional",
		"revision": "11",
	})
	err := vsets.Add(bar.(*asserts.ValidationSet))
	c.Assert(err, IsNil)
	baz := s.mockValidationSetAssert(c, "baz", "1", map[string]any{
		"id":       "yOqKhntON3vR7kwEbVPsILm7bUViPDzx",
		"name":     "some-snap",
		"presence": "required",
	})
	err = vsets.Add(baz.(*asserts.ValidationSet))
	c.Assert(err, IsNil)

	opts := &snapstate.RevisionOptions{ValidationSets: vsets}
	_, err = snapstate.Install(context.Background(), s.state, "some-snap", opts, 0, snapstate.Flags{})
	c.Assert(err, IsNil)

	// validation sets are set on the action
	expectedOp := fakeOp{
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:         "install",
			InstanceName:   "some-snap",
			ValidationSets: vsets.Keys(),
			Revision:       snap.R(11),
		},
		revno: snap.R(11),
	}
	c.Assert(s.fakeBackend.ops[1], DeepEquals, expectedOp)
}

func (s *snapmgrTestSuite) TestInstallPrerequisiteWithSameDeviceContext(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// unset the global store, it will need to come via the device context
	snapstate.ReplaceStore(s.state, nil)

	deviceCtx := &snapstatetest.TrivialDeviceContext{
		CtxStore: contentStore{
			fakeStore: s.fakeStore,
			state:     s.state,
		},
		DeviceModel: &asserts.Model{},
	}
	snapstatetest.MockDeviceContext(deviceCtx)

	ts, err := snapstate.InstallWithDeviceContext(context.Background(), s.state, "snap-content-plug", nil, s.user.ID, snapstate.Flags{}, nil, deviceCtx, "")
	c.Assert(err, IsNil)
	c.Assert(ts.Tasks(), Not(HasLen), 0)

	chg := s.state.NewChange("install", "test: install")
	chg.AddAll(ts)

	s.settle(c)

	c.Check(s.fakeStore.downloads, DeepEquals, []fakeDownload{
		{macaroon: s.user.StoreMacaroon, name: "snap-content-plug", target: filepath.Join(dirs.SnapBlobDir, "snap-content-plug_11.snap")},
		{macaroon: s.user.StoreMacaroon, name: "snap-content-slot", target: filepath.Join(dirs.SnapBlobDir, "snap-content-slot_11.snap")},
	})
}

func (s *snapmgrTestSuite) TestInstallQuotaGroup(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var quotaWasCalled bool
	s.o.TaskRunner().AddHandler("quota-add-snap", func(t *state.Task, _ *tomb.Tomb) error {
		quotaWasCalled = true
		t.State().Lock()
		ss, err := snapstate.TaskSnapSetup(t)
		t.State().Unlock()
		c.Assert(err, IsNil)
		c.Assert(ss.QuotaGroupName, Equals, "foo")
		return nil
	}, nil)

	flags := snapstate.Flags{
		QuotaGroupName: "foo",
	}

	chg := s.state.NewChange("install", "")
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, s.user.ID, flags)
	c.Assert(err, IsNil)
	c.Check(ts.Tasks(), Not(HasLen), 0)
	chg.AddAll(ts)

	s.settle(c)

	c.Check(chg.Err(), IsNil)
	c.Check(chg.Status(), Equals, state.DoneStatus)
	c.Check(quotaWasCalled, Equals, true)
}

func (s *snapmgrTestSuite) TestInstallUndoQuotaGroup(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var quotaWasCalled bool
	var quotaUndoWasCalled bool
	s.o.TaskRunner().AddHandler("quota-add-snap", func(t *state.Task, _ *tomb.Tomb) error {
		quotaWasCalled = true
		t.State().Lock()
		ss, err := snapstate.TaskSnapSetup(t)
		t.State().Unlock()
		c.Assert(err, IsNil)
		c.Assert(ss.QuotaGroupName, Equals, "foo")
		return nil
	}, func(t *state.Task, _ *tomb.Tomb) error {
		quotaUndoWasCalled = true
		return nil
	})

	flags := snapstate.Flags{
		QuotaGroupName: "foo",
	}

	chg := s.state.NewChange("install", "")
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, s.user.ID, flags)
	c.Assert(err, IsNil)
	c.Check(ts.Tasks(), Not(HasLen), 0)
	chg.AddAll(ts)

	// fail the change after the quota-on-install task (after state is saved)
	s.o.TaskRunner().AddHandler("fail", func(*state.Task, *tomb.Tomb) error {
		return errors.New("expected")
	}, nil)

	failingTask := s.state.NewTask("fail", "expected failure")
	chg.AddTask(failingTask)
	linkTask := findLastTask(chg, "quota-add-snap")
	failingTask.WaitFor(linkTask)
	for _, lane := range linkTask.Lanes() {
		failingTask.JoinLane(lane)
	}

	s.settle(c)

	c.Check(chg.Status(), Equals, state.ErrorStatus)
	c.Check(quotaWasCalled, Equals, true)
	c.Check(quotaUndoWasCalled, Equals, true)
}

func (s *snapmgrTestSuite) TestInstallMigrateData(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	c.Assert(tr.Set("core", "experimental.hidden-snap-folder", true), IsNil)
	tr.Commit()

	chg := s.state.NewChange("install", "")
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	c.Check(ts.Tasks(), Not(HasLen), 0)
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.Status(), Equals, state.DoneStatus)

	c.Assert(s.fakeBackend.ops.First("hide-snap-data"), Not(IsNil))

	expected := &dirs.SnapDirOptions{HiddenSnapDataDir: true}
	assertMigrationState(c, s.state, "some-snap", expected)
}

func (s *snapmgrTestSuite) TestUndoMigrationIfInstallFails(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	c.Assert(tr.Set("core", "experimental.hidden-snap-folder", true), IsNil)
	tr.Commit()

	// fail at the end
	s.fakeBackend.linkSnapFailTrigger = filepath.Join(dirs.SnapMountDir, "/some-snap/11")

	chg := s.state.NewChange("install", "")
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(s.fakeBackend.ops.First("hide-snap-data"), Not(IsNil))
	s.fakeBackend.ops.MustFindOp(c, "undo-hide-snap-data")

	// we fail between writing the sequence file and the state
	assertMigrationInSeqFile(c, "some-snap", nil)

	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "some-snap", &snapst), testutil.ErrorIs, state.ErrNoState)
}

func (s *snapmgrTestSuite) TestUndoMigrationIfInstallFailsAfterSettingState(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	c.Assert(tr.Set("core", "experimental.hidden-snap-folder", true), IsNil)
	tr.Commit()

	chg := s.state.NewChange("install", "")
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", nil, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	// fail the change after the link-snap task (after state is saved)
	s.o.TaskRunner().AddHandler("fail", func(*state.Task, *tomb.Tomb) error {
		return errors.New("expected")
	}, nil)

	failingTask := s.state.NewTask("fail", "expected failure")
	chg.AddTask(failingTask)
	linkTask := findLastTask(chg, "link-snap")
	failingTask.WaitFor(linkTask)
	for _, lane := range linkTask.Lanes() {
		failingTask.JoinLane(lane)
	}

	s.settle(c)

	c.Assert(s.fakeBackend.ops.First("hide-snap-data"), Not(IsNil))
	s.fakeBackend.ops.MustFindOp(c, "undo-hide-snap-data")

	// fail after writing seq file but before writing state
	assertMigrationInSeqFile(c, "some-snap", nil)

	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "some-snap", &snapst), testutil.ErrorIs, state.ErrNoState)
}

func (s *snapmgrTestSuite) TestInstallDeduplicatesSnapNames(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	installed, ts, err := snapstate.InstallMany(s.state, []string{"some-snap", "some-base", "some-snap", "some-base"}, nil, s.user.ID, nil)
	c.Assert(err, IsNil)
	c.Check(installed, testutil.DeepUnsortedMatches, []string{"some-snap", "some-base"})
	c.Check(ts, HasLen, 2)
}

type installFn func(info *snap.SideInfo) (*state.TaskSet, error)

func (s *snapmgrTestSuite) TestCorrectNumRevisionsIfNoneAdded(c *C) {
	// different paths to install a revision already stored in the state
	installFuncs := []installFn{
		func(si *snap.SideInfo) (*state.TaskSet, error) {
			yaml := "name: some-snap\nversion: 1.0\nepoch: 1*"
			path := snaptest.MakeTestSnapWithFiles(c, yaml, nil)
			ts, _, err := snapstate.InstallPath(s.state, si, path, "some-snap", "", snapstate.Flags{}, nil)
			return ts, err
		}, func(si *snap.SideInfo) (*state.TaskSet, error) {
			return snapstate.Update(s.state, "some-snap", &snapstate.RevisionOptions{Revision: si.Revision}, s.user.ID, snapstate.Flags{})
		},
	}

	for _, fn := range installFuncs {
		s.testRetainCorrectNumRevisions(c, fn)
	}
}

func (s *snapmgrTestSuite) testRetainCorrectNumRevisions(c *C, installFn installFn) {
	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{
		RealName: "some-snap",
		SnapID:   "some-snap-id",
		Revision: snap.R(1),
		Channel:  "latest/stable",
	}
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:          true,
		TrackingChannel: "latest/stable",
		Sequence:        snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
		Current:         si.Revision,
		SnapType:        "app",
	})

	tr := config.NewTransaction(s.state)
	c.Assert(tr.Set("core", "refresh.retain", 1), IsNil)
	tr.Commit()

	// install already stored revision
	ts, err := installFn(si)
	c.Assert(err, IsNil)
	c.Assert(ts, NotNil)
	chg := s.state.NewChange("install", "")
	chg.AddAll(ts)

	s.settle(c)
	c.Assert(chg.Err(), IsNil)

	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, IsNil)
	c.Assert(snapst.Sequence, DeepEquals, snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}))
}

func (s *snapmgrTestSuite) TestInstallPathMany(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("3"),
		}
		sideInfos = append(sideInfos, si)
	}

	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, 0, nil)
	c.Assert(err, IsNil)
	c.Assert(tss, HasLen, 2)

	for i, ts := range tss {
		// check that tasksets are in separate lanes
		for _, t := range ts.Tasks() {
			c.Assert(t.Lanes(), DeepEquals, []int{i + 1})
			if t.Kind() == "prerequisites" {
				sup, err := snapstate.TaskSnapSetup(t)
				c.Assert(err, IsNil)
				c.Check(sup.SnapName(), Equals, snapNames[i])
				c.Check(sup.Version, Equals, "1.0")
			}
		}
	}

	chg := s.state.NewChange("install", "install local snaps")
	for _, ts := range tss {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	for _, name := range snapNames {
		var snapst snapstate.SnapState
		err = snapstate.Get(s.state, name, &snapst)
		c.Assert(err, IsNil)
		c.Check(snapst.Current, Equals, snap.R("3"))
	}
}

func (s *snapmgrTestSuite) TestInstallPathManyWithOneFailing(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		sideInfos = append(sideInfos, &snap.SideInfo{RealName: name})
	}

	s.o.TaskRunner().AddHandler("fail", func(*state.Task, *tomb.Tomb) error {
		return errors.New("expected")
	}, nil)

	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, 0, nil)
	c.Assert(err, IsNil)
	c.Assert(tss, HasLen, 2)

	// fail installation of 'other-snap' which shouldn't affect 'some-snap'
	failingTask := s.state.NewTask("fail", "expected failure")
	snapThreeLanes := tss[1].Tasks()[0].Lanes()
	for _, lane := range snapThreeLanes {
		failingTask.JoinLane(lane)
	}
	tss[1].AddTask(failingTask)

	chg := s.state.NewChange("install", "install local snaps")
	for _, ts := range tss {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), NotNil)
	c.Assert(chg.IsReady(), Equals, true)

	// some-snap is installed
	err = snapstate.Get(s.state, "some-snap", &snapstate.SnapState{})
	c.Assert(err, IsNil)

	// other-snap is not
	err = snapstate.Get(s.state, "other-snap", &snapstate.SnapState{})
	c.Assert(errors.Is(err, state.ErrNoState), Equals, true)
}

func (s *snapmgrTestSuite) TestInstallPathManyTransactionally(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("3"),
		}
		sideInfos = append(sideInfos, si)
	}

	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos,
		paths, 0, &snapstate.Flags{Transaction: client.TransactionAllSnaps})
	c.Assert(err, IsNil)
	c.Assert(tss, HasLen, 2)

	for _, ts := range tss {
		// check that tasksets are all in one lane
		for _, t := range ts.Tasks() {
			c.Assert(t.Lanes(), DeepEquals, []int{1})
		}
	}

	chg := s.state.NewChange("install", "install local snaps")
	for _, ts := range tss {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	for _, name := range snapNames {
		var snapst snapstate.SnapState
		err = snapstate.Get(s.state, name, &snapst)
		c.Assert(err, IsNil)
		c.Check(snapst.Current, Equals, snap.R("3"))
	}
}

func (s *snapmgrTestSuite) TestInstallPathManyTransactionallyWithOneFailing(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		sideInfos = append(sideInfos, &snap.SideInfo{RealName: name})
	}

	// make other-snap installation fail
	s.fakeBackend.linkSnapFailTrigger = filepath.Join(dirs.SnapMountDir, "/other-snap/x1")

	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos,
		paths, 0, &snapstate.Flags{Transaction: client.TransactionAllSnaps})
	c.Assert(err, IsNil)
	c.Assert(tss, HasLen, 2)

	chg := s.state.NewChange("install", "install local snaps")
	for _, ts := range tss {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), NotNil)
	c.Assert(chg.IsReady(), Equals, true)

	// other-snap is not installed
	err = snapstate.Get(s.state, "other-snap", &snapstate.SnapState{})
	c.Assert(errors.Is(err, state.ErrNoState), Equals, true)
	// and some-snap neither
	err = snapstate.Get(s.state, "some-snap", &snapstate.SnapState{})
	c.Assert(errors.Is(err, state.ErrNoState), Equals, true)
}

func (s *snapmgrTestSuite) TestInstallPathManyAsUpdate(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("1"),
		}
		snapstate.Set(s.state, name, &snapstate.SnapState{
			Active:   true,
			Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
			Current:  si.Revision,
			SnapType: "app",
		})

		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
`, name)

		newSi := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("2"),
		}
		path, _ := snaptest.MakeTestSnapInfoWithFiles(c, yaml, nil, newSi)

		paths = append(paths, path)
		sideInfos = append(sideInfos, newSi)
	}

	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, 0, nil)
	c.Assert(err, IsNil)
	c.Assert(tss, HasLen, 2)

	chg := s.state.NewChange("install", "install local snaps")
	for _, ts := range tss {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	for _, name := range snapNames {
		var snapst snapstate.SnapState
		err = snapstate.Get(s.state, name, &snapst)
		c.Assert(err, IsNil)
		c.Check(snapst.Current, Equals, snap.R("2"))
	}
}

func (s *snapmgrTestSuite) TestInstallPathManyDiskSpaceError(c *C) {
	restore := snapstate.MockOsutilCheckFreeSpace(func(string, uint64) error { return &osutil.NotEnoughDiskSpaceError{} })
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("1"),
		}
		sideInfos = append(sideInfos, si)
	}
	tr := config.NewTransaction(s.state)
	c.Assert(tr.Set("core", "experimental.check-disk-space-install", true), IsNil)
	tr.Commit()

	_, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, 0, nil)
	diskSpaceErr, ok := err.(*snapstate.InsufficientSpaceError)
	c.Assert(ok, Equals, true)
	c.Check(diskSpaceErr, ErrorMatches, `insufficient space in .* to perform "install" change for the following snaps: some-snap, other-snap`)
	c.Check(diskSpaceErr.Path, Equals, filepath.Join(dirs.GlobalRootDir, "/var/lib/snapd"))
	c.Check(diskSpaceErr.Snaps, DeepEquals, snapNames)
}

func (s *snapmgrTestSuite) TestInstallPathManyClassic(c *C) {
	restore := maybeMockClassicSupport(c)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
confinement: classic
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("1"),
		}
		sideInfos = append(sideInfos, si)
	}

	tts, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, s.user.ID, &snapstate.Flags{Classic: true})
	c.Assert(err, IsNil)
	c.Assert(tts, HasLen, 2)

	for i := range paths {
		snapsup, err := snapstate.TaskSnapSetup(tts[i].Tasks()[0])
		c.Assert(err, IsNil)
		c.Check(snapsup.Classic, Equals, true)
	}
}

func (s *snapmgrTestSuite) TestInstallPathManyDevMode(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
confinement: devmode
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("1"),
		}
		sideInfos = append(sideInfos, si)
	}

	tts, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, s.user.ID, &snapstate.Flags{DevMode: true})
	c.Assert(err, IsNil)
	c.Assert(tts, HasLen, 2)

	for i := range paths {
		snapsup, err := snapstate.TaskSnapSetup(tts[i].Tasks()[0])
		c.Assert(err, IsNil)
		c.Check(snapsup.DevMode, Equals, true)
	}
}

func (s *snapmgrTestSuite) TestInstallPathManyMissingClassic(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
confinement: classic
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("1"),
		}
		sideInfos = append(sideInfos, si)
	}

	_, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, s.user.ID, nil)
	c.Assert(err, ErrorMatches, `snap "some-snap" requires classic confinement`)
}

func (s *snapmgrTestSuite) TestInstallPathManyFailOnEpochMismatch(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{{RealName: "some-snap", Revision: snap.R(-1)}}),
		Current:  snap.R(-1),
	})
	yaml := `name: some-snap
version: 1.0
epoch: 42
`
	path := makeTestSnap(c, yaml)
	si := &snap.SideInfo{
		RealName: "some-snap",
		Revision: snap.R(-2),
	}

	_, err := snapstate.InstallPathMany(context.Background(), s.state, []*snap.SideInfo{si}, []string{path}, s.user.ID, nil)
	c.Assert(err, ErrorMatches, `cannot refresh "some-snap" to local snap with epoch 42, because it can't read the current epoch of 1\*`)
}

func (s *snapmgrTestSuite) TestInstallPathManyClassicAsUpdate(c *C) {
	restore := release.MockReleaseInfo(&release.OS{ID: "ubuntu"})
	defer restore()
	// this needs doing because dirs depends on the release info
	dirs.SetRootDir(dirs.GlobalRootDir)

	restore = snapstate.MockSnapReadInfo(func(name string, si *snap.SideInfo) (*snap.Info, error) {
		return &snap.Info{SuggestedName: name, Confinement: "classic"}, nil
	})
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("1"),
		}
		snapstate.Set(s.state, name, &snapstate.SnapState{
			Active:   true,
			Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si}),
			Current:  si.Revision,
			Flags:    snapstate.Flags{Classic: true},
		})
		yaml := fmt.Sprintf(`name: %s
version: 1.0
confinement: classic
`, name)
		paths = append(paths, makeTestSnap(c, yaml))

		si = &snap.SideInfo{
			RealName: name,
			Revision: snap.R("2"),
		}
		sideInfos = append(sideInfos, si)
	}

	checkClassicInstall := func(tss []*state.TaskSet, err error, expectClassic bool) {
		c.Assert(err, IsNil)
		c.Check(tss, HasLen, 2)

		for i := range paths {
			snapsup, err := snapstate.TaskSnapSetup(tss[i].Tasks()[0])
			c.Assert(err, IsNil)
			c.Check(snapsup.Classic, Equals, expectClassic)
		}

		if c.Failed() {
			c.FailNow()
		}
	}

	// works with --classic
	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, s.user.ID, &snapstate.Flags{Classic: true})
	checkClassicInstall(tss, err, true)

	// works without --classic
	tss, err = snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, s.user.ID, nil)
	checkClassicInstall(tss, err, true)

	// devmode overrides the snapsetup classic flag
	tss, err = snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, s.user.ID, &snapstate.Flags{DevMode: true})
	checkClassicInstall(tss, err, false)

	// jailmode overrides it too (you need to provide both)
	tss, err = snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, s.user.ID, &snapstate.Flags{JailMode: true})
	checkClassicInstall(tss, err, false)

	// jailmode and classic together gets you both
	tss, err = snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, s.user.ID, &snapstate.Flags{JailMode: true, Classic: true})
	checkClassicInstall(tss, err, true)
}

func (s *snapmgrTestSuite) TestInstallPathManyValidateContainer(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	path, si := mkInvalidSnap(c)
	_, err := snapstate.InstallPathMany(context.Background(), s.state, []*snap.SideInfo{si}, []string{path}, s.user.ID, nil)
	c.Assert(err, ErrorMatches, fmt.Sprintf(".*%s.*", snap.ErrBadModes))
}

func mkInvalidSnap(c *C) (string, *snap.SideInfo) {
	si := &snap.SideInfo{
		RealName: "some-snap",
		Revision: snap.R("1"),
	}
	yaml := []byte(`name: some-snap
version: 1
`)

	dstDir := c.MkDir()
	c.Assert(os.Chmod(dstDir, 0700), IsNil)

	c.Assert(os.Mkdir(filepath.Join(dstDir, "meta"), 0700), IsNil)
	c.Assert(os.WriteFile(filepath.Join(dstDir, "meta", "snap.yaml"), yaml, 0700), IsNil)

	// snapdir has /meta/snap.yaml, but / is 0700
	brokenSnap := filepath.Join(c.MkDir(), "broken.snap")
	out, err := exec.Command("mksquashfs", dstDir, brokenSnap).CombinedOutput()
	if err != nil {
		c.Log(out)
		c.Error(err)
		c.FailNow()
	}

	return brokenSnap, si
}

func (s *snapmgrTestSuite) TestInstallPathManyWithLocalPrereqAndBaseNoStore(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	c.Assert(tr.Set("core", "experimental.check-disk-space-install", true), IsNil)
	tr.Commit()

	// use the real disk check since it also includes store checks
	restore := snapstate.MockInstallSize(snapstate.InstallSize)
	defer restore()

	// no core, we'll install it as well
	snapstate.Set(s.state, "core", nil)

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "prereq-snap", "core"}
	yamls := []string{
		`name: some-snap
version: 1.0
base: core
plugs:
  myplug:
    interface: content
    content: mycontent
    default-provider: prereq-snap
`,
		`name: prereq-snap
version: 1.0
base: core
slots:
  myslot:
    interface: content
    content: mycontent`,
		`name: core
version: 1.0
type: base
`,
	}

	for i, name := range snapNames {
		paths = append(paths, makeTestSnap(c, yamls[i]))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("1"),
		}
		sideInfos = append(sideInfos, si)
	}

	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, 0, nil)
	c.Assert(err, IsNil)
	c.Assert(tss, HasLen, 3)

	chg := s.state.NewChange("install", "install local snaps")
	for _, ts := range tss {
		chg.AddAll(ts)
	}

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.IsReady(), Equals, true)

	op := s.fakeBackend.ops.First("storesvc-snap-action")
	c.Assert(op, IsNil)
}

func (s *snapmgrTestSuite) TestMigrateOnInstallWithCore24(c *C) {
	c.Skip("TODO:Snap-folder: no automatic migration for core22 snaps to ~/Snap folder for now")

	s.state.Lock()
	defer s.state.Unlock()

	ts, err := snapstate.Install(context.Background(), s.state, "snap-for-core24", nil, 0, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg := s.state.NewChange("install", "install a snap")
	chg.AddAll(ts)

	s.settle(c)

	c.Assert(chg.Err(), IsNil)
	c.Assert(chg.Status(), Equals, state.DoneStatus)

	containsInOrder(c, s.fakeBackend.ops, []string{"hide-snap-data", "init-exposed-snap-home"})

	expected := &dirs.SnapDirOptions{HiddenSnapDataDir: true, MigratedToExposedHome: true}
	assertMigrationState(c, s.state, "snap-for-core24", expected)
}

func (s *snapmgrTestSuite) TestUndoMigrateOnInstallWithCore22AfterLinkSnap(c *C) {
	// we wrote the sequence file but then zeroed it out on undo
	expectSeqFile := true
	s.testUndoMigrateOnInstallWithCore22(c, expectSeqFile, failAfterLinkSnap)
}

func (s *snapmgrTestSuite) TestUndoMigrateOnInstallWithCore22OnExposedMigration(c *C) {
	// we never wrote the sequence file
	expectSeqFile := false
	s.testUndoMigrateOnInstallWithCore22(c, expectSeqFile, func(*overlord.Overlord, *state.Change) error {
		err := errors.New("boom")
		s.fakeBackend.maybeInjectErr = func(op *fakeOp) error {
			if op.op == "init-exposed-snap-home" {
				return err
			}
			return nil
		}

		return err
	})

}

func (s *snapmgrTestSuite) testUndoMigrateOnInstallWithCore22(c *C, expectSeqFile bool, prepFail prepFailFunc) {
	c.Skip("TODO:Snap-folder: no automatic migration for core22 snaps to ~/Snap folder for now")

	s.state.Lock()
	defer s.state.Unlock()

	snapName := "snap-core18-to-core22"
	ts, err := snapstate.Install(context.Background(), s.state, snapName, nil, 0, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg := s.state.NewChange("install", "install a snap")
	chg.AddAll(ts)

	expectedErr := prepFail(s.o, chg)

	s.settle(c)

	c.Assert(chg.Err(), ErrorMatches, fmt.Sprintf(`(.|\s)*%s\)?`, expectedErr.Error()))
	c.Assert(chg.Status(), Equals, state.ErrorStatus)

	containsInOrder(c, s.fakeBackend.ops, []string{"hide-snap-data", "init-exposed-snap-home"})

	// nothing in state
	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, snapName, &snapst), testutil.ErrorIs, state.ErrNoState)

	if expectSeqFile {
		// seq file exists but is zeroed out
		assertMigrationInSeqFile(c, snapName, nil)
	} else {
		exists, _, err := osutil.RegularFileExists(filepath.Join(dirs.SnapSeqDir, snapName+".json"))
		c.Assert(exists, Equals, false)
		c.Assert(err, ErrorMatches, ".*no such file or directory")
	}
}

func (s *snapmgrTestSuite) TestInstallConsidersProvenance(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	ts, err := snapstate.Install(context.Background(), s.state, "provenance-snap", &snapstate.RevisionOptions{Channel: "some-channel"}, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)

	var snapsup snapstate.SnapSetup
	err = ts.Tasks()[0].Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)

	c.Check(snapsup.ExpectedProvenance, Equals, "prov")
}

func (s *snapmgrTestSuite) TestInstallManyConsidersProvenance(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	_, tss, err := snapstate.InstallMany(s.state, []string{"provenance-snap"}, nil, s.user.ID, &snapstate.Flags{})
	c.Assert(err, IsNil)

	var snapsup snapstate.SnapSetup
	err = tss[0].Tasks()[0].Get("snap-setup", &snapsup)
	c.Assert(err, IsNil)

	c.Check(snapsup.ExpectedProvenance, Equals, "prov")
}

func (s *snapmgrTestSuite) TestInstallManyTransactionalWithLane(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lane := s.state.NewLane()
	flags := &snapstate.Flags{
		Transaction: client.TransactionAllSnaps,
		Lane:        lane,
	}
	affected, tss, err := snapstate.InstallMany(s.state, []string{"some-snap", "some-other-snap"}, nil, s.user.ID, flags)
	c.Assert(err, IsNil)
	c.Check(affected, testutil.DeepUnsortedMatches, []string{"some-snap", "some-other-snap"})
	c.Check(tss, HasLen, 2)

	for _, ts := range tss {
		for _, t := range ts.Tasks() {
			c.Assert(t.Lanes(), DeepEquals, []int{lane})
		}
	}
}

func (s *snapmgrTestSuite) TestInstallManyErrorsWithLaneButNoTransaction(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lane := s.state.NewLane()
	flags := &snapstate.Flags{
		Lane: lane,
	}

	affected, tss, err := snapstate.InstallMany(s.state, []string{"some-snap", "some-other-snap"}, nil, s.user.ID, flags)
	c.Assert(err, ErrorMatches, "cannot specify a lane without setting transaction to \"all-snaps\"")
	c.Check(affected, IsNil)
	c.Check(tss, IsNil)

	flags.Transaction = client.TransactionPerSnap

	affected, tss, err = snapstate.InstallMany(s.state, []string{"some-snap", "some-other-snap"}, nil, s.user.ID, flags)
	c.Assert(err, ErrorMatches, "cannot specify a lane without setting transaction to \"all-snaps\"")
	c.Check(affected, IsNil)
	c.Check(tss, IsNil)
}

func (s *snapmgrTestSuite) TestInstallPathManyTransactionalWithLane(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lane := s.state.NewLane()
	flags := &snapstate.Flags{
		Transaction: client.TransactionAllSnaps,
		Lane:        lane,
	}

	var paths []string
	var sideInfos []*snap.SideInfo

	snapNames := []string{"some-snap", "other-snap"}
	for _, name := range snapNames {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("3"),
		}
		sideInfos = append(sideInfos, si)
	}

	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, 0, flags)
	c.Assert(err, IsNil)
	c.Check(tss, HasLen, 2)

	for _, ts := range tss {
		for _, t := range ts.Tasks() {
			c.Assert(t.Lanes(), DeepEquals, []int{lane})
		}
	}
}

func (s *snapmgrTestSuite) TestInstallPathManyErrorsWithLaneButNoTransaction(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	lane := s.state.NewLane()
	flags := &snapstate.Flags{
		Lane: lane,
	}

	var paths []string
	var sideInfos []*snap.SideInfo

	for _, name := range []string{"some-snap", "other-snap"} {
		yaml := fmt.Sprintf(`name: %s
version: 1.0
epoch: 1
`, name)
		paths = append(paths, makeTestSnap(c, yaml))
		si := &snap.SideInfo{
			RealName: name,
			Revision: snap.R("3"),
		}
		sideInfos = append(sideInfos, si)
	}

	tss, err := snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, 0, flags)
	c.Assert(err, ErrorMatches, "cannot specify a lane without setting transaction to \"all-snaps\"")
	c.Check(tss, IsNil)

	flags.Transaction = client.TransactionPerSnap
	tss, err = snapstate.InstallPathMany(context.Background(), s.state, sideInfos, paths, 0, flags)
	c.Assert(err, ErrorMatches, "cannot specify a lane without setting transaction to \"all-snaps\"")
	c.Check(tss, IsNil)
}

func (s *snapmgrTestSuite) TestInstallManyRestartBoundaries(c *C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.state.Lock()
	defer s.state.Unlock()

	r := snapstatetest.MockDeviceModel(DefaultModel())
	defer r()

	// install one we expect gets restart boundary set, and one that we don't expect
	affected, tss, err := snapstate.InstallMany(s.state, []string{"brand-gadget", "some-snap"}, nil, s.user.ID, &snapstate.Flags{})
	c.Assert(err, IsNil)
	c.Check(affected, DeepEquals, []string{"brand-gadget", "some-snap"})
	c.Check(tss, HasLen, 2)

	// only ensure that SetEssentialSnapsRestartBoundaries was actually called, we don't
	// test that all restart boundaries were set, one is enough
	linkSnap1 := tss[0].MaybeEdge(snapstate.MaybeRebootEdge)
	linkSnap2 := tss[1].MaybeEdge(snapstate.MaybeRebootEdge)
	c.Assert(linkSnap1, NotNil)
	c.Assert(linkSnap2, NotNil)

	var boundary restart.RestartBoundaryDirection
	c.Check(linkSnap1.Get("restart-boundary", &boundary), IsNil)
	c.Check(linkSnap2.Get("restart-boundary", &boundary), ErrorMatches, `no state entry for key "restart-boundary"`)
}

func (s *snapmgrTestSuite) TestInstallPathManySplitEssentialWithSharedBase(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	sharedBase := true
	paths, infos := s.setupSplitRefreshAppDependsOnModelBase(c, sharedBase)

	chg := s.state.NewChange("install", "")
	tss, err := snapstate.InstallPathMany(context.Background(), s.state,
		infos, paths, s.user.ID, &snapstate.Flags{NoReRefresh: true})
	c.Assert(err, IsNil)

	for _, ts := range tss {
		chg.AddAll(ts)
	}

	c.Check(chg.CheckTaskDependencies(), IsNil)

	s.settle(c)

	checkRerefresh := false
	s.checkSplitRefreshWithSharedBase(c, chg, checkRerefresh)
}

func (s *snapmgrTestSuite) TestInstallPathManySplitEssentialWithoutSharedBased(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	sharedBase := false
	paths, infos := s.setupSplitRefreshAppDependsOnModelBase(c, sharedBase)

	chg := s.state.NewChange("install", "install path many")
	tss, err := snapstate.InstallPathMany(context.Background(), s.state,
		infos, paths, s.user.ID, nil)
	c.Assert(err, IsNil)

	for _, ts := range tss {
		chg.AddAll(ts)
	}

	c.Check(chg.CheckTaskDependencies(), IsNil)

	s.settle(c)

	for _, snap := range []string{"snapd", "some-snap", "some-base", "some-base-snap"} {
		t := findTaskForSnap(c, chg, "auto-connect", snap)
		c.Assert(t.Status(), Equals, state.DoneStatus, Commentf("expected task %q for snap %q to be done: %s", t.Kind(), snap, t.Status()))
	}

	for _, snap := range []string{"kernel", "gadget", "core18"} {
		t := findTaskForSnap(c, chg, "auto-connect", snap)
		c.Assert(t.Status(), Equals, state.DoStatus, Commentf("expected task %q for snap %q to be do: %s", t.Kind(), snap, t.Status()))
	}

	t := findTaskForSnap(c, chg, "link-snap", "kernel")
	c.Assert(t.Status(), Equals, state.WaitStatus, Commentf("expected kernel's link-snap to be waiting for restart"))

	// waiting for reboot
	for _, snap := range []string{"kernel", "gadget", "core18"} {
		t := findTaskForSnap(c, chg, "auto-connect", snap)
		c.Assert(t.Status(), Equals, state.DoStatus, Commentf("expected task %q for snap %q to be do: %s", t.Kind(), snap, t.Status()))
	}
	s.mockRestartAndSettle(c, chg)

	// completed after restart
	for _, snap := range []string{"kernel", "gadget", "core18"} {
		t := findTaskForSnap(c, chg, "auto-connect", snap)
		c.Assert(t.Status(), Equals, state.DoneStatus, Commentf("expected task %q for snap %q to be done: %s", t.Kind(), snap, t.Status()))
	}

	c.Check(chg.IsReady(), Equals, true)
	c.Check(chg.Status(), Equals, state.DoneStatus)
}

func (s *snapmgrTestSuite) TestInstallManyNoSnaps(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// try to install an empty list of snaps
	_, _, err := snapstate.InstallMany(s.state, nil, nil, s.user.ID, nil)
	c.Check(err, ErrorMatches, "no install/refresh information results from the store")
	c.Check(err, FitsTypeOf, &store.SnapActionError{})
}

func (s *snapmgrTestSuite) TestInstallNoResults(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, noResultsStore{fakeStore: s.fakeStore})

	_, err := snapstate.Install(context.Background(), s.state, "snap", nil, 0, snapstate.Flags{})
	c.Check(err, ErrorMatches, `unexpectedly empty response from the server \(try again later\)`)

	// note that this error is different than the one returned by InstallMany
	// under the same conditions
	c.Check(err, testutil.ErrorIs, snapstate.ErrMissingExpectedResult)
}

func (s *snapmgrTestSuite) TestInstallManyNoResults(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	snapstate.ReplaceStore(s.state, noResultsStore{fakeStore: s.fakeStore})

	_, _, err := snapstate.InstallMany(s.state, []string{"snap"}, nil, 0, nil)
	c.Check(err, ErrorMatches, "no install/refresh information results from the store")

	// normally using errors.Is would be preferred, but the daemon package
	// contains a large switch on the error type, so we need to ensure that the
	// error isn't wrapped
	c.Check(err, FitsTypeOf, &store.SnapActionError{})
}

func (s *snapmgrTestSuite) TestInstallFromStoreOneSnap(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	t := snapstate.StoreInstallGoal(
		snapstate.StoreSnap{
			InstanceName: "some-snap",
		},
		snapstate.StoreSnap{
			InstanceName: "some-other-snap",
		},
	)

	_, _, err := snapstate.InstallOne(context.Background(), s.state, t, snapstate.Options{
		ExpectOneSnap: true,
	})
	c.Check(err, testutil.ErrorIs, snapstate.ErrExpectedOneSnap)

	// the store should never be contacted in this case
	c.Check(s.fakeBackend.ops.Ops(), HasLen, 0)
}

func (s *snapmgrTestSuite) TestInstallOneSnapMisbehavingGoal(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	goal := snapstate.CustomInstallGoal{
		ToInstall: func(context.Context, *state.State, snapstate.Options) ([]snapstate.Target, error) {
			// contents don't matter, we just need to return more than one snap
			return make([]snapstate.Target, 2), nil
		},
	}

	_, _, err := snapstate.InstallOne(context.Background(), s.state, &goal, snapstate.Options{
		ExpectOneSnap: true,
	})
	c.Check(err, testutil.ErrorIs, snapstate.ErrExpectedOneSnap)
}

func (s *snapmgrTestSuite) TestInstallZeroComponentsRunThrough(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName: "test-snap",
		snapType: snap.TypeApp,
	})
}

func (s *snapmgrTestSuite) TestInstallInstanceManyComponentsRunThrough(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName:   "test-snap",
		snapType:   snap.TypeApp,
		components: []string{"standard-component", "standard-component-extra"},
	})
}

func (s *snapmgrTestSuite) TestInstallInstanceManyComponentsRunThroughUndo(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName:   "test-snap",
		snapType:   snap.TypeApp,
		components: []string{"standard-component", "standard-component-extra"},
		undo:       true,
	})
}

func (s *snapmgrTestSuite) TestInstallOneComponentsRunThrough(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName:   "test-snap",
		snapType:   snap.TypeApp,
		components: []string{"standard-component"},
	})
}

func (s *snapmgrTestSuite) TestInstallOneComponentsInstanceKeyRunThrough(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName:    "test-snap",
		instanceKey: "key",
		snapType:    snap.TypeApp,
		components:  []string{"standard-component"},
	})
}

func (s *snapmgrTestSuite) TestInstallOneComponentsInstanceKeyRunThroughUndo(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName:    "test-snap",
		instanceKey: "key",
		snapType:    snap.TypeApp,
		components:  []string{"standard-component"},
		undo:        true,
	})
}

func (s *snapmgrTestSuite) TestInstallManyComponentsRunThrough(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName:   "some-kernel",
		snapType:   snap.TypeKernel,
		components: []string{"standard-component", "kernel-modules-component"},
	})
}

func (s *snapmgrTestSuite) TestInstallManyComponentsRunThroughPreseed(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName:   "some-kernel",
		snapType:   snap.TypeKernel,
		components: []string{"standard-component", "kernel-modules-component"},
		preseed:    true,
	})
}

func (s *snapmgrTestSuite) TestInstallManyComponentsUndoRunThrough(c *C) {
	s.testInstallComponentsRunThrough(c, testInstallComponentsRunThroughOpts{
		snapName:   "some-kernel",
		snapType:   snap.TypeKernel,
		components: []string{"standard-component", "kernel-modules-component"},
		undo:       true,
	})
}

func undoOps(instanceName string, snapType snap.Type, newSequence, prevSequence *sequence.RevisionSideState) []fakeOp {
	forRefresh := prevSequence != nil

	newComponents := newSequence.Components

	installedKmods := make([]*snap.ComponentSideInfo, 0, len(newComponents))
	for _, cs := range newComponents {
		if cs.CompType == snap.KernelModulesComponent {
			installedKmods = append(installedKmods, cs.SideInfo)
		}
	}

	var prevComponents []*sequence.ComponentState
	if prevSequence != nil {
		prevComponents = prevSequence.Components
	}

	prevInstalledKmods := make([]*snap.ComponentSideInfo, 0, len(prevComponents))
	for _, cs := range prevComponents {
		if cs.CompType == snap.KernelModulesComponent {
			prevInstalledKmods = append(prevInstalledKmods, cs.SideInfo)
		}
	}

	var ops fakeOps
	if forRefresh && snapType == snap.TypeKernel {
		// undo for "remove-kernel-snap-setup"
		ops = append(ops, fakeOp{
			op: "prepare-kernel-snap",
		})
	}

	if len(installedKmods) > 0 || len(prevInstalledKmods) > 0 {
		ops = append(ops, fakeOp{
			op:           "prepare-kernel-modules-components",
			currentComps: installedKmods,
			finalComps:   prevInstalledKmods,
		})
	}

	snapRevision := newSequence.Snap.Revision
	var prevRevision snap.Revision
	if forRefresh {
		prevRevision = prevSequence.Snap.Revision
	}

	snapMount := filepath.Join(dirs.SnapMountDir, filepath.Join(instanceName, snapRevision.String()))
	ops = append(ops, []fakeOp{{
		op: "update-aliases",
	}, {
		op:    "auto-connect:Undoing",
		name:  instanceName,
		revno: snapRevision,
	}}...)

	for i := len(newComponents) - 1; i >= 0; i-- {
		csi := newComponents[i].SideInfo
		ops = append(ops, fakeOp{
			op:   "unlink-component",
			path: snap.ComponentMountDir(csi.Component.ComponentName, csi.Revision, instanceName),
		})
	}

	if !forRefresh {
		ops = append(ops, fakeOp{
			op:   "discard-namespace",
			name: instanceName,
		})
	}

	oldMount := "<no-old>"
	oldSaveDir := "<no-old>"
	if forRefresh {
		oldMount = filepath.Join(dirs.SnapMountDir, filepath.Join(instanceName, prevRevision.String()))
		oldSaveDir = filepath.Join(dirs.SnapDataSaveDir, instanceName)
	}

	ops = append(ops, []fakeOp{{
		op:                     "unlink-snap",
		path:                   snapMount,
		unlinkFirstInstallUndo: !forRefresh,
	}, {
		op:    "setup-profiles:Undoing",
		name:  instanceName,
		revno: snapRevision,
	}, {
		op:   "undo-copy-snap-data",
		path: snapMount,
		old:  oldMount,
	}, {
		op:   "undo-setup-snap-save-data",
		path: filepath.Join(dirs.SnapDataSaveDir, instanceName),
		old:  oldSaveDir,
	}}...)

	if !forRefresh {
		ops = append(ops, fakeOp{
			op:   "remove-snap-data-dir",
			name: instanceName,
			path: filepath.Join(dirs.SnapDataDir, instanceName),
		})
		if snapType == snap.TypeKernel {
			ops = append(ops, fakeOp{
				op: "remove-kernel-snap-setup",
			})
		}
	} else {
		if snapType == snap.TypeKernel {
			ops = append(ops, fakeOp{
				op: "remove-kernel-snap-setup",
			})
		}
		ops = append(ops,
			fakeOp{
				op:   "link-snap",
				path: filepath.Join(dirs.SnapMountDir, instanceName, prevRevision.String()),
			},
			fakeOp{
				op:     "maybe-set-next-boot",
				isUndo: true,
			},
		)
	}

	for i := len(newComponents) - 1; i >= 0; i-- {
		csi := newComponents[i].SideInfo

		compName := csi.Component.ComponentName
		compRev := csi.Revision

		containerName := fmt.Sprintf("%s+%s", instanceName, compName)
		filename := fmt.Sprintf("%s_%v.comp", containerName, compRev)

		var oldCS *sequence.ComponentState
		if prevSequence != nil {
			oldCS = prevSequence.FindComponent(csi.Component)
		}

		// if the snap revision isn't changing, then we need to re-link the old
		// component
		if snapRevision == prevRevision {
			ops = append(ops, []fakeOp{{
				op:   "link-component",
				path: snap.ComponentMountDir(compName, oldCS.SideInfo.Revision, instanceName),
			}}...)
		}

		if oldCS == nil || oldCS.SideInfo.Revision != csi.Revision {
			ops = append(ops, []fakeOp{{
				op:                "undo-setup-component",
				containerName:     containerName,
				containerFileName: filename,
			}, {
				op:                "remove-component-dir",
				containerName:     containerName,
				containerFileName: filename,
			}}...)
		}
	}

	if snapRevision != prevRevision {
		ops = append(ops, []fakeOp{{
			op:    "undo-setup-snap",
			name:  instanceName,
			stype: "app",
			path:  snapMount,
		}, {
			op:   "remove-snap-dir",
			name: instanceName,
			path: filepath.Join(dirs.SnapMountDir, instanceName),
		}}...)
	}

	return ops
}

type testInstallComponentsRunThroughOpts struct {
	snapName    string
	instanceKey string
	snapType    snap.Type
	components  []string
	undo        bool
	preseed     bool
}

func (s *snapmgrTestSuite) testInstallComponentsRunThrough(c *C, opts testInstallComponentsRunThroughOpts) {
	s.state.Lock()
	defer s.state.Unlock()

	if opts.preseed {
		restorePreseeding := snapdenv.MockPreseeding(opts.preseed)
		defer restorePreseeding()
		mockCmd := testutil.MockCommand(c, "mount", "")
		defer mockCmd.Restore()
	}

	r := snapstatetest.MockDeviceModel(MakeModel20("pc", map[string]any{"base": "core24"}))
	defer r()

	// sort these so that we can predict the order of the ops
	sort.Strings(opts.components)

	if opts.instanceKey != "" {
		tr := config.NewTransaction(s.state)
		tr.Set("core", "experimental.parallel-instances", true)
		tr.Commit()
	}

	snapID := opts.snapName + "-id"
	snapRevision := snap.R(11)
	const channel = "channel-for-components"

	instanceName := snap.InstanceName(opts.snapName, opts.instanceKey)

	// we start without the auxiliary store info
	c.Check(backend.AuxStoreInfoFilename(snapID), testutil.FileAbsent)

	var componentStates []*sequence.ComponentState
	for i, compName := range opts.components {
		componentStates = append(componentStates, sequence.NewComponentState(&snap.ComponentSideInfo{
			Component: naming.NewComponentRef(opts.snapName, compName),
			Revision:  snap.R(i + 1),
		}, componentNameToType(c, compName)))
	}

	s.fakeStore.snapResourcesFn = func(info *snap.Info) []store.SnapResourceResult {
		c.Assert(info.InstanceName(), DeepEquals, instanceName)
		var results []store.SnapResourceResult
		for _, cs := range componentStates {
			results = append(results, store.SnapResourceResult{
				DownloadInfo: snap.DownloadInfo{
					DownloadURL: "http://example.com/" + cs.SideInfo.Component.ComponentName,
				},
				Name:      cs.SideInfo.Component.ComponentName,
				Revision:  cs.SideInfo.Revision.N,
				Type:      fmt.Sprintf("component/%s", cs.CompType),
				Version:   "1.0",
				CreatedAt: "2024-01-01T00:00:00Z",
			})
		}
		return results
	}

	target := snapstate.StoreInstallGoal(snapstate.StoreSnap{
		InstanceName: instanceName,
		Components:   opts.components,
		RevOpts:      snapstate.RevisionOptions{Channel: channel},
	})

	info, ts, err := snapstate.InstallOne(context.Background(), s.state, target, snapstate.Options{
		UserID: 1,
	})
	c.Assert(err, IsNil)

	c.Check(info.InstanceName(), Equals, instanceName)
	c.Check(info.Channel, Equals, channel)
	c.Check(info.Revision, Equals, snapRevision)

	s.AddCleanup(snapstate.MockReadComponentInfo(func(
		compMntDir string, snapInfo *snap.Info, csi *snap.ComponentSideInfo,
	) (*snap.ComponentInfo, error) {
		for _, cs := range componentStates {
			compName := cs.SideInfo.Component.ComponentName
			compRev := cs.SideInfo.Revision
			if compMntDir == snap.ComponentMountDir(compName, compRev, instanceName) {
				return &snap.ComponentInfo{}, nil
			}
		}
		return nil, fmt.Errorf("unexpected component mount dir %q", compMntDir)
	}))

	chg := s.state.NewChange("install", "install a snap")
	chg.AddAll(ts)

	if opts.undo {
		last := ts.Tasks()[len(ts.Tasks())-1]
		terr := s.state.NewTask("error-trigger", "provoking total undo")
		terr.WaitFor(last)
		chg.AddTask(terr)
	}

	s.settle(c)

	// ensure all our tasks ran
	if opts.undo {
		c.Assert(chg.Err(), NotNil, Commentf("change tasks:\n%s", printTasks(chg.Tasks())))
	} else {
		c.Assert(chg.Err(), IsNil, Commentf("change tasks:\n%s", printTasks(chg.Tasks())))
	}

	c.Assert(chg.IsReady(), Equals, true, Commentf("change tasks:\n%s", printTasks(chg.Tasks())))
	c.Check(snapstate.Installing(s.state), Equals, false)

	downloads := []fakeDownload{{
		macaroon: s.user.StoreMacaroon,
		name:     opts.snapName,
		target:   filepath.Join(dirs.SnapBlobDir, fmt.Sprintf("%s_%v.snap", instanceName, snapRevision)),
	}}
	for i, compName := range opts.components {
		downloads = append(downloads, fakeDownload{
			macaroon: s.user.StoreMacaroon,
			name:     fmt.Sprintf("%s+%s", opts.snapName, compName),
			target:   filepath.Join(dirs.SnapBlobDir, fmt.Sprintf("%s+%s_%d.comp", instanceName, compName, i+1)),
		})
	}
	c.Check(s.fakeStore.downloads, DeepEquals, downloads)
	c.Check(s.fakeStore.seenPrivacyKeys["privacy-key"], Equals, true, Commentf("salts seen: %v", s.fakeStore.seenPrivacyKeys))

	snapFileName := fmt.Sprintf("%s_%v.snap", instanceName, snapRevision)

	// ops that happens before component tasks
	expected := fakeOps{{
		op:     "storesvc-snap-action",
		userID: 1,
	}, {
		op: "storesvc-snap-action:action",
		action: store.SnapAction{
			Action:       "install",
			InstanceName: instanceName,
			Channel:      channel,
		},
		revno:  snapRevision,
		userID: 1,
	}, {
		op:   "storesvc-download",
		name: opts.snapName,
	}, {
		op:    "validate-snap:Doing",
		name:  instanceName,
		revno: snapRevision,
	}}

	// ops for downloading a component (but not yet mounting it)
	for _, cs := range componentStates {
		compName := cs.SideInfo.Component.ComponentName
		compRev := cs.SideInfo.Revision
		containerName := fmt.Sprintf("%s+%s", instanceName, compName)
		filename := fmt.Sprintf("%s_%d.comp", containerName, compRev.N)

		expected = append(expected, []fakeOp{{
			op:   "storesvc-download",
			name: cs.SideInfo.Component.String(),
		}, {
			op:                "validate-component:Doing",
			name:              instanceName,
			revno:             snapRevision,
			componentName:     compName,
			componentPath:     filepath.Join(dirs.SnapBlobDir, filename),
			componentRev:      compRev,
			componentSideInfo: *cs.SideInfo,
		}}...)
	}

	expected = append(expected, []fakeOp{{
		op:  "current",
		old: "<no-current>",
	}, {
		op:   "open-snap-file",
		path: filepath.Join(dirs.SnapBlobDir, snapFileName),
		sinfo: snap.SideInfo{
			RealName: opts.snapName,
			SnapID:   snapID,
			Channel:  channel,
			Revision: snapRevision,
		},
	}, {
		op:    "setup-snap",
		name:  instanceName,
		path:  filepath.Join(dirs.SnapBlobDir, snapFileName),
		revno: snapRevision,
	}}...)

	// ops for mounting a component (but not yet linking it)
	for _, cs := range componentStates {
		compName := cs.SideInfo.Component.ComponentName
		compRev := cs.SideInfo.Revision
		containerName := fmt.Sprintf("%s+%s", instanceName, compName)

		expected = append(expected, fakeOp{
			op:                "setup-component",
			containerName:     containerName,
			containerFileName: fmt.Sprintf("%s_%d.comp", containerName, compRev.N),
		})
	}

	if opts.snapName == "some-kernel" {
		expected = append(expected, []fakeOp{{
			op: "prepare-kernel-snap",
		}, {
			op:    "update-gadget-assets:Doing",
			name:  instanceName,
			revno: snapRevision,
		}}...)
	}

	expected = append(expected, []fakeOp{{
		op:   "copy-data",
		path: filepath.Join(dirs.SnapMountDir, filepath.Join(instanceName, snapRevision.String())),
		old:  "<no-old>",
	}, {
		op:   "setup-snap-save-data",
		path: filepath.Join(dirs.SnapDataSaveDir, instanceName),
	}, {
		op:    "setup-profiles:Doing",
		name:  instanceName,
		revno: snapRevision,
	}, {
		op: "candidate",
		sinfo: snap.SideInfo{
			RealName: opts.snapName,
			SnapID:   snapID,
			Channel:  channel,
			Revision: snapRevision,
		},
	}, {
		op:                  "link-snap",
		path:                filepath.Join(dirs.SnapMountDir, filepath.Join(instanceName, snapRevision.String())),
		requireSnapdTooling: true,
	}}...)

	kmodComps := make([]*snap.ComponentSideInfo, 0, len(opts.components))
	for i, compName := range opts.components {
		csi := snap.ComponentSideInfo{
			Component: naming.NewComponentRef(opts.snapName, compName),
			Revision:  snap.R(i + 1),
		}
		if strings.HasPrefix(compName, string(snap.KernelModulesComponent)) {
			kmodComps = append(kmodComps, &csi)
		}
	}

	if len(kmodComps) == 0 {
		expected = append(expected, fakeOp{
			op: "maybe-set-next-boot",
		})
	}

	// ops for linking components
	for _, cs := range componentStates {
		compName := cs.SideInfo.Component.ComponentName
		compRev := cs.SideInfo.Revision
		expected = append(expected, []fakeOp{{
			op:   "link-component",
			path: snap.ComponentMountDir(compName, compRev, instanceName),
		}}...)
	}

	// ops that follow linking components
	expected = append(expected, []fakeOp{{
		op:    "auto-connect:Doing",
		name:  instanceName,
		revno: snapRevision,
	}, {
		op: "update-aliases",
	}}...)

	if len(kmodComps) > 0 {
		if opts.preseed {
			expected = append(expected, fakeOp{
				op:           "prepare-kernel-modules-components",
				currentComps: []*snap.ComponentSideInfo{},
				finalComps:   kmodComps,
			})
		}
		expected = append(expected, fakeOp{
			op:           "prepare-kernel-modules-components",
			currentComps: []*snap.ComponentSideInfo{},
			finalComps:   kmodComps,
		}, fakeOp{
			op: "maybe-set-next-boot",
		})
	}

	expectedSeq := sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: opts.snapName,
		SnapID:   snapID,
		Channel:  channel,
		Revision: snapRevision,
	}, componentStates)

	if opts.undo {
		expected = append(expected, undoOps(instanceName, opts.snapType, expectedSeq, nil)...)
	} else {
		expected = append(expected, fakeOp{
			op:    "cleanup-trash",
			name:  instanceName,
			revno: snapRevision,
		})
	}

	compsups := make([]snapstate.ComponentSetup, 0, len(opts.components))
	for i, comp := range opts.components {
		compsups = append(compsups, snapstate.ComponentSetup{
			CompSideInfo: &snap.ComponentSideInfo{
				Component: naming.NewComponentRef(opts.snapName, comp),
				Revision:  snap.R(i + 1),
			},
			CompType: componentNameToType(c, comp),
			DownloadInfo: &snap.DownloadInfo{
				DownloadURL: "http://example.com/" + comp,
			},
			CompPath: filepath.Join(dirs.SnapBlobDir, fmt.Sprintf("%s+%s_%d.comp", instanceName, comp, i+1)),
			ComponentInstallFlags: snapstate.ComponentInstallFlags{
				MultiComponentInstall: true,
			},
		})
	}
	checkComponentSetupTasks(c, ts, compsups, "download-component")

	// start with an easier-to-read error if this fails:
	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Assert(s.fakeBackend.ops, DeepEquals, expected)

	if opts.undo {
		var snapst snapstate.SnapState
		err = snapstate.Get(s.state, instanceName, &snapst)
		c.Assert(err, testutil.ErrorIs, state.ErrNoState)

		c.Check(backend.AuxStoreInfoFilename(snapID), testutil.FileAbsent)
	} else {
		// verify snap in the system state
		var snapst snapstate.SnapState
		err = snapstate.Get(s.state, instanceName, &snapst)
		c.Assert(err, IsNil)

		c.Check(snapst.Active, Equals, true)
		c.Check(snapst.TrackingChannel, Equals, fmt.Sprintf("%s/stable", channel))
		c.Check(snapst.Required, Equals, false)

		c.Check(snapst.CurrentSideInfo(), DeepEquals, &snap.SideInfo{
			RealName: opts.snapName,
			Channel:  channel,
			Revision: snapRevision,
			SnapID:   snapID,
		})

		var compst []*sequence.ComponentState
		for i, compName := range opts.components {
			compst = append(compst, &sequence.ComponentState{
				SideInfo: &snap.ComponentSideInfo{
					Component: naming.NewComponentRef(opts.snapName, compName),
					Revision:  snap.R(i + 1),
				},
				CompType: componentNameToType(c, compName),
			})
		}

		// make sure that all of our components are accounted for
		c.Assert(snapst.Sequence.Revisions[0].Components, DeepEquals, componentStates)

		c.Check(backend.AuxStoreInfoFilename(snapID), testutil.FilePresent)
	}
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathNoneRunThrough(c *C) {
	s.testInstallComponentsFromPathRunThrough(c, testInstallComponentsFromPathRunThroughOpts{
		snapName: "test-snap",
		snapType: snap.TypeApp,
	})
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathOneRunThrough(c *C) {
	s.testInstallComponentsFromPathRunThrough(c, testInstallComponentsFromPathRunThroughOpts{
		snapName:   "test-snap",
		snapType:   snap.TypeApp,
		components: []string{"standard-component"},
	})
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathOneRunThroughUndo(c *C) {
	s.testInstallComponentsFromPathRunThrough(c, testInstallComponentsFromPathRunThroughOpts{
		snapName:   "test-snap",
		snapType:   snap.TypeApp,
		components: []string{"standard-component"},
		undo:       true,
	})
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathUnassertedRunThrough(c *C) {
	s.testInstallComponentsFromPathRunThrough(c, testInstallComponentsFromPathRunThroughOpts{
		snapName:   "test-snap",
		snapType:   snap.TypeApp,
		components: []string{"standard-component"},
		unasserted: true,
	})
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathRunThrough(c *C) {
	s.testInstallComponentsFromPathRunThrough(c, testInstallComponentsFromPathRunThroughOpts{
		snapName:   "some-kernel",
		snapType:   snap.TypeKernel,
		components: []string{"standard-component", "kernel-modules-component"},
	})
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathRunThroughUndo(c *C) {
	s.testInstallComponentsFromPathRunThrough(c, testInstallComponentsFromPathRunThroughOpts{
		snapName:   "some-kernel",
		snapType:   snap.TypeKernel,
		components: []string{"standard-component", "kernel-modules-component"},
		undo:       true,
	})
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathManyRemovePaths(c *C) {
	s.testInstallComponentsFromPathRunThrough(c, testInstallComponentsFromPathRunThroughOpts{
		snapName:    "some-kernel",
		snapType:    snap.TypeKernel,
		components:  []string{"standard-component", "kernel-modules-component"},
		removePaths: true,
	})
}

type testInstallComponentsFromPathRunThroughOpts struct {
	snapName    string
	snapType    snap.Type
	instanceKey string
	components  []string
	undo        bool
	removePaths bool
	unasserted  bool
}

func (s *snapmgrTestSuite) testInstallComponentsFromPathRunThrough(c *C, opts testInstallComponentsFromPathRunThroughOpts) {
	s.state.Lock()
	defer s.state.Unlock()

	r := snapstatetest.MockDeviceModel(MakeModel20("pc", map[string]any{"base": "core24"}))
	defer r()

	// make sure that the store is never hit
	snapstate.ReplaceStore(s.state, &storetest.Store{})

	sort.Strings(opts.components)

	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	if opts.instanceKey != "" {
		tr := config.NewTransaction(s.state)
		tr.Set("core", "experimental.parallel-instances", true)
		tr.Commit()
	}

	snapID := "test-snap-id"
	snapRevision := snap.R(11)
	if opts.unasserted {
		snapID = ""
		snapRevision = snap.R(-1)
	}

	instanceName := snap.InstanceName(opts.snapName, opts.instanceKey)

	components := make([]snapstate.PathComponent, 0, len(opts.components))
	compPaths := make(map[string]string, len(opts.components))
	compRevs := make(map[string]snap.Revision)
	for i, compName := range opts.components {
		if opts.unasserted {
			compRevs[compName] = snap.R(-1)
		} else {
			compRevs[compName] = snap.R(i + 1)
		}
	}

	var componentStates []*sequence.ComponentState
	for _, compName := range opts.components {
		csi := &snap.ComponentSideInfo{
			Component: naming.NewComponentRef(opts.snapName, compName),
			Revision:  compRevs[compName],
		}
		if opts.unasserted {
			csi.Revision = snap.Revision{}
		}

		componentYaml := fmt.Sprintf(`component: %s
type: %s
version: 1.0
`, csi.Component, componentNameToType(c, compName))

		path := snaptest.MakeTestComponent(c, componentYaml)
		compPaths[csi.Component.ComponentName] = path
		components = append(components, snapstate.PathComponent{
			SideInfo: csi,
			Path:     path,
		})

		csSideInfo := &snap.ComponentSideInfo{
			Component: naming.NewComponentRef(opts.snapName, compName),
			Revision:  compRevs[compName],
		}
		componentStates = append(componentStates, sequence.NewComponentState(csSideInfo, componentNameToType(c, compName)))
	}

	s.AddCleanup(snapstate.MockReadComponentInfo(func(
		compMntDir string, snapInfo *snap.Info, csi *snap.ComponentSideInfo,
	) (*snap.ComponentInfo, error) {
		for _, compName := range opts.components {
			if compMntDir == snap.ComponentMountDir(compName, compRevs[compName], instanceName) {
				return &snap.ComponentInfo{}, nil
			}
		}
		return nil, fmt.Errorf("unexpected component mount dir %q", compMntDir)
	}))

	var snapPath string
	switch opts.snapType {
	case snap.TypeKernel:
		snapPath = makeTestSnap(c, `name: some-kernel
version: 1.0
type: kernel
components:
  standard-component:
    type: standard
  kernel-modules-component:
    type: kernel-modules
`)
	case snap.TypeApp:
		snapPath = makeTestSnap(c, `name: test-snap
version: 1.0
type: app
components:
  standard-component:
    type: standard
  standard-component-extra:
    type: standard
`)
	}

	chg := s.state.NewChange("install", "install a local snap")

	si := &snap.SideInfo{
		RealName: opts.snapName,
		SnapID:   snapID,
		Revision: snapRevision,
	}
	if opts.unasserted {
		si.Revision = snap.Revision{}
	}

	goal := snapstate.PathInstallGoal(snapstate.PathSnap{
		InstanceName: instanceName,
		Path:         snapPath,
		SideInfo:     si,
		Components:   components,
	})

	info, ts, err := snapstate.InstallOne(context.Background(), s.state, goal, snapstate.Options{
		Flags: snapstate.Flags{
			Required:       true,
			RemoveSnapPath: opts.removePaths,
		},
	})
	c.Assert(err, IsNil)
	c.Check(info.InstanceName(), Equals, instanceName)
	c.Check(info.Revision, Equals, si.Revision)

	chg.AddAll(ts)

	if opts.undo {
		last := ts.Tasks()[len(ts.Tasks())-1]
		terr := s.state.NewTask("error-trigger", "provoking total undo")
		terr.WaitFor(last)
		chg.AddTask(terr)
	}

	s.settle(c)

	c.Assert(chg.IsReady(), Equals, true, Commentf("change tasks:\n%s", printTasks(chg.Tasks())))

	if opts.undo {
		c.Assert(chg.Err(), NotNil, Commentf("change tasks:\n%s", printTasks(chg.Tasks())))
	} else {
		c.Assert(chg.Err(), IsNil, Commentf("change tasks:\n%s", printTasks(chg.Tasks())))
	}

	var expected fakeOps
	for _, cs := range componentStates {
		compName := cs.SideInfo.Component.ComponentName
		if !opts.unasserted {
			expected = append(expected, fakeOp{
				op:                              "validate-component:Doing",
				name:                            instanceName,
				revno:                           snapRevision,
				componentName:                   compName,
				componentPath:                   compPaths[compName],
				componentRev:                    compRevs[compName],
				componentSideInfo:               *cs.SideInfo,
				componentSkipAssertionsDownload: true,
			})
		}
	}

	expected = append(expected, fakeOps{{
		op:  "current",
		old: "<no-current>",
	}, {
		op:    "setup-snap",
		name:  instanceName,
		path:  snapPath,
		revno: snapRevision,
	}}...)

	for _, cs := range componentStates {
		compName := cs.SideInfo.Component.ComponentName
		containerName := fmt.Sprintf("%s+%s", instanceName, compName)
		filename := fmt.Sprintf("%s_%s.comp", containerName, cs.SideInfo.Revision)
		expected = append(expected, fakeOp{
			op:                "setup-component",
			containerName:     containerName,
			containerFileName: filename,
		})
	}

	if opts.snapType == snap.TypeKernel {
		expected = append(expected, fakeOp{
			op: "prepare-kernel-snap",
		}, fakeOp{
			op:    "update-gadget-assets:Doing",
			name:  instanceName,
			revno: snapRevision,
		})
	}
	expected = append(expected, []fakeOp{{
		op:   "copy-data",
		path: filepath.Join(dirs.SnapMountDir, filepath.Join(instanceName, snapRevision.String())),
		old:  "<no-old>",
	}, {
		op:   "setup-snap-save-data",
		path: filepath.Join(dirs.SnapDataSaveDir, instanceName),
	}, {
		op:    "setup-profiles:Doing",
		name:  instanceName,
		revno: snapRevision,
	}, {
		op: "candidate",
		sinfo: snap.SideInfo{
			RealName: opts.snapName,
			SnapID:   snapID,
			Revision: snapRevision,
		},
	}, {
		op:                  "link-snap",
		path:                filepath.Join(dirs.SnapMountDir, filepath.Join(instanceName, snapRevision.String())),
		requireSnapdTooling: true,
	}}...)

	kmodComps := make([]*snap.ComponentSideInfo, 0, len(components))
	for _, cs := range componentStates {
		if cs.CompType == snap.KernelModulesComponent {
			kmodComps = append(kmodComps, cs.SideInfo)
		}
	}

	if len(kmodComps) == 0 {
		expected = append(expected, fakeOp{
			op: "maybe-set-next-boot",
		})
	}

	for _, compName := range opts.components {
		expected = append(expected, fakeOp{
			op:   "link-component",
			path: snap.ComponentMountDir(compName, compRevs[compName], instanceName),
		})
	}

	expected = append(expected, []fakeOp{{
		op:    "auto-connect:Doing",
		name:  instanceName,
		revno: snapRevision,
	}, {
		op: "update-aliases",
	}}...)

	if len(kmodComps) > 0 {
		expected = append(expected, fakeOp{
			op:           "prepare-kernel-modules-components",
			currentComps: []*snap.ComponentSideInfo{},
			finalComps:   kmodComps,
		}, fakeOp{
			op: "maybe-set-next-boot",
		})
	}

	expectedSeq := sequence.NewRevisionSideState(si, componentStates)

	if opts.undo {
		expected = append(expected, undoOps(instanceName, opts.snapType, expectedSeq, nil)...)
	} else {
		expected = append(expected, fakeOp{
			op:    "cleanup-trash",
			name:  instanceName,
			revno: snapRevision,
		})
	}

	compsups := make([]snapstate.ComponentSetup, 0, len(components))
	for _, comp := range opts.components {
		compsups = append(compsups, snapstate.ComponentSetup{
			CompSideInfo: &snap.ComponentSideInfo{
				Component: naming.NewComponentRef(opts.snapName, comp),
				Revision:  compRevs[comp],
			},
			CompType: componentNameToType(c, comp),
			CompPath: compPaths[comp],
			ComponentInstallFlags: snapstate.ComponentInstallFlags{
				MultiComponentInstall: true,
				RemoveComponentPath:   opts.removePaths,
			},
			SkipAssertionsDownload: true,
		})
	}
	checkComponentSetupTasks(c, ts, compsups, "prepare-component")

	c.Assert(s.fakeBackend.ops.Ops(), DeepEquals, expected.Ops())
	c.Assert(s.fakeBackend.ops, DeepEquals, expected)

	if opts.undo {
		var snapst snapstate.SnapState
		err = snapstate.Get(s.state, instanceName, &snapst)
		c.Assert(err, testutil.ErrorIs, state.ErrNoState)
	} else {
		// verify snap in the system state
		var snapst snapstate.SnapState
		err = snapstate.Get(s.state, instanceName, &snapst)
		c.Assert(err, IsNil)

		c.Check(snapst.Active, Equals, true)
		c.Check(snapst.TrackingChannel, Equals, "")
		c.Check(snapst.Required, Equals, true)

		c.Check(snapst.CurrentSideInfo(), DeepEquals, &snap.SideInfo{
			RealName: opts.snapName,
			Revision: snapRevision,
			SnapID:   snapID,
		})

		// make sure that all of our components are accounted for
		c.Assert(snapst.Sequence.Revisions[0].Components, DeepEquals, componentStates)

		// if we requested that the snap be removed after install, then make
		// sure it is gone. otherwise, make sure that it is still there
		fileChecker := testutil.FilePresent
		if opts.removePaths {
			fileChecker = testutil.FileAbsent
		}

		c.Check(snapPath, fileChecker)
		for _, comp := range components {
			c.Check(comp.Path, fileChecker)
		}
	}
}

func printTasks(ts []*state.Task) string {
	var b strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&b, "task %s (%s), %s, %s\n", t.Kind(), t.ID(), t.Summary(), t.Status())
		if t.Status() != state.DoneStatus {
			fmt.Fprintln(&b, "  waiting on:")
			for _, w := range t.WaitTasks() {
				fmt.Fprintf(&b, "  - %s (%s), %s\n", w.Kind(), w.ID(), w.Status())
			}
		}
	}
	return b.String()
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathInvalidComponentFile(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	const (
		snapID        = "test-snap-id"
		snapName      = "test-snap"
		componentName = "standard-component"
	)
	snapRevision := snap.R(11)

	csi := snap.ComponentSideInfo{
		Component: naming.NewComponentRef(snapName, componentName),
		Revision:  snap.R(1),
	}

	compPath := filepath.Join(c.MkDir(), "invalid-component")
	err := os.WriteFile(compPath, []byte("invalid-component"), 0644)
	c.Assert(err, IsNil)

	components := []snapstate.PathComponent{{
		SideInfo: &csi,
		Path:     compPath,
	}}

	snapPath := makeTestSnap(c, `name: test-snap
version: 1.0
components:
  standard-component:
    type: standard
`)
	si := &snap.SideInfo{
		RealName: snapName,
		SnapID:   snapID,
		Revision: snapRevision,
	}

	goal := snapstate.PathInstallGoal(snapstate.PathSnap{
		Path:       snapPath,
		SideInfo:   si,
		Components: components,
	})
	_, _, err = snapstate.InstallOne(context.Background(), s.state, goal, snapstate.Options{})
	c.Assert(err, ErrorMatches, fmt.Sprintf(`.*cannot process snap or snapdir: file "%s" is invalid.*`, compPath))
}

func (s *snapmgrTestSuite) TestInstallComponentsFromPathInvalidComponentName(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// use the real thing for this one
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	const (
		snapID        = "test-snap-id"
		snapName      = "test-snap"
		componentName = "Bad-component"
	)
	snapRevision := snap.R(11)

	csi := snap.ComponentSideInfo{
		Component: naming.NewComponentRef(snapName, componentName),
		Revision:  snap.R(1),
	}

	components := []snapstate.PathComponent{{
		SideInfo: &csi,
	}}

	snapPath := makeTestSnap(c, `name: test-snap
version: 1.0
components:
  standard-component:
    type: standard
`)
	si := &snap.SideInfo{
		RealName: snapName,
		SnapID:   snapID,
		Revision: snapRevision,
	}

	goal := snapstate.PathInstallGoal(snapstate.PathSnap{
		Path:       snapPath,
		SideInfo:   si,
		Components: components,
	})
	_, _, err := snapstate.InstallOne(context.Background(), s.state, goal, snapstate.Options{})
	c.Assert(err, ErrorMatches, fmt.Sprintf(`invalid snap name: "%s"`, componentName))
}

func (s *snapmgrTestSuite) TestInstallWithConfdb(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	chg := s.state.NewChange("test", "test change")
	ts, err := snapstate.Install(context.Background(), s.state, "some-snap", &snapstate.RevisionOptions{Channel: "channel-for-confdb"}, s.user.ID, snapstate.Flags{})
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	checkSnapsupHasConfdb(ts, c)
}

type testInstallComponentsValidationSetsOpts struct {
	comps   []string
	err     string
	headers map[string]any
}

func (s *validationSetsSuite) TestInstallComponentsValidationSetsInvalid(c *C) {
	s.testInstallComponentsValidationSets(c, testInstallComponentsValidationSetsOpts{
		comps: []string{"test-component"},
		err:   `cannot install component "test-snap\+test-component" due to enforcing rules of validation set 16/foo/bar/1`,
		headers: map[string]any{
			"components": map[string]any{
				"test-component": "invalid",
			},
		},
	})
}

func (s *validationSetsSuite) TestInstallComponentsValidationSetsMissingRequired(c *C) {
	s.testInstallComponentsValidationSets(c, testInstallComponentsValidationSetsOpts{
		comps: nil,
		err:   `cannot install snap "test-snap" without component "test-component" required by validation sets 16/foo/bar/1`,
		headers: map[string]any{
			"components": map[string]any{
				"test-component": "required",
			},
		},
	})
}

func (s *validationSetsSuite) TestInstallComponentsValidationSetsWrongRevision(c *C) {
	s.testInstallComponentsValidationSets(c, testInstallComponentsValidationSetsOpts{
		comps: []string{"test-component"},
		err:   `cannot install component "test-snap\+test-component" at revision 1 without --ignore-validation, revision 10 is required by validation sets: 16/foo/bar/1`,
		headers: map[string]any{
			"revision": "7",
			"components": map[string]any{
				"test-component": map[string]any{
					"presence": "optional",
					"revision": "10",
				},
			},
		},
	})
}

func (s *validationSetsSuite) TestInstallComponentsValidationSetsCorrectRevision(c *C) {
	s.testInstallComponentsValidationSets(c, testInstallComponentsValidationSetsOpts{
		comps: []string{"test-component"},
		headers: map[string]any{
			"revision": "7",
			"components": map[string]any{
				"test-component": map[string]any{
					"presence": "optional",
					"revision": "1",
				},
			},
		},
	})
}

func (s *validationSetsSuite) TestInstallComponentsValidationSetsRequired(c *C) {
	s.testInstallComponentsValidationSets(c, testInstallComponentsValidationSetsOpts{
		comps: []string{"test-component"},
		headers: map[string]any{
			"components": map[string]any{
				"test-component": map[string]any{
					"presence": "required",
				},
			},
		},
	})
}

func (s *validationSetsSuite) testInstallComponentsValidationSets(c *C, opts testInstallComponentsValidationSetsOpts) {
	s.state.Lock()
	defer s.state.Unlock()

	const (
		snapName = "test-snap"
		channel  = "channel-for-components"
	)

	snapID := snaptest.AssertedSnapID(snapName)

	// make sure the fake store knows to use this id, since this is what will be
	// in the validation set
	s.fakeStore.registerID(snapName, snapID)
	s.fakeStore.mutateSnapInfo = func(info *snap.Info) error {
		info.Components = map[string]*snap.Component{
			"test-component": {
				Type: snap.TestComponent,
				Name: "test-component",
			},
		}
		return nil
	}

	restore := snapstate.MockEnforcedValidationSets(func(st *state.State, extraVss ...*asserts.ValidationSet) (*snapasserts.ValidationSets, error) {
		vs := snapasserts.NewValidationSets()
		headers := map[string]any{
			"id":       snapID,
			"name":     snapName,
			"presence": "optional",
		}
		for k, v := range opts.headers {
			headers[k] = v
		}
		vsa := s.mockValidationSetAssert(c, "bar", "1", headers)
		err := vs.Add(vsa.(*asserts.ValidationSet))
		c.Assert(err, IsNil)
		return vs, nil
	})
	defer restore()

	s.fakeStore.snapResourcesFn = func(info *snap.Info) []store.SnapResourceResult {
		c.Assert(info.InstanceName(), DeepEquals, snapName)
		return []store.SnapResourceResult{
			{
				DownloadInfo: snap.DownloadInfo{
					DownloadURL: "http://example.com/test-component",
				},
				Name:      "test-component",
				Revision:  1,
				Type:      "component/test",
				Version:   "1.0",
				CreatedAt: "2024-01-01T00:00:00Z",
			},
		}
	}

	goal := snapstate.StoreInstallGoal(snapstate.StoreSnap{
		InstanceName: snapName,
		Components:   opts.comps,
		RevOpts:      snapstate.RevisionOptions{Channel: channel},
	})

	_, _, err := snapstate.InstallOne(context.Background(), s.state, goal, snapstate.Options{})
	if len(opts.err) == 0 {
		c.Assert(err, IsNil)
	} else {
		c.Assert(err, ErrorMatches, opts.err)

		goal := snapstate.StoreInstallGoal(snapstate.StoreSnap{
			InstanceName: snapName,
			Components:   opts.comps,
			RevOpts:      snapstate.RevisionOptions{Channel: channel},
		})

		// if we're expecting an error, we should be able to ignore validation
		// and the error shouldn't happen
		_, _, err := snapstate.InstallOne(context.Background(), s.state, goal, snapstate.Options{
			Flags: snapstate.Flags{IgnoreValidation: true},
		})
		c.Assert(err, IsNil)
	}
}

type testUpdateComponentsValidationSetsOpts struct {
	comps   []string
	err     string
	headers map[string]any
}

func (s *validationSetsSuite) TestUpdateComponentsValidationSetsToWrongRevision(c *C) {
	s.testUpdateComponentsValidationSets(c, testUpdateComponentsValidationSetsOpts{
		comps: []string{"test-component"},
		err:   `cannot update component "test-snap\+test-component" to revision 2 without --ignore-validation, revision 1 is required by validation sets: 16/foo/bar/1`,
		headers: map[string]any{
			"revision": "11",
			"components": map[string]any{
				"test-component": map[string]any{
					"presence": "optional",
					"revision": "1",
				},
			},
		},
	})
}

func (s *validationSetsSuite) TestUpdateComponentsValidationSetsToCorrectRevision(c *C) {
	s.testUpdateComponentsValidationSets(c, testUpdateComponentsValidationSetsOpts{
		comps: []string{"test-component"},
		headers: map[string]any{
			"revision": "11",
			"components": map[string]any{
				"test-component": map[string]any{
					"presence": "optional",
					"revision": "2",
				},
			},
		},
	})
}

func (s *validationSetsSuite) TestUpdateComponentsValidationSetsMissingRequired(c *C) {
	s.testUpdateComponentsValidationSets(c, testUpdateComponentsValidationSetsOpts{
		comps: nil,
		err:   `cannot update snap "test-snap" without component "test-component" required by validation sets 16/foo/bar/1`,
		headers: map[string]any{
			"components": map[string]any{
				"test-component": map[string]any{
					"presence": "required",
				},
			},
		},
	})
}

func (s *validationSetsSuite) TestUpdateComponentsValidationSetsWithRequired(c *C) {
	s.testUpdateComponentsValidationSets(c, testUpdateComponentsValidationSetsOpts{
		comps: []string{"test-component"},
		headers: map[string]any{
			"components": map[string]any{
				"test-component": map[string]any{
					"presence": "required",
				},
			},
		},
	})
}

func (s *validationSetsSuite) testUpdateComponentsValidationSets(c *C, opts testUpdateComponentsValidationSetsOpts) {
	s.state.Lock()
	defer s.state.Unlock()

	tr := config.NewTransaction(s.state)
	tr.Set("core", "experimental.parallel-instances", true)
	tr.Commit()

	const (
		snapName    = "test-snap"
		instanceKey = "key"
		channel     = "channel-for-components"
	)
	instanceName := snap.InstanceName(snapName, instanceKey)

	snapID := snaptest.AssertedSnapID(snapName)

	revisionSideState := sequence.NewRevisionSideState(&snap.SideInfo{
		RealName: snapName,
		Revision: snap.R(7),
		SnapID:   snapID,
	}, []*sequence.ComponentState{sequence.NewComponentState(
		&snap.ComponentSideInfo{
			Component: naming.NewComponentRef(snapName, "test-component"),
			Revision:  snap.R(1),
		},
		snap.TestComponent,
	)})

	snapstate.Set(s.state, instanceName, &snapstate.SnapState{
		Active:      true,
		Sequence:    sequence.SnapSequence{Revisions: []*sequence.RevisionSideState{revisionSideState}},
		Current:     snap.R(7),
		InstanceKey: instanceKey,
		SnapType:    "app",
	})

	restore := snapstate.MockReadComponentInfo(func(
		compMntDir string, snapInfo *snap.Info, csi *snap.ComponentSideInfo,
	) (*snap.ComponentInfo, error) {
		return &snap.ComponentInfo{
			Component: csi.Component,
		}, nil
	})
	defer restore()

	// make sure the fake store knows to use this id, since this is what will be
	// in the validation set
	s.fakeStore.registerID(snapName, snapID)
	s.fakeStore.mutateSnapInfo = func(info *snap.Info) error {
		comps := make(map[string]*snap.Component)
		for _, c := range opts.comps {
			comps[c] = &snap.Component{
				Type: snap.TestComponent,
				Name: c,
			}
		}
		info.Components = comps
		return nil
	}

	restore = snapstate.MockEnforcedValidationSets(func(st *state.State, extraVss ...*asserts.ValidationSet) (*snapasserts.ValidationSets, error) {
		vs := snapasserts.NewValidationSets()
		headers := map[string]any{
			"id":       snapID,
			"name":     snapName,
			"presence": "optional",
		}
		for k, v := range opts.headers {
			headers[k] = v
		}
		vsa := s.mockValidationSetAssert(c, "bar", "1", headers)
		err := vs.Add(vsa.(*asserts.ValidationSet))
		c.Assert(err, IsNil)
		return vs, nil
	})
	defer restore()

	s.fakeStore.snapResourcesFn = func(info *snap.Info) []store.SnapResourceResult {
		c.Assert(info.InstanceName(), DeepEquals, instanceName)
		results := make([]store.SnapResourceResult, 0, len(opts.comps))
		for _, c := range opts.comps {
			results = append(results, store.SnapResourceResult{
				DownloadInfo: snap.DownloadInfo{
					DownloadURL: "http://example.com/" + c,
				},
				Name:      c,
				Revision:  2,
				Type:      "component/test",
				Version:   "1.0",
				CreatedAt: "2024-01-01T00:00:00Z",
			})
		}
		return results
	}

	goal := snapstate.StoreUpdateGoal(snapstate.StoreUpdate{
		InstanceName: instanceName,
		RevOpts:      snapstate.RevisionOptions{Channel: channel},
	})

	_, err := snapstate.UpdateOne(context.Background(), s.state, goal, nil, snapstate.Options{})
	if len(opts.err) == 0 {
		c.Assert(err, IsNil)
	} else {
		c.Assert(err, ErrorMatches, opts.err)

		goal := snapstate.StoreUpdateGoal(snapstate.StoreUpdate{
			InstanceName: instanceName,
			RevOpts:      snapstate.RevisionOptions{Channel: channel},
		})

		// if we're expecting an error, we should be able to ignore validation
		// and the error shouldn't happen
		_, err := snapstate.UpdateOne(context.Background(), s.state, goal, nil, snapstate.Options{
			Flags: snapstate.Flags{IgnoreValidation: true},
		})
		c.Assert(err, IsNil)
	}
}
