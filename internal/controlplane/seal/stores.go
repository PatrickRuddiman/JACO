package seal

import (
	"bytes"
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"slices"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
)

const FormatMarkerKey = "jaco:encrypted-state:v1"

var ErrLegacyStore = errors.New("seal: historical database lacks encryption provenance; explicit offline fresh-copy migration is required")

func (r *Keyring) formatMarkerValue() []byte {
	return append([]byte("encrypted-state-v1\x00"), r.proofKey[:]...)
}

// MarkFreshStore records that a newly created database has never held
// plaintext application records. Never use this to upgrade a database in
// place: logically deleted Bolt pages can still contain historical secrets.
func (r *Keyring) MarkFreshStore(store hraft.StableStore) error {
	if r == nil {
		return errors.New("seal: keyring is required")
	}
	value := r.formatMarkerValue()
	defer clear(value)
	data, err := r.Seal("raft-format", value)
	if err != nil {
		return err
	}
	return store.Set([]byte(FormatMarkerKey), data)
}

func (r *Keyring) CheckStore(store hraft.StableStore) error {
	if r == nil {
		return errors.New("seal: keyring is required")
	}
	data, err := store.Get([]byte(FormatMarkerKey))
	if errors.Is(err, boltdb.ErrKeyNotFound) || (err == nil && len(data) == 0) {
		return ErrLegacyStore
	}
	if err != nil {
		return fmt.Errorf("seal: read database format marker: %w", err)
	}
	plain, err := r.Open("raft-format", data)
	if err != nil {
		return fmt.Errorf("seal: authenticate database format marker: %w", err)
	}
	defer clear(plain)
	expected := r.formatMarkerValue()
	defer clear(expected)
	if !hmac.Equal(plain, expected) {
		return errors.New("seal: database keyring differs; use coordinated offline fresh-copy rotation")
	}
	return nil
}

type protectedLogs struct {
	hraft.LogStore
	keys *Keyring
}

// WrapLogs authenticates replicated commands before any durable write. Reads
// retain ciphertext: Raft uses GetLog to send records to other replicas.
func WrapLogs(store hraft.LogStore, keys *Keyring) (hraft.LogStore, error) {
	if store == nil || keys == nil {
		return nil, errors.New("seal: log store and keyring are required")
	}
	protected := &protectedLogs{LogStore: store, keys: keys}
	first, err := store.FirstIndex()
	if err != nil {
		return nil, err
	}
	last, err := store.LastIndex()
	if err != nil {
		return nil, err
	}
	for index := first; index != 0 && index <= last; index++ {
		var log hraft.Log
		if err := protected.GetLog(index, &log); err != nil && !errors.Is(err, hraft.ErrLogNotFound) {
			return nil, err
		}
	}
	return protected, nil
}

func (s *protectedLogs) GetLog(index uint64, log *hraft.Log) error {
	if err := s.LogStore.GetLog(index, log); err != nil {
		return err
	}
	if log.Index != index {
		return errors.New("seal: stored log index differs from its database key")
	}
	opened, err := s.keys.OpenLogFile(log)
	if err != nil {
		return fmt.Errorf("seal: log index %d: %w", index, err)
	}
	*log = *opened
	return nil
}

func (s *protectedLogs) StoreLog(log *hraft.Log) error {
	return s.StoreLogs([]*hraft.Log{log})
}

func (s *protectedLogs) StoreLogs(logs []*hraft.Log) error {
	sealed := make([]*hraft.Log, len(logs))
	for i, log := range logs {
		var err error
		sealed[i], err = s.keys.SealLogFile(log)
		if err != nil {
			return fmt.Errorf("seal: log index %d: %w", log.Index, err)
		}
	}
	return s.LogStore.StoreLogs(sealed)
}

type protectedSnapshots struct {
	hraft.SnapshotStore
	keys *Keyring
}

// WrapSnapshots prevents plaintext or unauthenticated InstallSnapshot streams
// reaching disk. Open returns ciphertext for replication and backup export.
func WrapSnapshots(store hraft.SnapshotStore, keys *Keyring) (hraft.SnapshotStore, error) {
	if store == nil || keys == nil {
		return nil, errors.New("seal: snapshot store and keyring are required")
	}
	protected := &protectedSnapshots{SnapshotStore: store, keys: keys}
	snapshots, err := store.List()
	if err != nil {
		return nil, err
	}
	for _, snapshot := range snapshots {
		_, reader, err := protected.Open(snapshot.ID)
		if err != nil {
			return nil, err
		}
		if err := reader.Close(); err != nil {
			return nil, err
		}
	}
	return protected, nil
}

func (s *protectedSnapshots) Create(version hraft.SnapshotVersion, index, term uint64, configuration hraft.Configuration, configurationIndex uint64, transport hraft.Transport) (hraft.SnapshotSink, error) {
	sink, err := s.SnapshotStore.Create(version, index, term, configuration, configurationIndex, transport)
	if err != nil {
		return nil, err
	}
	configuration.Servers = slices.Clone(configuration.Servers)
	meta := &hraft.SnapshotMeta{
		Version: version, ID: sink.ID(), Index: index, Term: term,
		Configuration: configuration, ConfigurationIndex: configurationIndex,
	}
	return &protectedSink{inner: sink, keys: s.keys, meta: meta}, nil
}

func (s *protectedSnapshots) Open(id string) (*hraft.SnapshotMeta, io.ReadCloser, error) {
	meta, reader, err := s.SnapshotStore.Open(id)
	if err != nil {
		return nil, nil, err
	}
	data, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, nil, err
	}
	data, err = s.keys.OpenSnapshotFile(meta, data)
	if err != nil {
		return nil, nil, fmt.Errorf("seal: snapshot %s: %w", id, err)
	}
	wireMeta := *meta
	wireMeta.Size = int64(len(data))
	return &wireMeta, io.NopCloser(bytes.NewReader(data)), nil
}

type protectedSink struct {
	buffer bytes.Buffer
	inner  hraft.SnapshotSink
	keys   *Keyring
	meta   *hraft.SnapshotMeta
	closed bool
	result error
}

func (s *protectedSink) ID() string { return s.inner.ID() }

func (s *protectedSink) Write(data []byte) (int, error) {
	if s.closed {
		return 0, errors.New("seal: snapshot sink is closed")
	}
	return s.buffer.Write(data)
}

func (s *protectedSink) Close() error {
	if s.closed {
		return s.result
	}
	s.closed = true
	defer clear(s.buffer.Bytes())
	data, err := s.keys.SealSnapshotFile(s.meta, s.buffer.Bytes())
	if err != nil {
		s.result = errors.Join(err, s.inner.Cancel())
		return s.result
	}
	if _, err := io.Copy(s.inner, bytes.NewReader(data)); err != nil {
		s.result = errors.Join(err, s.inner.Cancel())
		return s.result
	}
	s.result = s.inner.Close()
	return s.result
}

func (s *protectedSink) Cancel() error {
	if s.closed {
		return s.result
	}
	s.closed = true
	clear(s.buffer.Bytes())
	s.result = s.inner.Cancel()
	return s.result
}
