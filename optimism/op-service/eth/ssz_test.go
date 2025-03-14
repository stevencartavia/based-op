package eth

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/holiman/uint256"
	"github.com/karalabe/ssz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// FuzzExecutionPayloadUnmarshal checks that our SSZ decoding never panics
func FuzzExecutionPayloadUnmarshal(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		{
			var payload ExecutionPayload
			err := payload.UnmarshalSSZ(BlockV1, uint32(len(data)), bytes.NewReader(data))
			if err != nil {
				// not every input is a valid ExecutionPayload, that's ok. Should just not panic.
				return
			}
		}

		{
			var payload ExecutionPayload
			err := payload.UnmarshalSSZ(BlockV2, uint32(len(data)), bytes.NewReader(data))
			if err != nil {
				// not every input is a valid ExecutionPayload, that's ok. Should just not panic.
				return
			}
		}
	})
}

func TestExecutionPayloadUnmarshalMaliciousData(t *testing.T) {
	// Prior to https://github.com/ethereum-optimism/optimism/pull/10362 this causes a panic
	// It should return an error and not panic
	data := []byte("0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000\x00\x02\x00\x000000000000000000000000000000000000000000000000000000000000000000\x00\x02\x00\x000000")
	var payload ExecutionPayload
	err := payload.UnmarshalSSZ(BlockV2, uint32(len(data)), bytes.NewReader(data))
	require.Error(t, err)
}

// FuzzExecutionPayloadMarshalUnmarshal checks that our SSZ encoding>decoding round trips properly
func FuzzExecutionPayloadMarshalUnmarshalV1(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, a, b, c, d uint64, extraData []byte, txs uint16, txsData []byte) {
		if len(data) < 32+20+32+32+256+32+32+32 {
			return
		}
		var payload ExecutionPayload
		payload.ParentHash = *(*common.Hash)(data[:32])
		data = data[32:]
		payload.FeeRecipient = *(*common.Address)(data[:20])
		data = data[20:]
		payload.StateRoot = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.ReceiptsRoot = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.LogsBloom = *(*Bytes256)(data[:256])
		data = data[256:]
		payload.PrevRandao = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.BlockNumber = Uint64Quantity(a)
		payload.GasLimit = Uint64Quantity(a)
		payload.GasUsed = Uint64Quantity(a)
		payload.Timestamp = Uint64Quantity(a)
		if len(extraData) > 32 {
			extraData = extraData[:32]
		}
		payload.ExtraData = extraData
		(*uint256.Int)(&payload.BaseFeePerGas).SetBytes(data[:32])
		payload.BlockHash = *(*common.Hash)(data[:32])
		payload.Transactions = make([]Data, txs)
		for i := 0; i < int(txs); i++ {
			if len(txsData) < 2 {
				payload.Transactions[i] = make(Data, 0)
				continue
			}
			txSize := binary.LittleEndian.Uint16(txsData[:2])
			txsData = txsData[2:]
			if int(txSize) > len(txsData) {
				txSize = uint16(len(txsData))
			}
			payload.Transactions[i] = txsData[:txSize]
			txsData = txsData[txSize:]
		}
		var buf bytes.Buffer
		if _, err := payload.MarshalSSZ(&buf); err != nil {
			t.Fatalf("failed to marshal ExecutionPayload: %v", err)
		}
		var roundTripped ExecutionPayload
		err := roundTripped.UnmarshalSSZ(BlockV1, uint32(len(buf.Bytes())), bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("failed to decode previously marshalled payload: %v", err)
		}
		if diff := cmp.Diff(payload, roundTripped); diff != "" {
			t.Fatalf("The data did not round trip correctly:\n%s", diff)
		}
	})
}

