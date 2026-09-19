package seal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"google.golang.org/protobuf/proto"

	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func (r *Keyring) joinProof(purpose string, parts ...[]byte) ([]byte, error) {
	if r == nil || r.keys[r.active] == nil {
		return nil, errors.New("seal: join keyring is required")
	}
	mac := hmac.New(sha256.New, r.proofKey[:])
	mac.Write([]byte("JACO join proof v1\x00" + purpose))
	for _, part := range parts {
		mac.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part))))
		mac.Write(part)
	}
	return mac.Sum(nil), nil
}

func joinRequestBytes(req *pb.NodeJoinRequest) ([]byte, error) {
	if req == nil {
		return nil, errors.New("seal: join request is required")
	}
	unsigned := proto.Clone(req).(*pb.NodeJoinRequest)
	unsigned.StateKeyProof = nil
	return proto.MarshalOptions{Deterministic: true}.Marshal(unsigned)
}

func (r *Keyring) SignJoinRequest(req *pb.NodeJoinRequest) error {
	data, err := joinRequestBytes(req)
	if err != nil {
		return err
	}
	defer clear(data)
	proof, err := r.joinProof("request", data)
	if err != nil {
		return err
	}
	req.StateKeyProof = proof
	return nil
}

func (r *Keyring) VerifyJoinRequest(req *pb.NodeJoinRequest) error {
	data, err := joinRequestBytes(req)
	if err != nil {
		return err
	}
	defer clear(data)
	expected, err := r.joinProof("request", data)
	if err != nil {
		return err
	}
	if !hmac.Equal(expected, req.GetStateKeyProof()) {
		return errors.New("seal: join state-key proof mismatch")
	}
	return nil
}

func (r *Keyring) joinResponseProof(req *pb.NodeJoinRequest, resp *pb.NodeJoinResponse) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("seal: join response is required")
	}
	if err := r.VerifyJoinRequest(req); err != nil {
		return nil, err
	}
	request, err := joinRequestBytes(req)
	if err != nil {
		return nil, err
	}
	defer clear(request)
	unsigned := proto.Clone(resp).(*pb.NodeJoinResponse)
	unsigned.StateKeyProof = nil
	response, err := proto.MarshalOptions{Deterministic: true}.Marshal(unsigned)
	if err != nil {
		return nil, err
	}
	return r.joinProof("response", request, response)
}

func (r *Keyring) SignJoinResponse(req *pb.NodeJoinRequest, resp *pb.NodeJoinResponse) error {
	proof, err := r.joinResponseProof(req, resp)
	if err != nil {
		return err
	}
	resp.StateKeyProof = proof
	return nil
}

func (r *Keyring) VerifyJoinResponse(req *pb.NodeJoinRequest, resp *pb.NodeJoinResponse) error {
	expected, err := r.joinResponseProof(req, resp)
	if err != nil {
		return err
	}
	if !hmac.Equal(expected, resp.GetStateKeyProof()) {
		return errors.New("seal: join response state-key proof mismatch")
	}
	return nil
}
