package seal

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	hraft "github.com/hashicorp/raft"
)

const (
	CommandPurpose     = "raft-command"
	SnapshotPurpose    = "raft-snapshot"
	CachePurposePrefix = "ingress-cache:"
)

type protectedFSM struct {
	inner hraft.FSM
	keys  *Keyring
}

// WrapFSM keeps the existing in-memory FSM private to the process while
// commands and snapshots crossing the Raft boundary remain encrypted.
func WrapFSM(inner hraft.FSM, keys *Keyring) (hraft.FSM, error) {
	if inner == nil || keys == nil {
		return nil, errors.New("seal: FSM and keyring are required")
	}
	return &protectedFSM{inner: inner, keys: keys}, nil
}

func (f *protectedFSM) Apply(log *hraft.Log) interface{} {
	plain, err := f.keys.Open(CommandPurpose, log.Data)
	if err != nil {
		// Raft advances its applied index even when Apply returns an error.
		// Authentication failure must stop the process, never skip a command.
		panic(fmt.Errorf("seal: fatal command authentication at index %d: %w", log.Index, err))
	}
	defer clear(plain)
	decoded := *log
	decoded.Data = plain
	return f.inner.Apply(&decoded)
}

func (f *protectedFSM) Snapshot() (hraft.FSMSnapshot, error) {
	snapshot, err := f.inner.Snapshot()
	if err != nil {
		return nil, err
	}
	return &protectedSnapshot{inner: snapshot, keys: f.keys}, nil
}

func (f *protectedFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("seal: read snapshot: %w", err)
	}
	plain, err := f.keys.Open(SnapshotPurpose, data)
	if err != nil {
		return fmt.Errorf("seal: open snapshot: %w", err)
	}
	defer clear(plain)
	return f.inner.Restore(io.NopCloser(bytes.NewReader(plain)))
}

type protectedSnapshot struct {
	inner hraft.FSMSnapshot
	keys  *Keyring
}

func (s *protectedSnapshot) Persist(sink hraft.SnapshotSink) error {
	buffer := &snapshotBuffer{id: sink.ID()}
	if err := s.inner.Persist(buffer); err != nil {
		return errors.Join(err, sink.Cancel())
	}
	defer clear(buffer.Bytes())
	if buffer.cancelled {
		return errors.Join(errors.New("seal: inner snapshot was cancelled"), sink.Cancel())
	}
	data, err := s.keys.Seal(SnapshotPurpose, buffer.Bytes())
	if err != nil {
		return errors.Join(err, sink.Cancel())
	}
	if _, err := sink.Write(data); err != nil {
		return errors.Join(err, sink.Cancel())
	}
	return sink.Close()
}

func (s *protectedSnapshot) Release() { s.inner.Release() }

type snapshotBuffer struct {
	bytes.Buffer
	id        string
	cancelled bool
}

func (s *snapshotBuffer) ID() string { return s.id }
func (*snapshotBuffer) Close() error { return nil }
func (s *snapshotBuffer) Cancel() error {
	s.cancelled = true
	return nil
}