func FuzzExecutionPayloadMarshalUnmarshalV2(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, a, b, c, d uint64, extraData []byte, txs uint16, txsData []byte, wCount uint16) {
		if len(data) < 32+20+32+32+256+32+32+32 {
			return
		}
		var payload ExecutionPayload
		payload.ParentHash = *(*common.Hash)(data[:32])
		data = data[32:]
		payload.FeeRecipient = *(*common.Address)(data[:20])
		data = data[20:]
		payload.StateRoot = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.ReceiptsRoot = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.LogsBloom = *(*Bytes256)(data[:256])
		data = data[256:]
		payload.PrevRandao = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.BlockNumber = Uint64Quantity(a)
		payload.GasLimit = Uint64Quantity(a)
		payload.GasUsed = Uint64Quantity(a)
		payload.Timestamp = Uint64Quantity(a)
		if len(extraData) > 32 {
			extraData = extraData[:32]
		}
		payload.ExtraData = extraData
		(*uint256.Int)(&payload.BaseFeePerGas).SetBytes(data[:32])
		payload.BlockHash = *(*common.Hash)(data[:32])
		payload.Transactions = make([]Data, txs)
		for i := 0; i < int(txs); i++ {
			if len(txsData) < 2 {
				payload.Transactions[i] = make(Data, 0)
				continue
			}
			txSize := binary.LittleEndian.Uint16(txsData[:2])
			txsData = txsData[2:]
			if int(txSize) > len(txsData) {
				txSize = uint16(len(txsData))
			}
			payload.Transactions[i] = txsData[:txSize]
			txsData = txsData[txSize:]
		}

		wCount = wCount % maxWithdrawalsPerPayload
		withdrawals := make(types.Withdrawals, wCount)
		for i := 0; i < int(wCount); i++ {
			withdrawals[i] = &types.Withdrawal{
				Index:     a,
				Validator: b,
				Address:   common.BytesToAddress(data[:20]),
				Amount:    c,
			}
		}
		payload.Withdrawals = &withdrawals

		var buf bytes.Buffer
		if _, err := payload.MarshalSSZ(&buf); err != nil {
			t.Fatalf("failed to marshal ExecutionPayload: %v", err)
		}
		var roundTripped ExecutionPayload
		err := roundTripped.UnmarshalSSZ(BlockV2, uint32(len(buf.Bytes())), bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("failed to decode previously marshalled payload: %v", err)
		}
		if diff := cmp.Diff(payload, roundTripped); diff != "" {
			t.Fatalf("The data did not round trip correctly:\n%s", diff)
		}
	})
}

func FuzzExecutionPayloadMarshalUnmarshalV3(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, a, b, c, d uint64, extraData []byte, txs uint16, txsData []byte, wCount uint16, blobGasUsed, excessBlobGas uint64) {
		if len(data) < 32+20+32+32+256+32+32+32 {
			return
		}
		var payload ExecutionPayload
		payload.ParentHash = *(*common.Hash)(data[:32])
		data = data[32:]
		payload.FeeRecipient = *(*common.Address)(data[:20])
		data = data[20:]
		payload.StateRoot = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.ReceiptsRoot = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.LogsBloom = *(*Bytes256)(data[:256])
		data = data[256:]
		payload.PrevRandao = *(*Bytes32)(data[:32])
		data = data[32:]
		payload.BlockNumber = Uint64Quantity(a)
		payload.GasLimit = Uint64Quantity(a)
		payload.GasUsed = Uint64Quantity(a)
		payload.Timestamp = Uint64Quantity(a)
		payload.BlobGasUsed = (*Uint64Quantity)(&blobGasUsed)
		payload.ExcessBlobGas = (*Uint64Quantity)(&excessBlobGas)
		if len(extraData) > 32 {
			extraData = extraData[:32]
		}
		payload.ExtraData = extraData
		(*uint256.Int)(&payload.BaseFeePerGas).SetBytes(data[:32])
		payload.BlockHash = *(*common.Hash)(data[:32])
		payload.Transactions = make([]Data, txs)
		for i := 0; i < int(txs); i++ {
			if len(txsData) < 2 {
				payload.Transactions[i] = make(Data, 0)
				continue
			}
			txSize := binary.LittleEndian.Uint16(txsData[:2])
			txsData = txsData[2:]
			if int(txSize) > len(txsData) {
				txSize = uint16(len(txsData))
			}
			payload.Transactions[i] = txsData[:txSize]
			txsData = txsData[txSize:]
		}

		wCount = wCount % maxWithdrawalsPerPayload
		withdrawals := make(types.Withdrawals, wCount)
		for i := 0; i < int(wCount); i++ {
			withdrawals[i] = &types.Withdrawal{
				Index:     a,
				Validator: b,
				Address:   common.BytesToAddress(data[:20]),
				Amount:    c,
			}
		}
		payload.Withdrawals = &withdrawals

		var buf bytes.Buffer
		if _, err := payload.MarshalSSZ(&buf); err != nil {
			t.Fatalf("failed to marshal ExecutionPayload: %v", err)
		}
		var roundTripped ExecutionPayload
		err := roundTripped.UnmarshalSSZ(BlockV3, uint32(len(buf.Bytes())), bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("failed to decode previously marshalled payload: %v", err)
		}
		if diff := cmp.Diff(payload, roundTripped); diff != "" {
			t.Fatalf("The data did not round trip correctly:\n%s", diff)
		}
	})
}

