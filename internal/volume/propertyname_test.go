package volume

import (
	"regexp"
	"testing"
)

// zfsUserProperty is what zfs will actually accept after the namespace: only
// lowercase letters, digits and ":-._".
var zfsUserProperty = regexp.MustCompile(`^[a-z0-9.]+:[a-z0-9:._-]+$`)

// TestEveryPropertyNameIsLegalForZFS fails if any driver property is spelled in
// a way zfs rejects.
//
// This exists because two of them were, and nothing caught it: the fake stores
// whatever key it is handed, so every unit test passed while the appliance
// answered
//
//	cannot set property for '<dataset>': invalid property 'io.truenas.csi:deletedAt'
//
// The consequence was not a loud failure either. Retiring a volume stamped
// nothing, the reaper refuses anything it cannot date, and a delete-protected
// volume would have sat in the graveyard for ever — a feature whose whole
// purpose is bounded retention, silently unbounded.
func TestEveryPropertyNameIsLegalForZFS(t *testing.T) {
	for _, name := range []string{
		OwnerProperty, ProtocolProperty,
		GraveyardProperty, DeletedAtProperty, RetiredFromProperty,
	} {
		t.Run(name, func(t *testing.T) {
			if !zfsUserProperty.MatchString(name) {
				t.Errorf("%q is not a legal ZFS user property name.\n"+
					"zfs accepts only lowercase letters, digits and \":-._\" after the "+
					"namespace, and refuses anything else with \"invalid property\". The "+
					"fake accepts any key, so this cannot be caught by exercising it.", name)
			}
		})
	}
}
