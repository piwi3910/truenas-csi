package config

import (
	"sync"
	"testing"
)

// TestCurrentAppliesARotatedCredential covers the case that made this store
// necessary: an appliance that was unreachable while its key was rotated. The
// registry still holds the Backend it copied at startup, and the next dial must
// NOT present the superseded key — TrueNAS revokes a key presented after it has
// been replaced, which is how three keys were destroyed.
func TestCurrentAppliesARotatedCredential(t *testing.T) {
	b := Backend{Name: "nas-rotate", Username: "old", APIKey: "8-old", Pool: "Pool0"}
	t.Cleanup(func() { ForgetCredential(b.Name) })

	if got := b.Current(); got.String() != b.String() || !got.Credentials().Equal(b.Credentials()) {
		t.Fatal("with nothing recorded, Current must return the backend unchanged")
	}

	SetCredential(b.Name, Credential{Username: "new", APIKey: "9-new", InsecureSkipVerify: true})
	got := b.Current()
	if got.APIKey != "9-new" || got.Username != "new" || !got.InsecureSkipVerify {
		t.Fatalf("Current did not apply the rotated credential: %+v", got)
	}
	if got.Pool != "Pool0" || got.Name != "nas-rotate" {
		t.Fatalf("Current changed something that is not a credential: %+v", got)
	}
	if b.APIKey != "8-old" {
		t.Fatal("Current must not mutate the receiver: it is shared by value")
	}
}

// TestCurrentIsKeyedByBackendName: two appliances must never swap credentials.
func TestCurrentIsKeyedByBackendName(t *testing.T) {
	t.Cleanup(func() { ForgetCredential("nas-a"); ForgetCredential("nas-b") })
	SetCredential("nas-a", Credential{APIKey: "9-a"})

	if got := (Backend{Name: "nas-b", APIKey: "8-b"}).Current().APIKey; got != "8-b" {
		t.Fatalf("nas-b picked up nas-a's credential: %q", got)
	}
}

// TestCredentialStoreIsConcurrencySafe: the whole point of the store is that a
// reload writes it while dials read it. Run with -race.
func TestCredentialStoreIsConcurrencySafe(t *testing.T) {
	t.Cleanup(func() { ForgetCredential("nas-race") })
	b := Backend{Name: "nas-race", APIKey: "8-old"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				SetCredential(b.Name, Credential{APIKey: "9-new"})
				_ = b.Current()
			}
		}()
	}
	wg.Wait()
}

func TestCredentialEqual(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b Credential
		want bool
	}{
		{"identical", Credential{Username: "u", APIKey: "8-a"}, Credential{Username: "u", APIKey: "8-a"}, true},
		{"different key", Credential{APIKey: "8-a"}, Credential{APIKey: "9-b"}, false},
		{"different user", Credential{Username: "u"}, Credential{Username: "v"}, false},
		{"different ca", Credential{CACert: []byte("a")}, Credential{CACert: []byte("b")}, false},
		{"same ca", Credential{CACert: []byte("a")}, Credential{CACert: []byte("a")}, true},
		{"verification flipped", Credential{}, Credential{InsecureSkipVerify: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Equal(tc.b); got != tc.want {
				t.Fatalf("Equal = %v, want %v", got, tc.want)
			}
		})
	}
}
