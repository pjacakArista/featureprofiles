// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package components provides functions to enumerate components from the device.
package components

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/openconfig/featureprofiles/internal/deviations"
	tpb "github.com/openconfig/gnoi/types"
	"github.com/openconfig/ondatra"
	"github.com/openconfig/ondatra/gnmi"
	"github.com/openconfig/ondatra/gnmi/oc"
	"github.com/openconfig/ondatra/gnmi/oc/ocpath"
	"github.com/openconfig/ygnmi/ygnmi"
)

const (
	activeController  = oc.Platform_ComponentRedundantRole_PRIMARY
	standbyController = oc.Platform_ComponentRedundantRole_SECONDARY
)

// componentsCache stores fetched components per device.
// The key is the DUT name. This cache persists across different test functions
// within the same execution of `go test`.
var componentsCache = make(map[string][]*oc.Component)

// fetchCachedComponents fetches all components from the DUT, using a package-level cache.
func fetchCachedComponents(t *testing.T, dut *ondatra.DUTDevice) []*oc.Component {
	dutName := dut.Name()
	if cached, ok := componentsCache[dutName]; ok {
		t.Logf("Using cached components for DUT %s.", dutName)
		return cached
	}
	t.Logf("Fetching all components for DUT %s.", dutName)
	components := gnmi.GetAll[*oc.Component](t, dut, gnmi.OC().ComponentAny().State())
	componentsCache[dutName] = components
	return components
}

// FindComponentsByType finds the list of components based on hardware type.
func FindComponentsByType(t *testing.T, dut *ondatra.DUTDevice, cType oc.E_PlatformTypes_OPENCONFIG_HARDWARE_COMPONENT) []string {
	t.Helper()
	components := fetchCachedComponents(t, dut)
	var s []string
	for _, c := range components {
		switch v := c.GetType().(type) {
		case oc.E_PlatformTypes_OPENCONFIG_HARDWARE_COMPONENT:
			if v == cType {
				s = append(s, c.GetName())
			}
		}
	}
	return s
}

// FindActiveComponentsByType finds the list of active components based on hardware type.
func FindActiveComponentsByType(t *testing.T, dut *ondatra.DUTDevice, cType oc.E_PlatformTypes_OPENCONFIG_HARDWARE_COMPONENT) []string {
	t.Helper()
	components := gnmi.GetAll[*oc.Component](t, dut, gnmi.OC().ComponentAny().State())
	var s []string
	for _, c := range components {
		switch v := c.GetType().(type) {
		case oc.E_PlatformTypes_OPENCONFIG_HARDWARE_COMPONENT:
			if v == cType && c.OperStatus == oc.PlatformTypes_COMPONENT_OPER_STATUS_ACTIVE {
				s = append(s, c.GetName())
			}
		}
	}
	return s
}

// FindSWComponentsByType finds the list of SW components based on a type.
func FindSWComponentsByType(t *testing.T, dut *ondatra.DUTDevice, cType oc.E_PlatformTypes_OPENCONFIG_SOFTWARE_COMPONENT) []string {
	t.Helper()
	components := fetchCachedComponents(t, dut)
	var s []string
	for _, c := range components {
		switch v := c.GetType().(type) {
		case oc.E_PlatformTypes_OPENCONFIG_SOFTWARE_COMPONENT:
			if v == cType {
				s = append(s, c.GetName())
			}
		}
	}
	return s
}

// FindMatchingStrings filters out the components list based on regex pattern.
func FindMatchingStrings(components []string, r *regexp.Regexp) []string {
	var s []string
	for _, c := range components {
		if r.MatchString(c) {
			s = append(s, c)
		}
	}
	return s
}

// AwaitSwitchoverReady waits for the controller card to be ready for switchover.
// It blocks on the OpenConfig switchover-ready leaf for up to timeout, then for
// ARISTA DUTs additionally polls "show redundancy status" CLI to confirm the
// "Ready for switchover" string appears.
//
// On ARISTA platforms with fast supervisor boot (e.g. Camp), the gNMI path can
// become true 2-3 minutes before the device is truly ready to accept
// SwitchControlProcessor; the CLI gate closes that gap and prevents a transient
// gNOI Unavailable rejection. The CLI gate uses a separate 5-minute bound and
// is non-fatal on timeout (proceeds and lets SwitchControlProcessor fail
// naturally if the device is still not ready).
func AwaitSwitchoverReady(t *testing.T, dut *ondatra.DUTDevice, controller string, timeout time.Duration) {
	t.Helper()
	switchoverReady := gnmi.OC().Component(controller).SwitchoverReady()
	gnmi.Await(t, dut, switchoverReady.State(), timeout, true)
	t.Logf("SwitchoverReady: %v", gnmi.Get(t, dut, switchoverReady.State()))
	if dut.Vendor() == ondatra.ARISTA {
		awaitARISTACLISwitchoverReady(t, dut, 5*time.Minute)
	}
}

// awaitARISTACLISwitchoverReady polls "show redundancy status" until the output
// contains "Ready for switchover" or the timeout expires. Non-fatal on timeout.
func awaitARISTACLISwitchoverReady(t *testing.T, dut *ondatra.DUTDevice, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		result := dut.CLI().RunResult(t, "show redundancy status")
		if result.Error() == "" && strings.Contains(result.Output(), "Ready for switchover") {
			t.Log("CLI confirms: Ready for switchover")
			return
		}
		if time.Now().After(deadline) {
			t.Logf("CLI switchover-ready not seen within %v (proceeding anyway)", timeout)
			return
		}
		if result.Error() != "" {
			t.Logf("CLI 'show redundancy status' error: %s; retrying in 30s", result.Error())
		} else {
			t.Log("CLI: not yet ready for switchover, retrying in 30s")
		}
		time.Sleep(30 * time.Second)
	}
}

