package derive

import (
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum-optimism/optimism/op-service/eth"
)

var (
	SubmitEventABI      = "TransactionSubmitted(address,address,uint256,bytes)"
	SubmitEventABIHash  = crypto.Keccak256Hash([]byte(SubmitEventABI))
	SubmitEventVersion0 = common.Hash{}
)

// UnmarshalSubmitsLogEvent decodes an EVM log entry emitted by the Submits contract into typed Submits data.
//
// parse log data for:
//
//	event TransactionSubmitsed(
//	    address indexed from,
//	    address indexed to,
//	    uint256 indexed version,
//	    bytes opaqueData
//	);
//
// Additionally, the event log-index and
func UnmarshalSubmitsLogEvent(ev *types.Log) (*types.SubmitTx, error) {
	if len(ev.Topics) != 4 {
		return nil, fmt.Errorf("expected 4 event topics (event identity, indexed from, indexed to, indexed version), got %d", len(ev.Topics))
	}
	if ev.Topics[0] != SubmitEventABIHash {
		return nil, fmt.Errorf("invalid Submits event selector: %s, expected %s", ev.Topics[0], SubmitEventABIHash)
	}
	if len(ev.Data) < 64 {
		return nil, fmt.Errorf("incomplate opaqueData slice header (%d bytes): %x", len(ev.Data), ev.Data)
	}
	//if len(ev.Data)%32 != 0 {
	//	return nil, fmt.Errorf("expected log data to be multiple of 32 bytes: got %d bytes", len(ev.Data))
	//}

	// indexed 0
	from := common.BytesToAddress(ev.Topics[1][12:])
	// indexed 1
	to := common.BytesToAddress(ev.Topics[2][12:])
	// indexed 2
	version := ev.Topics[3]
	// unindexed data
	// Solidity serializes the event's Data field as follows:
	// abi.encode(abi.encodPacked(uint256 mint, uint256 value, uint64 gasLimit, uint8 isCreation, bytes data))
	// Thus the first 32 bytes of the Data will give us the offset of the opaqueData,
	// which should always be 0x20.
	var opaqueContentOffset uint256.Int
	opaqueContentOffset.SetBytes(ev.Data[0:32])
	if !opaqueContentOffset.IsUint64() || opaqueContentOffset.Uint64() != 32 {
		return nil, fmt.Errorf("invalid opaqueData slice header offset: %d", opaqueContentOffset.Uint64())
	}
	// The next 32 bytes indicate the length of the opaqueData content.
	var opaqueContentLength uint256.Int
	opaqueContentLength.SetBytes(ev.Data[32:64])
	// Make sure the length is an uint64, it's not larger than the remaining data, and the log is using minimal padding (i.e. can't add 32 bytes without exceeding data)
	if !opaqueContentLength.IsUint64() || opaqueContentLength.Uint64() > uint64(len(ev.Data)-64) || opaqueContentLength.Uint64()+32 <= uint64(len(ev.Data)-64) {
		return nil, fmt.Errorf("invalid opaqueData slice header length: %d", opaqueContentLength.Uint64())
	}
	// The remaining data is the opaqueData which is tightly packed
	// and then padded to 32 bytes by the EVM.
	opaqueData := ev.Data[64 : 64+opaqueContentLength.Uint64()]

	var dep types.SubmitTx

	dep.SourceHash = ev.TxHash
	dep.From = from
	dep.IsSystemTransaction = false

	var err error
	switch version {
	case SubmitEventVersion0:
		err = unmarshalSubmitsVersion0(&dep, to, opaqueData)
	default:
		return nil, fmt.Errorf("invalid Submits version, got %s", version)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to decode Submits (version %s): %w", version, err)
	}
	return &dep, nil
}

