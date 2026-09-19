package seal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	hraft "github.com/hashicorp/raft"
)

func logFilePurpose(log *hraft.Log) string {
	return fmt.Sprintf("raft-log-file:%d:%d:%d", log.Index, log.Term, log.Type)
}

// SealLogFile binds the assigned replay index, term and type after Raft
// assigns them. Command data inside remains independently sealed for peers.
func (r *Keyring) SealLogFile(log *hraft.Log) (*hraft.Log, error) {
	if log.Type == hraft.LogCommand {
		plain, err := r.Open(CommandPurpose, log.Data)
		clear(plain)
		if err != nil {
			return nil, err
		}
	}
	body := binary.BigEndian.AppendUint64(nil, uint64(len(log.Data)))
	body = append(body, log.Data...)
	body = append(body, log.Extensions...)
	defer clear(body)
	data, err := r.Seal(logFilePurpose(log), body)
	if err != nil {
		return nil, err
	}
	sealed := *log
	sealed.Data = data
	sealed.Extensions = nil
	return &sealed, nil
}

func (r *Keyring) OpenLogFile(log *hraft.Log) (*hraft.Log, error) {
	if len(log.Extensions) != 0 {
		return nil, errors.New("seal: unauthenticated log extensions")
	}
	body, err := r.Open(logFilePurpose(log), log.Data)
	if err != nil {
		return nil, err
	}
	defer clear(body)
	if len(body) < 8 {
		return nil, errors.New("seal: malformed log envelope")
	}
	size := binary.BigEndian.Uint64(body[:8])
	if size > uint64(len(body)-8) {
		return nil, errors.New("seal: malformed log data length")
	}
	opened := *log
	opened.Data = bytes.Clone(body[8 : 8+size])
	opened.Extensions = bytes.Clone(body[8+size:])
	if opened.Type == hraft.LogCommand {
		plain, err := r.Open(CommandPurpose, opened.Data)
		clear(plain)
		if err != nil {
			return nil, err
		}
	}
	return &opened, nil
}