func FuzzOBP01(f *testing.F) {
	payload := &ExecutionPayload{
		ExtraData: make([]byte, 32),
	}
	var buf bytes.Buffer
	_, err := payload.MarshalSSZ(&buf)
	require.NoError(f, err)
	data := buf.Bytes()

	f.Fuzz(func(t *testing.T, edOffset uint32, txOffset uint32) {
		clone := make([]byte, len(data))
		copy(clone, data)

		binary.LittleEndian.PutUint32(clone[436:440], edOffset)
		binary.LittleEndian.PutUint32(clone[504:508], txOffset)

		var unmarshalled ExecutionPayload
		err = unmarshalled.UnmarshalSSZ(BlockV1, uint32(len(clone)), bytes.NewReader(clone))
		if err == nil {
			t.Fatalf("expected a failure, but didn't get one")
		}
	})
}

// TestOPB01 verifies that the SSZ unmarshalling code
// properly checks for the transactionOffset being larger
// than the extraDataOffset.
func TestOPB01(t *testing.T) {
	payload := &ExecutionPayload{
		ExtraData: make([]byte, 32),
	}
	var buf bytes.Buffer
	_, err := payload.MarshalSSZ(&buf)
	require.NoError(t, err)
	data := buf.Bytes()

	// transactions offset is set between indices 504 and 508
	copy(data[504:508], make([]byte, 4))

	var unmarshalled ExecutionPayload
	err = unmarshalled.UnmarshalSSZ(BlockV1, uint32(len(data)), bytes.NewReader(data))
	require.Equal(t, ErrBadTransactionOffset, err)
}

// TestOPB04 verifies that the SSZ marshaling code
// properly returns an error when the ExtraData field
// cannot be represented in the outputted SSZ.
func TestOPB04(t *testing.T) {
	data := make([]byte, math.MaxUint32)

	var buf bytes.Buffer
	// First, test the maximum len - which in this case is the max uint32
	// minus the execution payload fixed part.
	payload := &ExecutionPayload{
		ExtraData:   data[:math.MaxUint32-executionPayloadFixedPart(BlockV1)],
		Withdrawals: nil,
	}

	_, err := payload.MarshalSSZ(&buf)
	require.NoError(t, err)
	buf.Reset()

	tests := []struct {
		version     BlockVersion
		withdrawals *types.Withdrawals
	}{
		{BlockV1, nil},
		{BlockV2, &types.Withdrawals{}},
	}

	for _, test := range tests {
		payload := &ExecutionPayload{
			ExtraData:   data[:math.MaxUint32-executionPayloadFixedPart(test.version)+1],
			Withdrawals: test.withdrawals,
		}
		_, err := payload.MarshalSSZ(&buf)
		require.Error(t, err)
		require.Equal(t, ErrExtraDataTooLarge, err)
	}
}

func createPayloadWithWithdrawals(w *types.Withdrawals) *ExecutionPayload {
	return &ExecutionPayload{
		ParentHash:    common.HexToHash("0x123"),
		FeeRecipient:  common.HexToAddress("0x456"),
		StateRoot:     Bytes32(common.HexToHash("0x789")),
		ReceiptsRoot:  Bytes32(common.HexToHash("0xabc")),
		LogsBloom:     Bytes256{byte(13), byte(14), byte(15)},
		PrevRandao:    Bytes32(common.HexToHash("0x111")),
		BlockNumber:   Uint64Quantity(222),
		GasLimit:      Uint64Quantity(333),
		GasUsed:       Uint64Quantity(444),
		Timestamp:     Uint64Quantity(555),
		ExtraData:     common.FromHex("6666"),
		BaseFeePerGas: Uint256Quantity(*uint256.NewInt(777)),
		BlockHash:     common.HexToHash("0x888"),
		Withdrawals:   w,
		Transactions:  []Data{common.FromHex("9999")},
	}
}

