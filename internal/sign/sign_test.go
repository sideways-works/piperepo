package sign

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

var seed = []byte("a seed every instance of one store shares")

func TestOneSeedMakesOneOpenPGPKey(t *testing.T) {
	a, err := OpenPGPFromSeed(seed, "piperepo")
	if err != nil {
		t.Fatal(err)
	}

	b, err := OpenPGPFromSeed(seed, "piperepo")
	if err != nil {
		t.Fatal(err)
	}

	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("one seed made %s and %s", a.Fingerprint(), b.Fingerprint())
	}

	ka, _ := a.PublicArmored()
	kb, _ := b.PublicArmored()

	if !bytes.Equal(ka, kb) {
		t.Fatal("one seed's public keys differ")
	}

	other, err := OpenPGPFromSeed([]byte("another store's seed, as long"), "piperepo")
	if err != nil {
		t.Fatal(err)
	}

	if other.Fingerprint() == a.Fingerprint() {
		t.Fatal("two seeds made one key")
	}

	if _, err := OpenPGPFromSeed([]byte("short"), "x"); err == nil {
		t.Fatal("a short seed made a key")
	}
}

func TestSignaturesVerifyWithThePublicKey(t *testing.T) {
	k, err := OpenPGPFromSeed(seed, "piperepo")
	if err != nil {
		t.Fatal(err)
	}

	pub, err := k.PublicArmored()
	if err != nil {
		t.Fatal(err)
	}

	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pub))
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("Origin: piperepo\nSuite: stable\n")

	signed, err := k.ClearSign(data)
	if err != nil {
		t.Fatal(err)
	}

	block, _ := clearsign.Decode(signed)
	if block == nil || !bytes.Equal(block.Plaintext, data) {
		t.Fatalf("the clear signature holds %q", signed)
	}

	if _, err := block.VerifySignature(ring, nil); err != nil {
		t.Fatal(err)
	}

	for _, armored := range []bool{true, false} {
		sig, err := k.Detach(data, armored)
		if err != nil {
			t.Fatal(err)
		}

		check := openpgp.CheckDetachedSignature
		if armored {
			check = openpgp.CheckArmoredDetachedSignature
		}

		if _, err := check(ring, bytes.NewReader(data), bytes.NewReader(sig), nil); err != nil {
			t.Fatalf("armored %v: %v", armored, err)
		}
	}
}

func TestOneSeedMakesOneRSAKey(t *testing.T) {
	a, err := RSAFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}

	b, err := RSAFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}

	if a.Key.N.Cmp(b.Key.N) != 0 {
		t.Fatal("one seed made two RSA keys")
	}

	if a.Key.N.BitLen() != rsaBits {
		t.Fatalf("a key of %d bits", a.Key.N.BitLen())
	}

	data := []byte("an index")

	sig, err := a.Sign(data, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(data)
	if err := rsa.VerifyPKCS1v15(&a.Key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatal(err)
	}

	pub, err := a.PublicPEM()
	if err != nil || !strings.HasPrefix(string(pub), "-----BEGIN PUBLIC KEY-----") {
		t.Fatalf("the public key: %s %v", pub, err)
	}
}
