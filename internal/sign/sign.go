// Package sign holds a store's signing key: an OpenPGP key for apt, pacman
// and rpm, an RSA key for apk. Either comes from a key file, or from a seed
// every instance of one store shares, so they all sign with the same key
// and nobody generates or keeps one.
package sign

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// ErrKey is a key that can't be read or made.
var ErrKey = errors.New("signing key")

// keyCreated is the creation time of every key made from a seed: a fixed
// one, so the same seed makes the same key.
var keyCreated = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// stream is a deterministic byte stream: SHA-256 over the seed, a label
// and a counter.
type stream struct {
	seed  []byte
	label string
	n     uint64
	buf   []byte
}

func newStream(seed []byte, label string) *stream { return &stream{seed: seed, label: label} }

func (s *stream) Read(p []byte) (int, error) {
	for i := range p {
		if len(s.buf) == 0 {
			h := sha256.New()
			h.Write(s.seed)
			h.Write([]byte{0})
			h.Write([]byte(s.label))

			var c [8]byte
			binary.BigEndian.PutUint64(c[:], s.n)
			h.Write(c[:])

			s.n++
			s.buf = h.Sum(nil)
		}

		p[i], s.buf = s.buf[0], s.buf[1:]
	}

	return len(p), nil
}

// OpenPGP signs with an OpenPGP key.
type OpenPGP struct {
	entity *openpgp.Entity
	config *packet.Config
}

func pgpConfig(now func() time.Time) *packet.Config {
	off := false

	return &packet.Config{
		DefaultHash: crypto.SHA512, Time: now, NonDeterministicSignaturesViaNotation: &off,
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
	}
}

// OpenPGPFromSeed is the Ed25519 key a seed makes, the same every time.
func OpenPGPFromSeed(seed []byte, name string) (*OpenPGP, error) {
	if len(seed) < 16 {
		return nil, fmt.Errorf("%w: a seed of at least 16 bytes", ErrKey)
	}

	c := pgpConfig(func() time.Time { return keyCreated })
	c.Rand = newStream(seed, "openpgp")

	e, err := openpgp.NewEntity(name, "", "", c)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	return &OpenPGP{entity: e, config: pgpConfig(time.Now)}, nil
}

// OpenPGPFromArmored reads an armored secret key, unencrypted.
func OpenPGPFromArmored(r io.Reader) (*OpenPGP, error) {
	list, err := openpgp.ReadArmoredKeyRing(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	for _, e := range list {
		if e.PrivateKey != nil && !e.PrivateKey.Encrypted {
			return &OpenPGP{entity: e, config: &packet.Config{DefaultHash: crypto.SHA512}}, nil
		}
	}

	return nil, fmt.Errorf("%w: no unencrypted secret key in the file", ErrKey)
}

// Fingerprint is the key's fingerprint, in hex.
func (o *OpenPGP) Fingerprint() string {
	return fmt.Sprintf("%X", o.entity.PrimaryKey.Fingerprint)
}

// PublicArmored is the public key, armored.
func (o *OpenPGP) PublicArmored() ([]byte, error) {
	var b bytes.Buffer

	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	if err := o.entity.Serialize(w); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	b.WriteByte('\n')

	return b.Bytes(), nil
}

// ClearSign is data in a cleartext signature, as InRelease holds it.
func (o *OpenPGP) ClearSign(data []byte) ([]byte, error) {
	var b bytes.Buffer

	w, err := clearsign.Encode(&b, o.entity.PrivateKey, o.config)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	b.WriteByte('\n')

	return b.Bytes(), nil
}

// Detach is a detached signature of data, armored or binary.
func (o *OpenPGP) Detach(data []byte, armored bool) ([]byte, error) {
	var b bytes.Buffer

	sign := openpgp.DetachSign
	if armored {
		sign = openpgp.ArmoredDetachSign
	}

	if err := sign(&b, o.entity, bytes.NewReader(data), o.config); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	if armored {
		b.WriteByte('\n')
	}

	return b.Bytes(), nil
}

// RSA signs with an RSA key, as apk's indexes are signed.
type RSA struct {
	Key *rsa.PrivateKey
}

// rsaBits is the size of a key made from a seed.
const rsaBits = 4096

// RSAFromSeed is the RSA key a seed makes, the same every time: its primes
// are found from a stream over the seed, as Go's own generator takes no
// reader it is bound to.
func RSAFromSeed(seed []byte) (*RSA, error) {
	if len(seed) < 16 {
		return nil, fmt.Errorf("%w: a seed of at least 16 bytes", ErrKey)
	}

	s := newStream(seed, "rsa")
	e := big.NewInt(65537)
	one := big.NewInt(1)

	for {
		p, q := prime(s, rsaBits/2), prime(s, rsaBits/2)
		if p.Cmp(q) == 0 {
			continue
		}

		n := new(big.Int).Mul(p, q)
		if n.BitLen() != rsaBits {
			continue
		}

		phi := new(big.Int).Mul(new(big.Int).Sub(p, one), new(big.Int).Sub(q, one))

		d := new(big.Int).ModInverse(e, phi)
		if d == nil {
			continue
		}

		k := &rsa.PrivateKey{N: n, E: int(e.Int64()), D: d, Primes: []*big.Int{p, q}}
		if err := k.Validate(); err != nil {
			continue
		}

		k.Precompute()

		return &RSA{Key: k}, nil
	}
}

// prime is the first probable prime at or after a random odd number of
// bits whose top two bits are set.
func prime(r io.Reader, bits int) *big.Int {
	buf := make([]byte, bits/8)
	_, _ = io.ReadFull(r, buf) //nolint:errcheck // the stream never fails

	buf[0] |= 0xc0
	buf[len(buf)-1] |= 1

	p := new(big.Int).SetBytes(buf)
	two := big.NewInt(2)

	for !p.ProbablyPrime(32) {
		p.Add(p, two)
	}

	return p
}

// RSAFromPEM reads a PEM RSA private key, PKCS #1 or PKCS #8.
func RSAFromPEM(data []byte) (*RSA, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: no PEM block", ErrKey)
	}

	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &RSA{Key: k}, nil
	}

	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	r, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: not an RSA key", ErrKey)
	}

	return &RSA{Key: r}, nil
}

// PublicPEM is the public key as apk keeps it in /etc/apk/keys.
func (r *RSA) PublicPEM() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(&r.Key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// Sign is a PKCS #1 v1.5 signature of data under hash.
func (r *RSA) Sign(data []byte, hash crypto.Hash) ([]byte, error) {
	h := hash.New()
	h.Write(data)

	sig, err := rsa.SignPKCS1v15(nil, r.Key, hash, h.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	return sig, nil
}