func TestMarshalUnmarshalWithdrawals(t *testing.T) {
	emptyWithdrawal := &types.Withdrawals{}
	withdrawals := &types.Withdrawals{
		{
			Index:     987,
			Validator: 654,
			Address:   common.HexToAddress("0x898"),
			Amount:    321,
		},
	}
	maxWithdrawals := make(types.Withdrawals, maxWithdrawalsPerPayload)
	for i := 0; i < maxWithdrawalsPerPayload; i++ {
		maxWithdrawals[i] = &types.Withdrawal{
			Index:     987,
			Validator: 654,
			Address:   common.HexToAddress("0x898"),
			Amount:    321,
		}
	}
	tooManyWithdrawals := make(types.Withdrawals, maxWithdrawalsPerPayload+1)
	for i := 0; i < maxWithdrawalsPerPayload+1; i++ {
		tooManyWithdrawals[i] = &types.Withdrawal{
			Index:     987,
			Validator: 654,
			Address:   common.HexToAddress("0x898"),
			Amount:    321,
		}
	}

	tests := []struct {
		name        string
		version     BlockVersion
		hasError    bool
		withdrawals *types.Withdrawals
	}{
		{"ZeroWithdrawalsSucceeds", BlockV2, false, emptyWithdrawal},
		{"ZeroWithdrawalsFailsToDeserialize", BlockV1, true, emptyWithdrawal},
		{"WithdrawalsSucceeds", BlockV2, false, withdrawals},
		{"WithdrawalsFailsToDeserialize", BlockV1, true, withdrawals},
		{"MaxWithdrawalsSucceeds", BlockV2, false, &maxWithdrawals},
		{"TooManyWithdrawalsErrors", BlockV2, true, &tooManyWithdrawals},
	}

	for _, test := range tests {
		test := test

		t.Run(fmt.Sprintf("TestWithdrawalUnmarshalMarshal_%s", test.name), func(t *testing.T) {
			input := createPayloadWithWithdrawals(test.withdrawals)

			var buf bytes.Buffer
			_, err := input.MarshalSSZ(&buf)
			require.NoError(t, err)
			data := buf.Bytes()

			output := &ExecutionPayload{}
			err = output.UnmarshalSSZ(test.version, uint32(len(data)), bytes.NewReader(data))

			if test.hasError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, input, output)
				if test.withdrawals != nil {
					require.Equal(t, len(*test.withdrawals), len(*output.Withdrawals))
				}
			}
		})
	}
}

func TestMarshalUnmarshalExecutionPayloadEnvelopes(t *testing.T) {
	hash := common.HexToHash("0x123")

	zero := uint64(0)
	validInput := &ExecutionPayloadEnvelope{
		ParentBeaconBlockRoot: &hash,
		ExecutionPayload:      createPayloadWithWithdrawals(&types.Withdrawals{}),
	}
	validInput.ExecutionPayload.ExcessBlobGas = (*Uint64Quantity)(&zero)
	validInput.ExecutionPayload.BlobGasUsed = (*Uint64Quantity)(&zero)

	missingHash := &ExecutionPayloadEnvelope{
		ParentBeaconBlockRoot: nil,
		ExecutionPayload:      createPayloadWithWithdrawals(&types.Withdrawals{}),
	}

	missingExecutionPayload := &ExecutionPayloadEnvelope{
		ParentBeaconBlockRoot: &hash,
		ExecutionPayload:      nil,
	}

	tests := []struct {
		name  string
		input *ExecutionPayloadEnvelope
		err   error
	}{
		{"ValidInputSucceeds", validInput, nil},
		{"MissingHashFailsToSerialize", missingHash, ErrMissingData},
		{"MissingExecutionDataFailsToSerialize", missingExecutionPayload, ErrMissingData},
	}

	for _, test := range tests {
		test := test

		t.Run(fmt.Sprintf("TestExecutionPayloadEnvelopeMarshalUnmarshal_%s", test.name), func(t *testing.T) {
			hash := common.HexToHash("0x123")

			var buf bytes.Buffer
			_, err := test.input.MarshalSSZ(&buf)

			if test.err != nil {
				require.ErrorIs(t, err, test.err)
				return
			} else {
				require.NoError(t, err)
			}

			data := buf.Bytes()

			output := &ExecutionPayloadEnvelope{}
			err = output.UnmarshalSSZ(uint32(len(data)), bytes.NewReader(data))

			require.NoError(t, err)

			require.NotNil(t, output.ParentBeaconBlockRoot)
			assert.Equal(t, hash, *output.ParentBeaconBlockRoot)

			require.NotNil(t, output.ExecutionPayload)
			if diff := cmp.Diff(*test.input.ExecutionPayload, *output.ExecutionPayload); diff != "" {
				t.Fatalf("The data did not round trip correctly:\n%s", diff)
			}
		})
	}
}

