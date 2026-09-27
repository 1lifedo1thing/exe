package peer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

// The address is what a Solana wallet would show for the key, so the
// vectors are Solana's own: the system program is 32 zero bytes, the
// token program's id is known by both its bytes and its name.
func TestBase58(t *testing.T) {
	seq := make([]byte, 32)
	for i := range seq {
		seq[i] = byte(i)
	}
	for _, c := range []struct{ hex, want string }{
		{hex.EncodeToString(make([]byte, 32)), "11111111111111111111111111111111"},
		{"06ddf6e1d765a193d9cbe146ceeb79ac1cb485ed5f5b37913a8cf5857eff00a9", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
		{hex.EncodeToString(seq), "1thX6LZfHDZZKUs92febYZhYRcXddmzfzF2NvTkPNE"},
		{"000001" + hex.EncodeToString(bytes.Repeat([]byte{0xff}, 29)), "11QF7N3ErceFxTPNH9CGKqHisFCRSeGDL8Bb4wTGTp"},
		{"", ""},
	} {
		b, err := hex.DecodeString(c.hex)
		if err != nil {
			t.Fatal(err)
		}
		if got := Base58(b); got != c.want {
			t.Errorf("Base58(%s) = %q, want %q", c.hex, got, c.want)
		}
	}
}

func TestIdentityAddress(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	id := newIdentity(priv, pub)
	if got, want := id.Address(), Base58(pub); got != want {
		t.Errorf("Address() = %q, want the key in base58, %q", got, want)
	}
	if n := len(id.Address()); n < 32 || n > 44 {
		t.Errorf("Address() is %d characters, a Solana address is 32 to 44", n)
	}
}
