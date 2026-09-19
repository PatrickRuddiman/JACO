// Package seal encrypts application payloads with independently provisioned
// wrapping keys. Envelopes carry a random data key, never the wrapping key.
package seal

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

const magic = "JACOENC\x01"

var (
	ErrUnsealed = errors.New("seal: plaintext state requires explicit offline migration")
	keyID       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Keyring is immutable after construction. The active key seals new payloads;
// retained keys open historical payloads during rotation and recovery.
type Keyring struct {
	active   string
	keys     map[string]cipher.AEAD
	proofKey [32]byte
}

func New(active string, keys map[string][]byte) (*Keyring, error) {
	if !keyID.MatchString(active) || len(keys[active]) != 32 {
		return nil, errors.New("seal: active key must name a 256-bit key")
	}
	r := &Keyring{active: active, keys: make(map[string]cipher.AEAD, len(keys))}
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	material := []byte("JACO keyring possession v1\x00")
	material = binary.BigEndian.AppendUint16(material, uint16(len(active)))
	material = append(material, active...)
	defer func() { clear(material) }()
	for _, id := range ids {
		key := keys[id]
		if !keyID.MatchString(id) || len(key) != 32 {
			return nil, errors.New("seal: key IDs must be 1-64 alphanumeric/hyphen/underscore characters and keys exactly 32 bytes")
		}
		aead, err := newAEAD(key)
		if err != nil {
			return nil, err
		}
		r.keys[id] = aead
		material = binary.BigEndian.AppendUint16(material, uint16(len(id)))
		material = append(material, id...)
		material = append(material, key...)
	}
	r.proofKey = sha256.Sum256(material)
	return r, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// Seal binds the payload to a purpose (command, snapshot, or named cache
// entry) so an authenticated envelope cannot be moved between those uses.
func (r *Keyring) Seal(purpose string, plain []byte) ([]byte, error) {
	if r == nil || r.keys[r.active] == nil || purpose == "" {
		return nil, errors.New("seal: keyring and purpose are required")
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("seal: generate data key: %w", err)
	}
	defer clear(dek)
	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	header := binary.BigEndian.AppendUint16([]byte(magic), uint16(len(r.active)))
	header = append(header, r.active...)
	aad := append(bytes.Clone(header), []byte("\x00"+purpose)...)
	wrapped := r.keys[r.active].Seal(nil, nil, dek, aad)
	payload := dataAEAD.Seal(nil, nil, plain, append(aad, wrapped...))
	return append(append(header, wrapped...), payload...), nil
}

func (r *Keyring) Open(purpose string, envelope []byte) ([]byte, error) {
	if r == nil || purpose == "" {
		return nil, errors.New("seal: keyring and purpose are required")
	}
	if !bytes.HasPrefix(envelope, []byte(magic[:len(magic)-1])) {
		return nil, ErrUnsealed
	}
	if len(envelope) < len(magic)+2 || string(envelope[:len(magic)]) != magic {
		return nil, errors.New("seal: invalid or unsupported envelope version")
	}
	idLen := int(binary.BigEndian.Uint16(envelope[len(magic):]))
	headerLen := len(magic) + 2 + idLen
	if idLen < 1 || idLen > 64 || headerLen > len(envelope) {
		return nil, errors.New("seal: invalid envelope header")
	}
	wrapping := r.keys[string(envelope[len(magic)+2:headerLen])]
	if wrapping == nil {
		return nil, errors.New("seal: envelope wrapping key is unavailable")
	}
	wrappedLen := 32 + wrapping.Overhead()
	if len(envelope) < headerLen+wrappedLen+wrapping.Overhead() {
		return nil, errors.New("seal: truncated envelope")
	}
	aad := append(bytes.Clone(envelope[:headerLen]), []byte("\x00"+purpose)...)
	wrapped := envelope[headerLen : headerLen+wrappedLen]
	dek, err := wrapping.Open(nil, nil, wrapped, aad)
	if err != nil {
		return nil, fmt.Errorf("seal: authenticate data key: %w", err)
	}
	defer clear(dek)
	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	plain, err := dataAEAD.Open(nil, nil, envelope[headerLen+wrappedLen:], append(aad, wrapped...))
	if err != nil {
		return nil, fmt.Errorf("seal: authenticate payload: %w", err)
	}
	return plain, nil
}