func TestFailsToDeserializeTooLittleData(t *testing.T) {
	var payload ExecutionPayloadEnvelope
	err := payload.UnmarshalSSZ(1, bytes.NewReader([]byte{0x00}))
	assert.Equal(t, err, errors.New("scope too small to decode execution payload envelope: 1"))
}

func TestMarshalUnmarshalNewFrag(t *testing.T) {
	f := NewFrag{
		BlockNumber: 7,
		Seq:         3,
		IsLast:      false,
		Txs: [][]byte{
			{0x01, 0x02, 0x03},
			{0x04, 0x05, 0x06, 0x07},
		},
	}

	var buf bytes.Buffer

	err := f.MarshalSSZ(&buf)
	require.NoError(t, err)

	data := buf.Bytes()

	unmarshalled := &NewFrag{}
	err = unmarshalled.UnmarshalSSZ(uint32(len(data)), bytes.NewReader(data))

	require.NoError(t, err)

	if diff := cmp.Diff(f, *unmarshalled); diff != "" {
		t.Fatalf("The data did not round trip correctly:\n%s", diff)
	}
}

func TestMarshalUnmarshalSignedNewFrag(t *testing.T) {
	f := SignedNewFrag{
		Signature: Bytes65{0x01, 0x42, 0x65, 0x07, 0x01, 0x42, 0x65, 0x07, 0x01, 0x42, 0x65, 0x07},
		Frag: NewFrag{
			BlockNumber: 7,
			Seq:         3,
			IsLast:      false,
			Txs: [][]byte{
				{0x01, 0x02, 0x03},
				{0x04, 0x05, 0x06, 0x07},
			},
		},
	}

	var buf bytes.Buffer

	err := f.MarshalSSZ(&buf)
	require.NoError(t, err)

	data := buf.Bytes()

	unmarshalled := &SignedNewFrag{}
	err = unmarshalled.UnmarshalSSZ(uint32(len(data)), bytes.NewReader(data))

	require.NoError(t, err)

	if diff := cmp.Diff(f, *unmarshalled); diff != "" {
		t.Fatalf("The data did not round trip correctly:\n%s", diff)
	}
}

func TestMarshalUnmarshalSeal(t *testing.T) {
	s := Seal{
		TotalFrags:       10,
		BlockNumber:      256,
		GasUsed:          30000,
		GasLimit:         60000,
		ParentHash:       Bytes32{0x01, 0x03, 0x05},
		TransactionsRoot: Bytes32{0x02, 0x03, 0x04, 0x7},
		ReceiptsRoot:     Bytes32{0x00, 0x08},
		StateRoot:        Bytes32{0xff, 0xfe, 0xfa},
		BlockHash:        Bytes32{0xaa, 0xbb, 0xcc, 0xdd, 0xee},
	}

	var buf bytes.Buffer

	err := ssz.EncodeToStream(&buf, &s)
	require.NoError(t, err)

	data := buf.Bytes()

	unmarshalled := &Seal{}
	err = ssz.DecodeFromStream(bytes.NewBuffer(data), unmarshalled, uint32(len(data)))

	require.NoError(t, err)

	if diff := cmp.Diff(s, *unmarshalled); diff != "" {
		t.Fatalf("The data did not round trip correctly:\n%s", diff)
	}
}

