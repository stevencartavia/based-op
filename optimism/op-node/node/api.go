package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/ethereum-optimism/optimism/op-node/node/safedb"
	"github.com/ethereum-optimism/optimism/op-node/p2p"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/version"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/metrics"
	"github.com/ethereum-optimism/optimism/op-service/rpc"
)

type l2EthClient interface {
	InfoByHash(ctx context.Context, hash common.Hash) (eth.BlockInfo, error)
	// GetProof returns a proof of the account, it may return a nil result without error if the address was not found.
	// Optionally keys of the account storage trie can be specified to include with corresponding values in the proof.
	GetProof(ctx context.Context, address common.Address, storage []common.Hash, blockTag string) (*eth.AccountResult, error)
	OutputV0AtBlock(ctx context.Context, blockHash common.Hash) (*eth.OutputV0, error)
}

type driverClient interface {
	SyncStatus(ctx context.Context) (*eth.SyncStatus, error)
	BlockRefWithStatus(ctx context.Context, num uint64) (eth.L2BlockRef, *eth.SyncStatus, error)
	ResetDerivationPipeline(context.Context) error
	StartSequencer(ctx context.Context, blockHash common.Hash) error
	StopSequencer(context.Context) (common.Hash, error)
	SequencerActive(context.Context) (bool, error)
	OnUnsafeL2Payload(ctx context.Context, payload *eth.ExecutionPayloadEnvelope) error
	OverrideLeader(ctx context.Context) error
	ConductorEnabled(ctx context.Context) (bool, error)
}

type SafeDBReader interface {
	SafeHeadAtL1(ctx context.Context, l1BlockNum uint64) (l1 eth.BlockID, l2 eth.BlockID, err error)
}

type adminAPI struct {
	*rpc.CommonAdminAPI
	dr driverClient
}

func NewAdminAPI(dr driverClient, m metrics.RPCMetricer, log log.Logger) *adminAPI {
	return &adminAPI{
		CommonAdminAPI: rpc.NewCommonAdminAPI(m, log),
		dr:             dr,
	}
}

func (n *adminAPI) ResetDerivationPipeline(ctx context.Context) error {
	recordDur := n.M.RecordRPCServerRequest("admin_resetDerivationPipeline")
	defer recordDur()
	return n.dr.ResetDerivationPipeline(ctx)
}

func (n *adminAPI) StartSequencer(ctx context.Context, blockHash common.Hash) error {
	recordDur := n.M.RecordRPCServerRequest("admin_startSequencer")
	defer recordDur()
	return n.dr.StartSequencer(ctx, blockHash)
}

func (n *adminAPI) StopSequencer(ctx context.Context) (common.Hash, error) {
	recordDur := n.M.RecordRPCServerRequest("admin_stopSequencer")
	defer recordDur()
	return n.dr.StopSequencer(ctx)
}

func (n *adminAPI) SequencerActive(ctx context.Context) (bool, error) {
	recordDur := n.M.RecordRPCServerRequest("admin_sequencerActive")
	defer recordDur()
	return n.dr.SequencerActive(ctx)
}

// PostUnsafePayload is a special API that allows posting an unsafe payload to the L2 derivation pipeline.
// It should only be used by op-conductor for sequencer failover scenarios.
func (n *adminAPI) PostUnsafePayload(ctx context.Context, envelope *eth.ExecutionPayloadEnvelope) error {
	recordDur := n.M.RecordRPCServerRequest("admin_postUnsafePayload")
	defer recordDur()

	payload := envelope.ExecutionPayload
	if actual, ok := envelope.CheckBlockHash(); !ok {
		log.Error("payload has bad block hash", "bad_hash", payload.BlockHash.String(), "actual", actual.String())
		return fmt.Errorf("payload has bad block hash: %s, actual block hash is: %s", payload.BlockHash.String(), actual.String())
	}

	return n.dr.OnUnsafeL2Payload(ctx, envelope)
}

// OverrideLeader disables sequencer conductor interactions and allow sequencer to run in non-HA mode during disaster recovery scenarios.
func (n *adminAPI) OverrideLeader(ctx context.Context) error {
	recordDur := n.M.RecordRPCServerRequest("admin_overrideLeader")
	defer recordDur()
	return n.dr.OverrideLeader(ctx)
}

// ConductorEnabled returns true if the sequencer conductor is enabled.
func (n *adminAPI) ConductorEnabled(ctx context.Context) (bool, error) {
	recordDur := n.M.RecordRPCServerRequest("admin_conductorEnabled")
	defer recordDur()
	return n.dr.ConductorEnabled(ctx)
}

type nodeAPI struct {
	config *rollup.Config
	client l2EthClient
	dr     driverClient
	safeDB SafeDBReader
	log    log.Logger
	m      metrics.RPCMetricer
}

func NewNodeAPI(config *rollup.Config, l2Client l2EthClient, dr driverClient, safeDB SafeDBReader, log log.Logger, m metrics.RPCMetricer) *nodeAPI {
	return &nodeAPI{
		config: config,
		client: l2Client,
		dr:     dr,
		safeDB: safeDB,
		log:    log,
		m:      m,
	}
}

func (n *nodeAPI) OutputAtBlock(ctx context.Context, number hexutil.Uint64) (*eth.OutputResponse, error) {
	recordDur := n.m.RecordRPCServerRequest("optimism_outputAtBlock")
	defer recordDur()

	ref, status, err := n.dr.BlockRefWithStatus(ctx, uint64(number))
	if err != nil {
		return nil, fmt.Errorf("failed to get L2 block ref with sync status: %w", err)
	}

	output, err := n.client.OutputV0AtBlock(ctx, ref.Hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get L2 output at block %s: %w", ref, err)
	}
	return &eth.OutputResponse{
		Version:               output.Version(),
		OutputRoot:            eth.OutputRoot(output),
		BlockRef:              ref,
		WithdrawalStorageRoot: common.Hash(output.MessagePasserStorageRoot),
		StateRoot:             common.Hash(output.StateRoot),
		Status:                status,
	}, nil
}

