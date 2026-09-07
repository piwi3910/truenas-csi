// Package csi implements the CSI Identity, Controller and Node gRPC services.
package csi

import (
	"context"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/driver"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type identity struct {
	csipb.UnimplementedIdentityServer
	ready func() bool
}

// NewIdentity builds the Identity service. ready reports whether the driver has
// reached a usable state.
func NewIdentity(ready func() bool) csipb.IdentityServer {
	if ready == nil {
		ready = func() bool { return true }
	}
	return &identity{ready: ready}
}

func (i *identity) GetPluginInfo(context.Context, *csipb.GetPluginInfoRequest) (*csipb.GetPluginInfoResponse, error) {
	return &csipb.GetPluginInfoResponse{Name: driver.DriverName, VendorVersion: driver.Version}, nil
}

func (i *identity) GetPluginCapabilities(context.Context, *csipb.GetPluginCapabilitiesRequest) (*csipb.GetPluginCapabilitiesResponse, error) {
	svc := func(t csipb.PluginCapability_Service_Type) *csipb.PluginCapability {
		return &csipb.PluginCapability{Type: &csipb.PluginCapability_Service_{
			Service: &csipb.PluginCapability_Service{Type: t}}}
	}
	return &csipb.GetPluginCapabilitiesResponse{Capabilities: []*csipb.PluginCapability{
		svc(csipb.PluginCapability_Service_CONTROLLER_SERVICE),
		svc(csipb.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS),
		{Type: &csipb.PluginCapability_VolumeExpansion_{
			VolumeExpansion: &csipb.PluginCapability_VolumeExpansion{
				Type: csipb.PluginCapability_VolumeExpansion_ONLINE}}},
	}}, nil
}

func (i *identity) Probe(context.Context, *csipb.ProbeRequest) (*csipb.ProbeResponse, error) {
	return &csipb.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: i.ready()}}, nil
}