func TestMarshalUnmarshalSignedSeal(t *testing.T) {
	s := SignedSeal{
		Signature: Bytes65{0x01, 0x42, 0x65, 0x07, 0x01, 0x42, 0x65, 0x07, 0x01, 0x42, 0x65, 0x07},
		Seal: Seal{
			TotalFrags:       10,
			BlockNumber:      256,
			GasUsed:          30000,
			GasLimit:         60000,
			ParentHash:       Bytes32{0x01, 0x03, 0x05},
			TransactionsRoot: Bytes32{0x02, 0x03, 0x04, 0x7},
			ReceiptsRoot:     Bytes32{0x00, 0x08},
			StateRoot:        Bytes32{0xff, 0xfe, 0xfa},
			BlockHash:        Bytes32{0xaa, 0xbb, 0xcc, 0xdd, 0xee},
		}}

	var buf bytes.Buffer

	err := s.MarshalSSZ(&buf)
	require.NoError(t, err)

	data := buf.Bytes()

	unmarshalled := &SignedSeal{}
	err = unmarshalled.UnmarshalSSZ(uint32(len(data)), bytes.NewBuffer(data))
	require.NoError(t, err)

	if diff := cmp.Diff(s, *unmarshalled); diff != "" {
		t.Fatalf("The data did not round trip correctly:\n%s", diff)
	}
}

func TestMarshalUnmarshalSignedEnv(t *testing.T) {
	e := SignedEnv{
		Signature: Bytes65{0x6a, 0x5f, 0x84, 0xdb, 0xf9, 0x72, 0x26, 0x62, 0x07, 0xfb, 0xe1, 0x42, 0xfe, 0x48, 0x42, 0xfc, 0x80, 0x4c, 0xc7, 0xca, 0xcc, 0xa2, 0x8b, 0x60, 0x4e, 0xb0, 0xae, 0x21, 0x56, 0x63, 0xa8, 0x27, 0x64, 0x76, 0x23, 0xa9, 0x06, 0x4c, 0x73, 0xa4, 0x3e, 0x20, 0x09, 0x6a, 0x58, 0xfb, 0x0b, 0x31, 0xeb, 0xa3, 0x8f, 0x1d, 0x51, 0xf3, 0x2b, 0x35, 0x49, 0xeb, 0xc7, 0x8b, 0xb5, 0x37, 0x8c, 0x0d, 0x1c},
		Env: Env{
			Number:                97,
			Beneficiary:           decodeB20("4200000000000000000000000000000000000011"),
			Timestamp:             1741894329,
			GasLimit:              60000000,
			Basefee:               679116901,
			Difficulty:            common.U2560,
			Prevrandao:            common.BytesToHash(decodeOrPanic("75ed449926e893872b74e7c73e207710b042d04d055b6234728b009f2bc70635")),
			ParentHash:            common.BytesToHash(decodeOrPanic("1fcb07f5b2c4783244ae53fa10362d931e81cdde3ee6c63992df5a3e7ffe8e88")),
			ParentBeaconBlockRoot: common.BytesToHash(decodeOrPanic("44188e8e70142a7897d952a999f784a546b80e83078c864f7d9fbaba32f0a5b2")),
			ExtraData:             []byte{0x00, 0x00, 0x00, 0x00, 0xfa, 0x00, 0x00, 0x00, 0x06},
		},
	}

	var buf bytes.Buffer

	err := e.MarshalSSZ(&buf)
	require.NoError(t, err)

	data := buf.Bytes()

	unmarshalled := &SignedEnv{}
	err = unmarshalled.UnmarshalSSZ(uint32(len(data)), bytes.NewBuffer(data))
	require.NoError(t, err)
	if diff := cmp.Diff(e, *unmarshalled, cmp.AllowUnexported(big.Int{})); diff != "" {
		t.Fatalf("The data did not round trip correctly:\n%s", diff)
	}
}

func decodeOrPanic(s string) []byte {
	decoded, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return decoded
}

func decodeB32(s string) Bytes32 {
	return Bytes32(decodeOrPanic(s))
}