func (n *nodeAPI) SafeHeadAtL1Block(ctx context.Context, number hexutil.Uint64) (*eth.SafeHeadResponse, error) {
	recordDur := n.m.RecordRPCServerRequest("optimism_safeHeadAtL1Block")
	defer recordDur()
	l1Block, safeHead, err := n.safeDB.SafeHeadAtL1(ctx, uint64(number))
	if errors.Is(err, safedb.ErrNotFound) {
		return nil, err
	} else if err != nil {
		return nil, fmt.Errorf("failed to get safe head at l1 block %s: %w", number, err)
	}
	return &eth.SafeHeadResponse{
		L1Block:  l1Block,
		SafeHead: safeHead,
	}, nil
}

func (n *nodeAPI) SyncStatus(ctx context.Context) (*eth.SyncStatus, error) {
	recordDur := n.m.RecordRPCServerRequest("optimism_syncStatus")
	defer recordDur()
	return n.dr.SyncStatus(ctx)
}

func (n *nodeAPI) RollupConfig(_ context.Context) (*rollup.Config, error) {
	recordDur := n.m.RecordRPCServerRequest("optimism_rollupConfig")
	defer recordDur()
	return n.config, nil
}

func (n *nodeAPI) Version(ctx context.Context) (string, error) {
	recordDur := n.m.RecordRPCServerRequest("optimism_version")
	defer recordDur()
	return version.Version + "-" + version.Meta, nil
}

type basedAPI struct {
	p2p     p2p.Node
	log     log.Logger
	metrics metrics.RPCMetricer
}

func NewBasedAPI(node p2p.Node, log log.Logger, metrics metrics.RPCMetricer) *basedAPI {
	return &basedAPI{
		p2p:     node,
		log:     log,
		metrics: metrics,
	}
}

func verifySignature(log log.Logger, signatureBytes []byte, messageBytes []byte, expectedSigner common.Address) pubsub.ValidationResult {
	if len(signatureBytes) != 65 {
		log.Warn("invalid signature length", "signature", signatureBytes)
		return pubsub.ValidationReject
	}

	if signatureBytes[64] == 27 || signatureBytes[64] == 28 {
		signatureBytes[64] -= 27
	}

	pub, err := crypto.SigToPub(messageBytes, signatureBytes)
	if err != nil {
		log.Warn("invalid signature", "err", err)
		return pubsub.ValidationReject
	}

	msgSigner := crypto.PubkeyToAddress(*pub)
	if msgSigner != expectedSigner {
		log.Warn("unexpected signer", "addr", msgSigner, "expected", expectedSigner)
		return pubsub.ValidationReject
	}

	return pubsub.ValidationAccept
}

func (n *basedAPI) NewFrag(ctx context.Context, signedFrag eth.SignedNewFrag) (string, error) {
	recordDur := n.metrics.RecordRPCServerRequest("based_newFrag")
	defer recordDur()

	n.log.Info("NewFrag RPC request received")

	root := signedFrag.Frag.Root()

	expectedSigner := n.p2p.CurrentGateway()

	if validation := verifySignature(n.log, signedFrag.Signature[:], root[:], expectedSigner); validation != pubsub.ValidationAccept {
		return "ERROR", fmt.Errorf("signature validation failed")
	}

	n.log.Info("NewFrag RPC request accepted")

	if err := n.p2p.GossipOut().PublishNewFrag(ctx, n.p2p.Host().ID(), &signedFrag); err != nil {
		return "", fmt.Errorf("failed to publish new frag: %w", err)
	}

	return "OK", nil
}

func (n *basedAPI) SealFrag(ctx context.Context, signedSeal eth.SignedSeal) (string, error) {
	recordDur := n.metrics.RecordRPCServerRequest("based_sealFrag")
	defer recordDur()

	n.log.Info("SealFrag RPC request received", "seal", signedSeal.Seal)

	root := signedSeal.Seal.Root()

	expectedSigner := n.p2p.CurrentGateway()

	if validation := verifySignature(n.log, signedSeal.Signature[:], root[:], expectedSigner); validation != pubsub.ValidationAccept {
		return "ERROR", fmt.Errorf("signature validation failed")
	}

	n.log.Info("SealFrag RPC request accepted")

	if err := n.p2p.GossipOut().PublishSealFrag(ctx, n.p2p.Host().ID(), &signedSeal); err != nil {
		return "", fmt.Errorf("failed to publish new seal: %w", err)
	}

	return "OK", nil
}

func (n *basedAPI) Env(ctx context.Context, signedEnv eth.SignedEnv) (string, error) {
	recordDur := n.metrics.RecordRPCServerRequest("based_env")
	defer recordDur()

	n.log.Info("Env RPC request received", "env", signedEnv.Env)

	root := signedEnv.Env.Root()

	expectedSigner := n.p2p.CurrentGateway()

	if validation := verifySignature(n.log, signedEnv.Signature[:], root[:], expectedSigner); validation != pubsub.ValidationAccept {
		return "ERROR", fmt.Errorf("signature validation failed")
	}

	n.log.Info("Env RPC request accepted")

	if err := n.p2p.GossipOut().PublishEnv(ctx, n.p2p.Host().ID(), &signedEnv); err != nil {
		return "", fmt.Errorf("failed to publish new env: %w", err)
	}

	return "OK", nil
}
