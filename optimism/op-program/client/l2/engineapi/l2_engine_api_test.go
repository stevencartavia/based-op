package engineapi

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/testlog"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	geth "github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

func TestCreatedBlocksAreCached(t *testing.T) {
	logger, logs := testlog.CaptureLogger(t, log.LvlInfo)

	backend := newStubBackend(t)
	engineAPI := NewL2EngineAPI(logger, backend, nil)
	require.NotNil(t, engineAPI)
	genesis := backend.GetHeaderByNumber(0)
	genesisHash := genesis.Hash()
	eip1559Params := eth.Bytes8([]byte{0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8})
	result, err := engineAPI.ForkchoiceUpdatedV3(context.Background(), &eth.ForkchoiceState{
		HeadBlockHash:      genesisHash,
		SafeBlockHash:      genesisHash,
		FinalizedBlockHash: genesisHash,
	}, &eth.PayloadAttributes{
		Timestamp:             eth.Uint64Quantity(genesis.Time + 1),
		PrevRandao:            eth.Bytes32{0x11},
		SuggestedFeeRecipient: common.Address{0x33},
		Withdrawals:           &types.Withdrawals{},
		ParentBeaconBlockRoot: &common.Hash{0x22},
		NoTxPool:              false,
		GasLimit:              (*eth.Uint64Quantity)(&genesis.GasLimit),
		EIP1559Params:         &eip1559Params,
	})
	require.NoError(t, err)
	require.EqualValues(t, engine.VALID, result.PayloadStatus.Status)
	require.NotNil(t, result.PayloadID)

	envelope, err := engineAPI.GetPayloadV3(context.Background(), *result.PayloadID)
	require.NoError(t, err)
	require.NotNil(t, envelope)
	newPayloadResult, err := engineAPI.NewPayloadV3(context.Background(), envelope.ExecutionPayload, []common.Hash{}, envelope.ParentBeaconBlockRoot)
	require.NoError(t, err)
	require.EqualValues(t, engine.VALID, newPayloadResult.Status)

	foundLog := logs.FindLog(testlog.NewMessageFilter("Using existing beacon payload"))
	require.NotNil(t, foundLog)
	require.Equal(t, envelope.ExecutionPayload.BlockHash, foundLog.AttrValue("hash"))
}

func newStubBackend(t *testing.T) *stubCachingBackend {
	genesis := createGenesis()
	ethCfg := &ethconfig.Config{
		NetworkId:   genesis.Config.ChainID.Uint64(),
		Genesis:     genesis,
		StateScheme: rawdb.HashScheme,
		NoPruning:   true,
	}
	nodeCfg := &node.Config{
		Name: "l2-geth",
	}
	n, err := node.New(nodeCfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = n.Close()
	})
	backend, err := geth.New(n, ethCfg)
	require.NoError(t, err)

	chain := backend.BlockChain()
	return &stubCachingBackend{EngineBackend: chain}
}

func createGenesis() *core.Genesis {
	config := *params.MergedTestChainConfig
	var zero uint64
	// activate recent OP-stack forks
	config.RegolithTime = &zero
	config.CanyonTime = &zero
	config.EcotoneTime = &zero
	config.FjordTime = &zero
	config.GraniteTime = &zero
	config.HoloceneTime = &zero

	l2Genesis := &core.Genesis{
		Config:     &config,
		Difficulty: common.Big0,
		ParentHash: common.Hash{},
		BaseFee:    big.NewInt(7),
		Alloc:      map[common.Address]types.Account{},
		ExtraData:  []byte{0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8}, // for Holocene eip-1559 params
	}

	return l2Genesis
}

type stubCachingBackend struct {
	EngineBackend
}

func (s *stubCachingBackend) AssembleAndInsertBlockWithoutSetHead(processor *BlockProcessor) (*types.Block, error) {
	block, err := processor.Assemble()
	if err != nil {
		return nil, err
	}
	if _, err := s.EngineBackend.InsertBlockWithoutSetHead(block, false); err != nil {
		return nil, err
	}
	return block, nil
}

var _ CachingEngineBackend = (*stubCachingBackend)(nil)

func TestNewFragV0(t *testing.T) {
	logger, _ := testlog.CaptureLogger(t, log.LvlInfo)

	backend := newStubBackend(t)
	engineAPI := NewL2EngineAPI(logger, backend, nil)

	frag := &eth.SignedNewFrag{
		Signature: eth.Bytes65{},
		Frag: eth.NewFrag{
			BlockNumber: 10,
			Seq:         0,
			IsLast:      true,
			Txs:         make([][]byte, 0),
		},
	}

	res, err := engineAPI.NewFragV0(context.Background(), frag)

	require.EqualValues(t, engine.VALID, res)
	require.NoError(t, err)
}

func TestSealFragV0(t *testing.T) {
	logger, _ := testlog.CaptureLogger(t, log.LvlInfo)

	backend := newStubBackend(t)
	engineAPI := NewL2EngineAPI(logger, backend, nil)

	seal := &eth.SignedSeal{
		Signature: eth.Bytes65{},
		Seal: eth.Seal{
			TotalFrags:       1,
			BlockNumber:      1,
			GasUsed:          0,
			GasLimit:         0,
			ParentHash:       eth.Bytes32{},
			TransactionsRoot: eth.Bytes32{},
			ReceiptsRoot:     eth.Bytes32{},
			StateRoot:        eth.Bytes32{},
			BlockHash:        eth.Bytes32{},
		}}

	res, err := engineAPI.SealFragV0(context.Background(), seal)

	require.EqualValues(t, engine.VALID, res)
	require.NoError(t, err)
}

func TestEnvV0(t *testing.T) {
	logger, _ := testlog.CaptureLogger(t, log.LvlInfo)

	backend := newStubBackend(t)
	engineAPI := NewL2EngineAPI(logger, backend, nil)

	env := &eth.SignedEnv{
		Signature: eth.Bytes65{},
		Env: eth.Env{
			Number:      1,
			Beneficiary: common.HexToAddress("0x0102030405060708091011121314151617181920"),
			Timestamp:   2,
			GasLimit:    3,
			Basefee:     4,
			Difficulty:  new(uint256.Int).SetUint64(123123123123123),
			Prevrandao:  common.HexToHash("0x0102030405060708091011121314151617181920212223242526272829303132"),
		}}

	res, err := engineAPI.EnvV0(context.Background(), env)

	require.EqualValues(t, engine.VALID, res)
	require.NoError(t, err)
}