func decodeB20(s string) common.Address {
	return common.BytesToAddress(decodeOrPanic(s))
}

func TestSealRoot(t *testing.T) {
	s := Seal{
		TotalFrags:       8,
		BlockNumber:      123,
		GasUsed:          25_000,
		GasLimit:         1_000_000,
		ParentHash:       decodeB32("e75fae0065403d4091f3d6549c4219db69c96d9de761cfc75fe9792b6166c758"),
		TransactionsRoot: decodeB32("e75fae0065403d4091f3d6549c4219db69c96d9de761cfc75fe9792b6166c758"),
		ReceiptsRoot:     decodeB32("e75fae0065403d4091f3d6549c4219db69c96d9de761cfc75fe9792b6166c758"),
		StateRoot:        decodeB32("e75fae0065403d4091f3d6549c4219db69c96d9de761cfc75fe9792b6166c758"),
		BlockHash:        decodeB32("e75fae0065403d4091f3d6549c4219db69c96d9de761cfc75fe9792b6166c758"),
	}

	expected := decodeB32("e86afda21ddc7338c7e84561681fde45e2ab55cce8cde3163e0ae5f1c378439e")
	root := s.Root()

	if expected != root {
		t.Fatalf("Expected root %s, found %s", expected.String(), root.String())
	}
}

func TestFragRoot(t *testing.T) {
	transactions := [][]byte{{1, 2, 3}}
	f := NewFrag{
		BlockNumber: 1,
		Seq:         0,
		IsLast:      true,
		Txs:         transactions,
	}

	expected := decodeB32("2a5ebad20a81878e5f229928e5c2043580051673b89a7a286008d30f62b10963")
	root := f.Root()

	if expected != root {
		t.Fatalf("Expected root %s, found %s", expected.String(), root.String())
	}
}

func TestEnvRoot(t *testing.T) {
	e := Env{
		Number:                1,
		Beneficiary:           decodeB20("1234567890123456789012345678901234567890"),
		Timestamp:             2,
		GasLimit:              3,
		Basefee:               4,
		Difficulty:            uint256.NewInt(5),
		Prevrandao:            common.BytesToHash(decodeOrPanic("e75fae0065403d4091f3d6549c4219db69c96d9de761cfc75fe9792b6166c758")),
		ParentHash:            common.BytesToHash(decodeOrPanic("e75fae0065403d4091f3d6549c4219db69c96d9de761cfc75fe9792b6166c758")),
		ParentBeaconBlockRoot: common.BytesToHash(decodeOrPanic("e75fae0065403d4091f3d6549c4219db69c96d9de761cfc75fe9792b6166c758")),
		ExtraData:             []byte{0x01, 0x02, 0x03},
	}

	expected := decodeB32("fa09df7670737568ba783dfd934e19b06e6681e367a866a5647449bd4e5ca324")
	root := e.Root()

	if expected != root {
		t.Fatalf("Expected root %s, found %s", expected.String(), root.String())
	}
}

func TestEnvRoot2(t *testing.T) {
	e := Env{
		Number:                97,
		ParentHash:            common.BytesToHash(decodeOrPanic("1fcb07f5b2c4783244ae53fa10362d931e81cdde3ee6c63992df5a3e7ffe8e88")),
		Beneficiary:           decodeB20("4200000000000000000000000000000000000011"),
		Timestamp:             1741894329,
		GasLimit:              60000000,
		Basefee:               679116901,
		Difficulty:            common.U2560,
		Prevrandao:            common.BytesToHash(decodeOrPanic("75ed449926e893872b74e7c73e207710b042d04d055b6234728b009f2bc70635")),
		ExtraData:             []byte{0x00, 0x00, 0x00, 0x00, 0xfa, 0x00, 0x00, 0x00, 0x06},
		ParentBeaconBlockRoot: common.BytesToHash(decodeOrPanic("44188e8e70142a7897d952a999f784a546b80e83078c864f7d9fbaba32f0a5b2")),
	}

	expected := decodeB32("779172e004a01f01249038ed576516345ee9ce46a5e678b61945f33eff7448bb")
	root := e.Root()

	if expected != root {
		t.Fatalf("Expected root %s, found %s", expected.String(), root.String())
	}
}
