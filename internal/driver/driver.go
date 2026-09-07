// Package driver holds the identity of this CSI driver.
package driver

// DriverName is the CSI plugin name. It is immutable: it is recorded in every
// PersistentVolume this driver provisions, so changing it orphans existing volumes.
const DriverName = "csi.truenas.watteel.com"

// Version is the build version, set via -ldflags at release time.
var Version = "dev"