// GetSubcomponentPath creates a gNMI path based on the component name.
// If useNameOnly is true, returns a path to the specified name instead of a full subcomponent path.
func GetSubcomponentPath(name string, useNameOnly bool) *tpb.Path {
	if useNameOnly {
		return &tpb.Path{
			Elem: []*tpb.PathElem{{Name: name}},
		}
	}
	return &tpb.Path{
		Origin: "openconfig",
		Elem: []*tpb.PathElem{
			{Name: "components"},
			{Name: "component", Key: map[string]string{"name": name}},
		},
	}
}

// Y provides the ygnmi based components helper.  A ygnmi.Client is tied to a specific
// DUT.
type Y struct {
	*ygnmi.Client
	// typeCache caches the results of FindByType.
	// Key: oc.Component_Type_Union, Value: []string (component names).
	typeCache map[oc.Component_Type_Union][]string
}

// New creates a new ygnmi based helper from a *ondatra.DUTDevice.
func New(t testing.TB, dut *ondatra.DUTDevice) Y {
	gnmic := dut.RawAPIs().GNMI(t)
	yc, err := ygnmi.NewClient(gnmic)
	if err != nil {
		t.Fatalf("Could not create ygnmi.Client: %v", err)
	}
	return Y{
		Client:    yc,
		typeCache: make(map[oc.Component_Type_Union][]string),
	}
}

// FindByType finds the list of components based on component type.
func (y Y) FindByType(ctx context.Context, want oc.Component_Type_Union) ([]string, error) {
	if y.typeCache == nil {
		y.typeCache = make(map[oc.Component_Type_Union][]string)
	}
	if cached, ok := y.typeCache[want]; ok {
		return cached, nil
	}

	var names []string

	anyTypePath := ocpath.Root().ComponentAny().Type()
	values, err := ygnmi.LookupAll(ctx, y.Client, anyTypePath.State())
	if err != nil {
		return nil, err
	}

	for _, value := range values {
		if got, ok := value.Val(); ok {
			if got != want {
				continue
			}
			name := value.Path.GetElem()[1].GetKey()["name"]
			names = append(names, name)
		}
	}

	if len(names) < 1 {
		return nil, fmt.Errorf("none of the %d components match %v", len(values), want)
	}

	y.typeCache[want] = names
	return names, nil
}

// FindStandbyControllerCard gets a list of two components and finds out the active and standby controller_cards.
func FindStandbyControllerCard(t *testing.T, dut *ondatra.DUTDevice, supervisors []string) (string, string) {
	var activeCC, standbyCC string
	for _, supervisor := range supervisors {
		watch := gnmi.Watch(t, dut, gnmi.OC().Component(supervisor).RedundantRole().State(), 10*time.Minute, func(val *ygnmi.Value[oc.E_Platform_ComponentRedundantRole]) bool {
			return val.IsPresent()
		})
		if val, ok := watch.Await(t); !ok {
			t.Fatalf("DUT did not reach target state within %v: got %v", 10*time.Minute, val)
		}
		role := gnmi.Get(t, dut, gnmi.OC().Component(supervisor).RedundantRole().State())
		t.Logf("Component(supervisor).RedundantRole().Get(t): %v, Role: %v", supervisor, role)
		if role == standbyController {
			standbyCC = supervisor
		} else if role == activeController {
			activeCC = supervisor
		} else {
			t.Fatalf("Expected controller %s to be active or standby, got %v", supervisor, role)
		}
	}
	if standbyCC == "" || activeCC == "" {
		t.Fatalf("Expected non-empty activeCC and standbyCC, got activeCC: %v, standbyCC: %v", activeCC, standbyCC)
	}
	t.Logf("Detected activeCC: %v, standbyCC: %v", activeCC, standbyCC)

	return standbyCC, activeCC
}

// OpticalChannelComponentFromPort finds the optical channel component for a port.
func OpticalChannelComponentFromPort(t *testing.T, dut *ondatra.DUTDevice, p *ondatra.Port) string {
	t.Helper()

	if deviations.MissingPortToOpticalChannelMapping(dut) {
		switch dut.Vendor() {
		case ondatra.ARISTA:
			transceiverName := gnmi.Get(t, dut, gnmi.OC().Interface(p.Name()).Transceiver().State())
			return fmt.Sprintf("%s-Optical0", transceiverName)
		default:
			t.Fatal("Manual Optical channel name required when deviation missing_port_to_optical_channel_component_mapping applied.")
		}
	}
	transceiverName := gnmi.Get(t, dut, gnmi.OC().Interface(p.Name()).Transceiver().State())
	if transceiverName == "" {
		t.Fatalf("Associated Transceiver for Interface (%v) not found!", p.Name())
	}
	opticalChannelName := gnmi.Get(t, dut, gnmi.OC().Component(transceiverName).Transceiver().Channel(0).AssociatedOpticalChannel().State())
	if opticalChannelName == "" {
		t.Fatalf("Associated Optical Channel for Transceiver (%v) not found!", transceiverName)
	}
	return opticalChannelName
}