func unmarshalSubmitsVersion0(dep *types.SubmitTx, to common.Address, opaqueData []byte) error {
	if len(opaqueData) < 32+32+8+1 {
		return fmt.Errorf("unexpected opaqueData length: %d", len(opaqueData))
	}
	offset := uint64(0)

	offset += 32

	// uint256 value
	dep.Value = new(big.Int).SetBytes(opaqueData[offset : offset+32])
	offset += 32

	// uint64 gas
	gas := new(big.Int).SetBytes(opaqueData[offset : offset+8])
	if !gas.IsUint64() {
		return fmt.Errorf("bad gas value: %x", opaqueData[offset:offset+8])
	}
	dep.Gas = gas.Uint64()
	offset += 8

	// uint8 isCreation
	// isCreation: If the boolean byte is 1 then dep.To will stay nil,
	// and it will create a contract using L2 account nonce to determine the created address.
	if opaqueData[offset] == 0 {
		dep.To = &to
	}
	offset += 1

	// The remainder of the opaqueData is the transaction data (without length prefix).
	// The data may be padded to a multiple of 32 bytes
	txDataLen := uint64(len(opaqueData)) - offset

	// remaining bytes fill the data
	dep.Data = opaqueData[offset : offset+txDataLen]

	return nil
}

// MarshalSubmitsLogEvent returns an EVM log entry that encodes a TransactionSubmitsed event from the Submits contract.
// This is the reverse of the Submits transaction derivation.
func MarshalSubmitsLogEvent(SubmitsContractAddr common.Address, Submits *types.DepositTx) (*types.Log, error) {
	toBytes := common.Hash{}
	if Submits.To != nil {
		toBytes = eth.AddressAsLeftPaddedHash(*Submits.To)
	}
	topics := []common.Hash{
		SubmitEventABIHash,
		eth.AddressAsLeftPaddedHash(Submits.From),
		toBytes,
		SubmitEventVersion0,
	}

	data := make([]byte, 64, 64+3*32)

	// opaqueData slice content offset: value will always be 0x20.
	binary.BigEndian.PutUint64(data[32-8:32], 32)

	opaqueData, err := marshalSubmitsVersion0(Submits)
	if err != nil {
		return &types.Log{}, err
	}

	// opaqueData slice length
	binary.BigEndian.PutUint64(data[64-8:64], uint64(len(opaqueData)))

	// opaqueData slice content
	data = append(data, opaqueData...)

	// pad to multiple of 32
	if len(data)%32 != 0 {
		data = append(data, make([]byte, 32-(len(data)%32))...)
	}

	return &types.Log{
		Address: SubmitsContractAddr,
		Topics:  topics,
		Data:    data,
		Removed: false,

		// ignored (zeroed):
		BlockNumber: 0,
		TxHash:      common.Hash{},
		TxIndex:     0,
		BlockHash:   common.Hash{},
		Index:       0,
	}, nil
}

func marshalSubmitsVersion0(Submits *types.DepositTx) ([]byte, error) {
	opaqueData := make([]byte, 32+32+8+1, 32+32+8+1+len(Submits.Data))
	offset := 0

	// uint256 mint
	if Submits.Mint != nil {
		if Submits.Mint.BitLen() > 256 {
			return nil, fmt.Errorf("mint value exceeds 256 bits: %d", Submits.Mint)
		}
		Submits.Mint.FillBytes(opaqueData[offset : offset+32])
	}
	offset += 32

	// uint256 value
	if Submits.Value.BitLen() > 256 {
		return nil, fmt.Errorf("value value exceeds 256 bits: %d", Submits.Value)
	}
	Submits.Value.FillBytes(opaqueData[offset : offset+32])
	offset += 32

	// uint64 gas
	binary.BigEndian.PutUint64(opaqueData[offset:offset+8], Submits.Gas)
	offset += 8

	// uint8 isCreation
	if Submits.To == nil { // isCreation
		opaqueData[offset] = 1
	}

	// Submits data then fills the remaining event data
	opaqueData = append(opaqueData, Submits.Data...)

	return opaqueData, nil
}
