package truenas

import (
	"encoding/json"
	"testing"
)

// TestNVMeAssociationsDecodeTheApplianceShape pins the row shape the nvmet
// association services really return. They are datastores with
// datastore_extend_fk, so a foreign key comes back under its column name minus
// "_id" with the referenced row nested in it (middlewared
// plugins/datastore/read.py _serialize_row; NVMetPortSubsysEntry,
// NVMetHostSubsysEntry and NVMetNamespaceEntry, TS-25.10.6). Decoding only the
// *_id spelling yields zero ids, and the driver then never recognises a
// binding it already made (azrtydxb/kuvryn-ai#122).
func TestNVMeAssociationsDecodeTheApplianceShape(t *testing.T) {
	var ps []NVMePortSubsys
	if err := json.Unmarshal([]byte(`[{"id": 41,
		"port": {"id": 3, "index": 1, "addr_trtype": "TCP", "addr_traddr": "10.0.0.5", "addr_trsvcid": 4420, "enabled": true},
		"subsys": {"id": 156, "name": "csi-pvc-7cad3d58", "subnqn": "nqn.x:csi-pvc-7cad3d58", "serial": "abc", "allow_any_host": false}}]`), &ps); err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].ID != 41 || ps[0].PortID.ID != 3 || ps[0].SubsysID.ID != 156 {
		t.Fatalf("port_subsys decoded as %+v, want id=41 port=3 subsys=156", ps)
	}

	var hs NVMeHostSubsys
	if err := json.Unmarshal([]byte(`{"id": 7,
		"host": {"id": 2, "hostnqn": "nqn.2014-08.org.nvmexpress:uuid:n1"},
		"subsys": {"id": 156, "name": "csi-pvc-7cad3d58"}}`), &hs); err != nil {
		t.Fatal(err)
	}
	if hs.ID != 7 || hs.HostID.ID != 2 || hs.SubsysID.ID != 156 {
		t.Fatalf("host_subsys decoded as %+v, want id=7 host=2 subsys=156", hs)
	}

	var ns NVMeNamespace
	if err := json.Unmarshal([]byte(`{"id": 9, "nsid": 1, "device_type": "ZVOL",
		"device_path": "zvol/Pool0/k8s/pvc-x", "enabled": false,
		"subsys": {"id": 156, "name": "csi-pvc-x"}}`), &ns); err != nil {
		t.Fatal(err)
	}
	if ns.ID != 9 || ns.SubsysID.ID != 156 || ns.DevicePath != "zvol/Pool0/k8s/pvc-x" || ns.Serving() {
		t.Fatalf("namespace decoded as %+v, want id=9 subsys=156 and not serving", ns)
	}

	// The *_id spelling (plain id or nested object) still decodes.
	var flat NVMePortSubsys
	if err := json.Unmarshal([]byte(`{"id": 1, "port_id": 3, "subsys_id": {"id": 156}}`), &flat); err != nil {
		t.Fatal(err)
	}
	if flat.PortID.ID != 3 || flat.SubsysID.ID != 156 {
		t.Fatalf("*_id spelling decoded as %+v", flat)
	}
}
