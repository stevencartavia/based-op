package p2p

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang/snappy"
	lru "github.com/hashicorp/golang-lru/v2"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

const (
	// maxGossipSize limits the total size of gossip RPC containers as well as decompressed individual messages.
	maxGossipSize = 10 * (1 << 20)
	// minGossipSize is used to make sure that there is at least some data to validate the signature against.
	minGossipSize          = 66
	maxOutboundQueue       = 256
	maxValidateQueue       = 256
	globalValidateThrottle = 512
	gossipHeartbeat        = 500 * time.Millisecond
	// seenMessagesTTL limits the duration that message IDs are remembered for gossip deduplication purposes
	// 130 * gossipHeartbeat
	seenMessagesTTL  = 130 * gossipHeartbeat
	DefaultMeshD     = 8  // topic stable mesh target count
	DefaultMeshDlo   = 6  // topic stable mesh low watermark
	DefaultMeshDhi   = 12 // topic stable mesh high watermark
	DefaultMeshDlazy = 6  // gossip target
	// peerScoreInspectFrequency is the frequency at which peer scores are inspected
	peerScoreInspectFrequency = 15 * time.Second
)

// Message domains, the msg id function uncompresses to keep data monomorphic,
// but invalid compressed data will need a unique different id.

var MessageDomainInvalidSnappy = [4]byte{0, 0, 0, 0}
var MessageDomainValidSnappy = [4]byte{1, 0, 0, 0}

type GossipSetupConfigurables interface {
	PeerScoringParams() *ScoringParams
	// ConfigureGossip creates configuration options to apply to the GossipSub setup
	ConfigureGossip(rollupCfg *rollup.Config) []pubsub.Option
}

type GossipRuntimeConfig interface {
	P2PSequencerAddress() common.Address
	CurrentGateway() common.Address
}

//go:generate mockery --name GossipMetricer
type GossipMetricer interface {
	RecordGossipEvent(evType int32)
}

func blocksTopicV1(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/0/blocks", cfg.L2ChainID.String())
}

func blocksTopicV2(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/1/blocks", cfg.L2ChainID.String())
}

func blocksTopicV3(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/2/blocks", cfg.L2ChainID.String())
}

func newFragV0(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/0/fragments", cfg.L2ChainID.String())
}

func sealFragV0(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/0/seals", cfg.L2ChainID.String())
}

func envV0(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/0/envs", cfg.L2ChainID.String())
}

// BuildSubscriptionFilter builds a simple subscription filter,
// to help protect against peers spamming useless subscriptions.
func BuildSubscriptionFilter(cfg *rollup.Config) pubsub.SubscriptionFilter {
	return pubsub.NewAllowlistSubscriptionFilter(blocksTopicV1(cfg), blocksTopicV2(cfg), blocksTopicV3(cfg), newFragV0(cfg), sealFragV0(cfg), envV0(cfg)) // add more topics here in the future, if any.
}

var msgBufPool = sync.Pool{New: func() any {
	// note: the topic validator concurrency is limited, so pool won't blow up, even with large pre-allocation.
	x := make([]byte, 0, maxGossipSize)
	return &x
}}

// BuildMsgIdFn builds a generic message ID function for gossipsub that can handle compressed payloads,
// mirroring the eth2 p2p gossip spec.
func BuildMsgIdFn(cfg *rollup.Config) pubsub.MsgIdFunction {
	return func(pmsg *pb.Message) string {
		valid := false
		var data []byte
		// If it's a valid compressed snappy data, then hash the uncompressed contents.
		// The validator can throw away the message later when recognized as invalid,
		// and the unique hash helps detect duplicates.
		dLen, err := snappy.DecodedLen(pmsg.Data)
		if err == nil && dLen <= maxGossipSize {
			res := msgBufPool.Get().(*[]byte)
			defer msgBufPool.Put(res)
			if data, err = snappy.Decode((*res)[:cap(*res)], pmsg.Data); err == nil {
				if cap(data) > cap(*res) {
					// if we ended up growing the slice capacity, fine, keep the larger one.
					*res = data[:cap(data)]
				}
				valid = true
			}
		}
		if data == nil {
			data = pmsg.Data
		}
		h := sha256.New()
		if valid {
			h.Write(MessageDomainValidSnappy[:])
		} else {
			h.Write(MessageDomainInvalidSnappy[:])
		}
		// The chain ID is part of the gossip topic, making the msg id unique
		topic := pmsg.GetTopic()
		var topicLen [8]byte
		binary.LittleEndian.PutUint64(topicLen[:], uint64(len(topic)))
		h.Write(topicLen[:])
		h.Write([]byte(topic))
		h.Write(data)
		// the message ID is shortened to save space, a lot of these may be gossiped.
		return string(h.Sum(nil)[:20])
	}
}

func (p *Config) ConfigureGossip(rollupCfg *rollup.Config) []pubsub.Option {
	params := BuildGlobalGossipParams(rollupCfg)

	// override with CLI changes
	params.D = p.MeshD
	params.Dlo = p.MeshDLo
	params.Dhi = p.MeshDHi
	params.Dlazy = p.MeshDLazy

	// in the future we may add more advanced options like scoring and PX / direct-mesh / episub
	return []pubsub.Option{
		pubsub.WithGossipSubParams(params),
		pubsub.WithFloodPublish(p.FloodPublish),
	}
}

func BuildGlobalGossipParams(cfg *rollup.Config) pubsub.GossipSubParams {
	params := pubsub.DefaultGossipSubParams()
	params.D = DefaultMeshD                    // topic stable mesh target count
	params.Dlo = DefaultMeshDlo                // topic stable mesh low watermark
	params.Dhi = DefaultMeshDhi                // topic stable mesh high watermark
	params.Dlazy = DefaultMeshDlazy            // gossip target
	params.HeartbeatInterval = gossipHeartbeat // interval of heartbeat
	params.FanoutTTL = 24 * time.Second        // ttl for fanout maps for topics we are not subscribed to but have published to
	params.HistoryLength = 12                  // number of windows to retain full messages in cache for IWANT responses
	params.HistoryGossip = 3                   // number of windows to gossip about

	return params
}

// NewGossipSub configures a new pubsub instance with the specified parameters.
// PubSub uses a GossipSubRouter as it's router under the hood.
func NewGossipSub(p2pCtx context.Context, h host.Host, cfg *rollup.Config, gossipConf GossipSetupConfigurables, scorer Scorer, m GossipMetricer, log log.Logger) (*pubsub.PubSub, error) {
	denyList, err := pubsub.NewTimeCachedBlacklist(30 * time.Second)
	if err != nil {
		return nil, err
	}
	gossipOpts := []pubsub.Option{
		pubsub.WithMaxMessageSize(maxGossipSize),
		pubsub.WithMessageIdFn(BuildMsgIdFn(cfg)),
		pubsub.WithNoAuthor(),
		pubsub.WithMessageSignaturePolicy(pubsub.StrictNoSign),
		pubsub.WithSubscriptionFilter(BuildSubscriptionFilter(cfg)),
		pubsub.WithValidateQueueSize(maxValidateQueue),
		pubsub.WithPeerOutboundQueueSize(maxOutboundQueue),
		pubsub.WithValidateThrottle(globalValidateThrottle),
		pubsub.WithSeenMessagesTTL(seenMessagesTTL),
		pubsub.WithPeerExchange(false),
		pubsub.WithBlacklist(denyList),
		pubsub.WithEventTracer(&gossipTracer{m: m}),
	}
	gossipOpts = append(gossipOpts, ConfigurePeerScoring(gossipConf, scorer, log)...)
	gossipOpts = append(gossipOpts, gossipConf.ConfigureGossip(cfg)...)
	return pubsub.NewGossipSub(p2pCtx, h, gossipOpts...)
}

func validationResultString(v pubsub.ValidationResult) string {
	switch v {
	case pubsub.ValidationAccept:
		return "ACCEPT"
	case pubsub.ValidationIgnore:
		return "IGNORE"
	case pubsub.ValidationReject:
		return "REJECT"
	default:
		return fmt.Sprintf("UNKNOWN_%d", v)
	}
}

func logValidationResult(self peer.ID, msg string, log log.Logger, fn pubsub.ValidatorEx) pubsub.ValidatorEx {
	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		res := fn(ctx, id, message)
		var src any
		src = id
		if id == self {
			src = "self"
		}
		log.Debug(msg, "result", validationResultString(res), "from", src)
		return res
	}
}

func guardGossipValidator(log log.Logger, fn pubsub.ValidatorEx) pubsub.ValidatorEx {
	return func(ctx context.Context, id peer.ID, message *pubsub.Message) (result pubsub.ValidationResult) {
		defer func() {
			if err := recover(); err != nil {
				log.Error("gossip validation panic", "err", err, "peer", id)
				result = pubsub.ValidationReject
			}
		}()
		return fn(ctx, id, message)
	}
}

type seenBlocks struct {
	sync.Mutex
	blockHashes []common.Hash
}

// hasSeen checks if the hash has been marked as seen, and how many have been seen.
func (sb *seenBlocks) hasSeen(h common.Hash) (count int, hasSeen bool) {
	sb.Lock()
	defer sb.Unlock()
	for _, prev := range sb.blockHashes {
		if prev == h {
			return len(sb.blockHashes), true
		}
	}
	return len(sb.blockHashes), false
}

// markSeen marks the block hash as seen
func (sb *seenBlocks) markSeen(h common.Hash) {
	sb.Lock()
	defer sb.Unlock()
	sb.blockHashes = append(sb.blockHashes, h)
}

type NewFragVersion int

const (
	NewFragV0 NewFragVersion = iota
)

type SealFragVersion int

const (
	SealFragV0 SealFragVersion = iota
)

type EnvVersion int

const (
	EnvV0 EnvVersion = iota
)

func BuildNewFragValidator(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, newFragVersion NewFragVersion) pubsub.ValidatorEx {
	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		var signedFrag eth.SignedNewFrag

		data := message.GetData()
		if err := signedFrag.UnmarshalSSZ(uint32(len(data)), bytes.NewReader(data)); err != nil {
			log.Warn("invalid signedFrag payload", "err", err, "peer", id)
			return pubsub.ValidationReject
		}

		msg := signedFrag.Frag.Root()

		expectedSigner := runCfg.CurrentGateway()

		if result := verifyGatewaySignature(log, signedFrag.Signature[:], msg[:], expectedSigner); result != pubsub.ValidationAccept {
			return result
		}

		message.ValidatorData = &signedFrag
		return pubsub.ValidationAccept
	}
}

func BuildSealFragValidator(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, sealFragVersion SealFragVersion) pubsub.ValidatorEx {
	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		var signedSeal eth.SignedSeal

		data := message.GetData()
		if err := signedSeal.UnmarshalSSZ(uint32(len(data)), bytes.NewReader(data)); err != nil {
			log.Warn("invalid signedSeal payload", "err", err, "peer", id)
			return pubsub.ValidationReject
		}

		msg := signedSeal.Seal.Root()

		expectedSigner := runCfg.CurrentGateway()

		if result := verifyGatewaySignature(log, signedSeal.Signature[:], msg[:], expectedSigner); result != pubsub.ValidationAccept {
			return result
		}

		message.ValidatorData = &signedSeal
		return pubsub.ValidationAccept
	}
}

func BuildEnvValidator(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, envVersion EnvVersion) pubsub.ValidatorEx {
	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		var signedEnv eth.SignedEnv

		data := message.GetData()
		if err := signedEnv.UnmarshalSSZ(uint32(len(data)), bytes.NewReader(data)); err != nil {
			log.Warn("invalid signedEnv payload", "err", err, "peer", id)
			return pubsub.ValidationReject
		}

		msg := signedEnv.Env.Root()

		expectedSigner := runCfg.CurrentGateway()

		if result := verifyGatewaySignature(log, signedEnv.Signature[:], msg[:], expectedSigner); result != pubsub.ValidationAccept {
			return result
		}

		message.ValidatorData = &signedEnv
		return pubsub.ValidationAccept
	}
}

func verifyGatewaySignature(log log.Logger, signatureBytes []byte, messageBytes []byte, expectedSigner common.Address) pubsub.ValidationResult {
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

	log.Info("gateway signature is valid for P2P message", "addr", msgSigner)

	return pubsub.ValidationAccept
}

func BuildBlocksValidator(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, blockVersion eth.BlockVersion) pubsub.ValidatorEx {

	// Seen block hashes per block height
	// uint64 -> *seenBlocks
	blockHeightLRU, err := lru.New[uint64, *seenBlocks](1000)
	if err != nil {
		panic(fmt.Errorf("failed to set up block height LRU cache: %w", err))
	}

	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		// [REJECT] if the compression is not valid
		outLen, err := snappy.DecodedLen(message.Data)
		if err != nil {
			log.Warn("invalid snappy compression length data", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		if outLen > maxGossipSize {
			log.Warn("possible snappy zip bomb, decoded length is too large", "decoded_length", outLen, "peer", id)
			return pubsub.ValidationReject
		}
		if outLen < minGossipSize {
			log.Warn("rejecting undersized gossip payload")
			return pubsub.ValidationReject
		}

		res := msgBufPool.Get().(*[]byte)
		defer msgBufPool.Put(res)
		data, err := snappy.Decode((*res)[:cap(*res)], message.Data)
		if err != nil {
			log.Warn("invalid snappy compression", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		// if we ended up growing the slice capacity, fine, keep the larger one.
		if cap(data) > cap(*res) {
			*res = data[:cap(data)]
		}

		// message starts with compact-encoding secp256k1 encoded signature
		signatureBytes, payloadBytes := data[:65], data[65:]

		// [REJECT] if the signature by the sequencer is not valid
		result := verifyBlockSignature(log, cfg, runCfg, id, signatureBytes, payloadBytes)
		if result != pubsub.ValidationAccept {
			return result
		}

		var envelope eth.ExecutionPayloadEnvelope

		// [REJECT] if the block encoding is not valid
		if blockVersion == eth.BlockV3 {
			if err := envelope.UnmarshalSSZ(uint32(len(payloadBytes)), bytes.NewReader(payloadBytes)); err != nil {
				log.Warn("invalid envelope payload", "err", err, "peer", id)
				return pubsub.ValidationReject
			}
		} else {
			var payload eth.ExecutionPayload
			if err := payload.UnmarshalSSZ(blockVersion, uint32(len(payloadBytes)), bytes.NewReader(payloadBytes)); err != nil {
				log.Warn("invalid execution payload", "err", err, "peer", id)
				return pubsub.ValidationReject
			}
			envelope = eth.ExecutionPayloadEnvelope{ExecutionPayload: &payload}
		}

		payload := envelope.ExecutionPayload

		// rounding down to seconds is fine here.
		now := uint64(time.Now().Unix())

		// [REJECT] if the `payload.timestamp` is older than 60 seconds in the past
		if uint64(payload.Timestamp) < now-60 {
			log.Warn("payload is too old", "timestamp", uint64(payload.Timestamp))
			return pubsub.ValidationReject
		}

		// [REJECT] if the `payload.timestamp` is more than 5 seconds into the future
		if uint64(payload.Timestamp) > now+5 {
			log.Warn("payload is too new", "timestamp", uint64(payload.Timestamp))
			return pubsub.ValidationReject
		}

		// [REJECT] if the `block_hash` in the `payload` is not valid
		if actual, ok := envelope.CheckBlockHash(); !ok {
			log.Warn("payload has bad block hash", "bad_hash", payload.BlockHash.String(), "actual", actual.String())
			return pubsub.ValidationReject
		}

		// [REJECT] if a V1 Block has withdrawals
		if !blockVersion.HasWithdrawals() && payload.Withdrawals != nil {
			log.Warn("payload is on v1 topic, but has withdrawals", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		// [REJECT] if a >= V2 Block does not have withdrawals
		if blockVersion.HasWithdrawals() && payload.Withdrawals == nil {
			log.Warn("payload is on v2/v3 topic, but does not have withdrawals", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		// [REJECT] if a >= V2 Block has non-empty withdrawals
		if blockVersion.HasWithdrawals() && len(*payload.Withdrawals) != 0 {
			log.Warn("payload is on v2/v3 topic, but has non-empty withdrawals", "bad_hash", payload.BlockHash.String(), "withdrawal_count", len(*payload.Withdrawals))
			return pubsub.ValidationReject
		}

		// [REJECT] if the block is on a topic <= V2 and has a blob gas value set
		if !blockVersion.HasBlobProperties() && payload.BlobGasUsed != nil {
			log.Warn("payload is on v1/v2 topic, but has blob gas used", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		// [REJECT] if the block is on a topic <= V2 and has an excess blob gas value set
		if !blockVersion.HasBlobProperties() && payload.ExcessBlobGas != nil {
			log.Warn("payload is on v1/v2 topic, but has excess blob gas", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		if blockVersion.HasBlobProperties() {
			// [REJECT] if the block is on a topic >= V3 and has a blob gas used value that is not zero
			if payload.BlobGasUsed == nil || *payload.BlobGasUsed != 0 {
				log.Warn("payload is on v3 topic, but has non-zero blob gas used", "bad_hash", payload.BlockHash.String(), "blob_gas_used", payload.BlobGasUsed)
				return pubsub.ValidationReject
			}

			// [REJECT] if the block is on a topic >= V3 and has an excess blob gas value that is not zero
			if payload.ExcessBlobGas == nil || *payload.ExcessBlobGas != 0 {
				log.Warn("payload is on v3 topic, but has non-zero excess blob gas", "bad_hash", payload.BlockHash.String(), "excess_blob_gas", payload.ExcessBlobGas)
				return pubsub.ValidationReject
			}
		}

		// [REJECT] if the block is on a topic >= V3 and the parent beacon block root is nil
		if blockVersion.HasParentBeaconBlockRoot() && envelope.ParentBeaconBlockRoot == nil {
			log.Warn("payload is on v3 topic, but has nil parent beacon block root", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		seen, ok := blockHeightLRU.Get(uint64(payload.BlockNumber))
		if !ok {
			seen = new(seenBlocks)
			blockHeightLRU.Add(uint64(payload.BlockNumber), seen)
		}

		if count, hasSeen := seen.hasSeen(payload.BlockHash); count > 5 {
			// [REJECT] if more than 5 blocks have been seen with the same block height
			log.Warn("seen too many different blocks at same height", "height", payload.BlockNumber)
			return pubsub.ValidationReject
		} else if hasSeen {
			// [IGNORE] if the block has already been seen
			log.Warn("validated already seen message again")
			return pubsub.ValidationIgnore
		}

		// mark it as seen. (note: with concurrent validation more than 5 blocks may be marked as seen still,
		// but validator concurrency is limited anyway)
		seen.markSeen(payload.BlockHash)

		// remember the decoded payload for later usage in topic subscriber.
		message.ValidatorData = &envelope
		return pubsub.ValidationAccept
	}
}

func verifyBlockSignature(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, id peer.ID, signatureBytes []byte, payloadBytes []byte) pubsub.ValidationResult {
	signingHash, err := BlockSigningHash(cfg, payloadBytes)
	if err != nil {
		log.Warn("failed to compute block signing hash", "err", err, "peer", id)
		return pubsub.ValidationReject
	}

	pub, err := crypto.SigToPub(signingHash[:], signatureBytes)
	if err != nil {
		log.Warn("invalid block signature", "err", err, "peer", id)
		return pubsub.ValidationReject
	}
	addr := crypto.PubkeyToAddress(*pub)

	// In the future we may load & validate block metadata before checking the signature.
	// And then check the signer based on the metadata, to support e.g. multiple p2p signers at the same time.
	// For now we only have one signer at a time and thus check the address directly.
	// This means we may drop old payloads upon key rotation,
	// but this can be recovered from like any other missed unsafe payload.
	if expected := runCfg.P2PSequencerAddress(); expected == (common.Address{}) {
		log.Warn("no configured p2p sequencer address, ignoring gossiped block", "peer", id, "addr", addr)
		return pubsub.ValidationIgnore
	} else if addr != expected {
		log.Warn("unexpected block author", "err", err, "peer", id, "addr", addr, "expected", expected)
		return pubsub.ValidationReject
	}
	return pubsub.ValidationAccept
}

type GossipIn interface {
	OnUnsafeL2Payload(ctx context.Context, from peer.ID, msg *eth.ExecutionPayloadEnvelope) error
	OnNewFrag(ctx context.Context, from peer.ID, msg *eth.SignedNewFrag) error
	OnSealFrag(ctx context.Context, from peer.ID, msg *eth.SignedSeal) error
	OnEnv(ctx context.Context, from peer.ID, msg *eth.SignedEnv) error
}

type GossipTopicInfo interface {
	AllBlockTopicsPeers() []peer.ID
	BlocksTopicV1Peers() []peer.ID
	BlocksTopicV2Peers() []peer.ID
	BlocksTopicV3Peers() []peer.ID
	NewFragTopicV0Peers() []peer.ID
	SealFragTopicV0Peers() []peer.ID
	EnvTopicV0Peers() []peer.ID
}

type GossipOut interface {
	GossipTopicInfo
	PublishL2Payload(ctx context.Context, msg *eth.ExecutionPayloadEnvelope, signer Signer) error
	PublishNewFrag(ctx context.Context, from peer.ID, signedFrag *eth.SignedNewFrag) error
	PublishSealFrag(ctx context.Context, from peer.ID, signedSeal *eth.SignedSeal) error
	PublishEnv(ctx context.Context, from peer.ID, env *eth.SignedEnv) error
	Close() error
}

type newFragTopic struct {
	// newFragV0 topic, main handle on newFragV0 gossip
	topic *pubsub.Topic
	// newFragV0 events handler, to be cancelled before closing the newFragV0 topic.
	events *pubsub.TopicEventHandler
	// newFragV0 subscriptions, to be cancelled before closing newFragV0 topic.
	sub *pubsub.Subscription
}

type sealFragTopic struct {
	// sealFragV0 topic, main handle on sealFragV0 gossip
	topic *pubsub.Topic
	// sealFragV0 events handler, to be cancelled before closing the sealFragV0 topic.
	events *pubsub.TopicEventHandler
	// sealFragV0 subscriptions, to be cancelled before closing sealFragV0 topic.
	sub *pubsub.Subscription
}

type envTopic struct {
	// envV0 topic, main handle on envV0 gossip
	topic *pubsub.Topic
	// envV0 events handler, to be cancelled before closing the envV0 topic.
	events *pubsub.TopicEventHandler
	// envV0 subscriptions, to be cancelled before closing envV0 topic.
	sub *pubsub.Subscription
}

type blockTopic struct {
	// blocks topic, main handle on block gossip
	topic *pubsub.Topic
	// block events handler, to be cancelled before closing the blocks topic.
	events *pubsub.TopicEventHandler
	// block subscriptions, to be cancelled before closing blocks topic.
	sub *pubsub.Subscription
}

func (bt *blockTopic) Close() error {
	bt.events.Cancel()
	bt.sub.Cancel()
	return bt.topic.Close()
}

func (nft *newFragTopic) Close() error {
	nft.events.Cancel()
	nft.sub.Cancel()
	return nft.topic.Close()
}

func (nst *sealFragTopic) Close() error {
	nst.events.Cancel()
	nst.sub.Cancel()
	return nst.topic.Close()
}

func (et *envTopic) Close() error {
	et.events.Cancel()
	et.sub.Cancel()
	return et.topic.Close()
}

type publisher struct {
	log log.Logger
	cfg *rollup.Config

	// p2pCancel cancels the downstream gossip event-handling functions, independent of the sources.
	// A closed gossip event source (event handler or subscription) does not stop any open event iteration,
	// thus we have to stop it ourselves this way.
	p2pCancel context.CancelFunc

	blocksV1 *blockTopic
	blocksV2 *blockTopic
	blocksV3 *blockTopic

	newFragV0  *newFragTopic
	sealFragV0 *sealFragTopic
	envV0      *envTopic

	runCfg GossipRuntimeConfig
}

var _ GossipOut = (*publisher)(nil)

func combinePeers(allPeers ...[]peer.ID) []peer.ID {
	var seen = make(map[peer.ID]bool)
	var res []peer.ID
	for _, peers := range allPeers {
		for _, p := range peers {
			if _, ok := seen[p]; ok {
				continue
			}
			res = append(res, p)
			seen[p] = true
		}
	}
	return res
}

func (p *publisher) AllBlockTopicsPeers() []peer.ID {
	return combinePeers(p.BlocksTopicV1Peers(), p.BlocksTopicV2Peers(), p.BlocksTopicV3Peers())
}

func (p *publisher) BlocksTopicV1Peers() []peer.ID {
	return p.blocksV1.topic.ListPeers()
}

func (p *publisher) BlocksTopicV2Peers() []peer.ID {
	return p.blocksV2.topic.ListPeers()
}

func (p *publisher) BlocksTopicV3Peers() []peer.ID {
	return p.blocksV3.topic.ListPeers()
}

func (p *publisher) NewFragTopicV0Peers() []peer.ID {
	return p.newFragV0.topic.ListPeers()
}

func (p *publisher) SealFragTopicV0Peers() []peer.ID {
	return p.sealFragV0.topic.ListPeers()
}

func (p *publisher) EnvTopicV0Peers() []peer.ID {
	return p.envV0.topic.ListPeers()
}

func (p *publisher) PublishL2Payload(ctx context.Context, envelope *eth.ExecutionPayloadEnvelope, signer Signer) error {
	res := msgBufPool.Get().(*[]byte)
	buf := bytes.NewBuffer((*res)[:0])
	defer func() {
		*res = buf.Bytes()
		defer msgBufPool.Put(res)
	}()

	buf.Write(make([]byte, 65))

	if envelope.ParentBeaconBlockRoot != nil {
		if _, err := envelope.MarshalSSZ(buf); err != nil {
			return fmt.Errorf("failed to encoded execution payload envelope to publish: %w", err)
		}
	} else {
		if _, err := envelope.ExecutionPayload.MarshalSSZ(buf); err != nil {
			return fmt.Errorf("failed to encoded execution payload to publish: %w", err)
		}
	}

	data := buf.Bytes()
	payloadData := data[65:]
	sig, err := signer.Sign(ctx, SigningDomainBlocksV1, p.cfg.L2ChainID, payloadData)
	if err != nil {
		return fmt.Errorf("failed to sign execution payload with signer: %w", err)
	}
	copy(data[:65], sig[:])

	// compress the full message
	// This also copies the data, freeing up the original buffer to go back into the pool
	out := snappy.Encode(nil, data)

	if p.cfg.IsEcotone(uint64(envelope.ExecutionPayload.Timestamp)) {
		return p.blocksV3.topic.Publish(ctx, out)
	} else if p.cfg.IsCanyon(uint64(envelope.ExecutionPayload.Timestamp)) {
		return p.blocksV2.topic.Publish(ctx, out)
	} else {
		return p.blocksV1.topic.Publish(ctx, out)
	}
}

func (p *publisher) PublishNewFrag(ctx context.Context, from peer.ID, signedFrag *eth.SignedNewFrag) error {
	buf := new(bytes.Buffer)
	signedFrag.MarshalSSZ(buf)

	return p.newFragV0.topic.Publish(ctx, buf.Bytes())
}

func (p *publisher) PublishSealFrag(ctx context.Context, from peer.ID, signedSeal *eth.SignedSeal) error {
	buf := new(bytes.Buffer)
	signedSeal.MarshalSSZ(buf)

	return p.sealFragV0.topic.Publish(ctx, buf.Bytes())
}

func (p *publisher) PublishEnv(ctx context.Context, from peer.ID, signedEnv *eth.SignedEnv) error {
	buf := new(bytes.Buffer)
	signedEnv.MarshalSSZ(buf)

	return p.envV0.topic.Publish(ctx, buf.Bytes())
}

func (p *publisher) Close() error {
	p.p2pCancel()
	e1 := p.blocksV1.Close()
	e2 := p.blocksV2.Close()
	return errors.Join(e1, e2)
}

func JoinGossip(self peer.ID, ps *pubsub.PubSub, log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, gossipIn GossipIn) (GossipOut, error) {
	p2pCtx, p2pCancel := context.WithCancel(context.Background())

	v1Logger := log.New("topic", "blocksV1")
	blocksV1Validator := guardGossipValidator(log, logValidationResult(self, "validated blockv1", v1Logger, BuildBlocksValidator(v1Logger, cfg, runCfg, eth.BlockV1)))
	blocksV1, err := newBlockTopic(p2pCtx, blocksTopicV1(cfg), ps, v1Logger, gossipIn, blocksV1Validator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup blocks v1 p2p: %w", err)
	}

	v2Logger := log.New("topic", "blocksV2")
	blocksV2Validator := guardGossipValidator(log, logValidationResult(self, "validated blockv2", v2Logger, BuildBlocksValidator(v2Logger, cfg, runCfg, eth.BlockV2)))
	blocksV2, err := newBlockTopic(p2pCtx, blocksTopicV2(cfg), ps, v2Logger, gossipIn, blocksV2Validator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup blocks v2 p2p: %w", err)
	}

	v3Logger := log.New("topic", "blocksV3")
	blocksV3Validator := guardGossipValidator(log, logValidationResult(self, "validated blockv3", v3Logger, BuildBlocksValidator(v3Logger, cfg, runCfg, eth.BlockV3)))
	blocksV3, err := newBlockTopic(p2pCtx, blocksTopicV3(cfg), ps, v3Logger, gossipIn, blocksV3Validator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup blocks v3 p2p: %w", err)
	}

	newFragV0Logger := log.New("topic", "newFragV0")
	newFragV0Validator := guardGossipValidator(log, logValidationResult(self, "validated newFragV0", newFragV0Logger, BuildNewFragValidator(newFragV0Logger, cfg, runCfg, NewFragV0)))
	newFragV0, err := newNewFragTopic(p2pCtx, newFragV0(cfg), ps, newFragV0Logger, gossipIn, newFragV0Validator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup newFragV0 p2p: %w", err)
	}

	sealFragV0Logger := log.New("topic", "sealFragV0")
	sealFragV0Validator := guardGossipValidator(log, logValidationResult(self, "validated sealFragV0", sealFragV0Logger, BuildSealFragValidator(sealFragV0Logger, cfg, runCfg, SealFragV0)))
	sealFragV0, err := sealFragFragTopic(p2pCtx, sealFragV0(cfg), ps, sealFragV0Logger, gossipIn, sealFragV0Validator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup sealFragV0 p2p: %w", err)
	}

	envV0Logger := log.New("topic", "envV0")
	envV0Validator := guardGossipValidator(log, logValidationResult(self, "validated envV0", envV0Logger, BuildEnvValidator(envV0Logger, cfg, runCfg, EnvV0)))
	envV0, err := newEnvTopic(p2pCtx, envV0(cfg), ps, envV0Logger, gossipIn, envV0Validator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup envV0 p2p: %w", err)
	}

	return &publisher{
		log:        log,
		cfg:        cfg,
		p2pCancel:  p2pCancel,
		blocksV1:   blocksV1,
		blocksV2:   blocksV2,
		blocksV3:   blocksV3,
		newFragV0:  newFragV0,
		sealFragV0: sealFragV0,
		envV0:      envV0,
		runCfg:     runCfg,
	}, nil
}

func newNewFragTopic(ctx context.Context, topicId string, ps *pubsub.PubSub, log log.Logger, gossipIn GossipIn, validator pubsub.ValidatorEx) (*newFragTopic, error) {
	err := ps.RegisterTopicValidator(topicId,
		validator,
		pubsub.WithValidatorTimeout(3*time.Second),
		pubsub.WithValidatorConcurrency(4))

	if err != nil {
		return nil, fmt.Errorf("failed to register gossip topic: %w", err)
	}

	newFragsTopic, err := ps.Join(topicId)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip topic: %w", err)
	}

	newFragsTopicEvents, err := newFragsTopic.EventHandler()
	if err != nil {
		return nil, fmt.Errorf("failed to create new frags gossip topic handler: %w", err)
	}

	go LogTopicEvents(ctx, log, newFragsTopicEvents)

	subscription, err := newFragsTopic.Subscribe()
	if err != nil {
		err = errors.Join(err, newFragsTopic.Close())
		return nil, fmt.Errorf("failed to subscribe to new frags gossip topic: %w", err)
	}

	subscriber := MakeSubscriber(log, NewFragHandler(gossipIn.OnNewFrag))
	go subscriber(ctx, subscription)

	return &newFragTopic{
		topic:  newFragsTopic,
		events: newFragsTopicEvents,
		sub:    subscription,
	}, nil
}

func sealFragFragTopic(ctx context.Context, topicId string, ps *pubsub.PubSub, log log.Logger, gossipIn GossipIn, validator pubsub.ValidatorEx) (*sealFragTopic, error) {
	err := ps.RegisterTopicValidator(topicId,
		validator,
		pubsub.WithValidatorTimeout(3*time.Second),
		pubsub.WithValidatorConcurrency(4))

	if err != nil {
		return nil, fmt.Errorf("failed to register gossip topic: %w", err)
	}

	sealFragsTopic, err := ps.Join(topicId)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip topic: %w", err)
	}

	sealFragsTopicEvents, err := sealFragsTopic.EventHandler()
	if err != nil {
		return nil, fmt.Errorf("failed to create new seals gossip topic handler: %w", err)
	}

	go LogTopicEvents(ctx, log, sealFragsTopicEvents)

	subscription, err := sealFragsTopic.Subscribe()
	if err != nil {
		err = errors.Join(err, sealFragsTopic.Close())
		return nil, fmt.Errorf("failed to subscribe to new seals gossip topic: %w", err)
	}

	subscriber := MakeSubscriber(log, SealFragHandler(gossipIn.OnSealFrag))
	go subscriber(ctx, subscription)

	return &sealFragTopic{
		topic:  sealFragsTopic,
		events: sealFragsTopicEvents,
		sub:    subscription,
	}, nil
}

func newEnvTopic(ctx context.Context, topicId string, ps *pubsub.PubSub, log log.Logger, gossipIn GossipIn, validator pubsub.ValidatorEx) (*envTopic, error) {
	err := ps.RegisterTopicValidator(topicId,
		validator,
		pubsub.WithValidatorTimeout(3*time.Second),
		pubsub.WithValidatorConcurrency(4))

	if err != nil {
		return nil, fmt.Errorf("failed to register gossip topic: %w", err)
	}

	envsTopic, err := ps.Join(topicId)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip topic: %w", err)
	}

	envTopicEvents, err := envsTopic.EventHandler()
	if err != nil {
		return nil, fmt.Errorf("failed to create new env gossip topic handler: %w", err)
	}

	go LogTopicEvents(ctx, log, envTopicEvents)

	subscription, err := envsTopic.Subscribe()
	if err != nil {
		err = errors.Join(err, envsTopic.Close())
		return nil, fmt.Errorf("failed to subscribe to new env gossip topic: %w", err)
	}

	subscriber := MakeSubscriber(log, EnvHandler(gossipIn.OnEnv))
	go subscriber(ctx, subscription)

	return &envTopic{
		topic:  envsTopic,
		events: envTopicEvents,
		sub:    subscription,
	}, nil
}

func newBlockTopic(ctx context.Context, topicId string, ps *pubsub.PubSub, log log.Logger, gossipIn GossipIn, validator pubsub.ValidatorEx) (*blockTopic, error) {
	err := ps.RegisterTopicValidator(topicId,
		validator,
		pubsub.WithValidatorTimeout(3*time.Second),
		pubsub.WithValidatorConcurrency(4))

	if err != nil {
		return nil, fmt.Errorf("failed to register gossip topic: %w", err)
	}

	blocksTopic, err := ps.Join(topicId)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip topic: %w", err)
	}

	blocksTopicEvents, err := blocksTopic.EventHandler()
	if err != nil {
		return nil, fmt.Errorf("failed to create blocks gossip topic handler: %w", err)
	}

	go LogTopicEvents(ctx, log, blocksTopicEvents)

	subscription, err := blocksTopic.Subscribe()
	if err != nil {
		err = errors.Join(err, blocksTopic.Close())
		return nil, fmt.Errorf("failed to subscribe to blocks gossip topic: %w", err)
	}

	subscriber := MakeSubscriber(log, BlocksHandler(gossipIn.OnUnsafeL2Payload))
	go subscriber(ctx, subscription)

	return &blockTopic{
		topic:  blocksTopic,
		events: blocksTopicEvents,
		sub:    subscription,
	}, nil
}

type TopicSubscriber func(ctx context.Context, sub *pubsub.Subscription)
type MessageHandler func(ctx context.Context, from peer.ID, msg any) error

func NewFragHandler(onNewFrag func(ctx context.Context, from peer.ID, msg *eth.SignedNewFrag) error) MessageHandler {
	return func(ctx context.Context, from peer.ID, msg any) error {
		frag, ok := msg.(*eth.SignedNewFrag)
		if !ok {
			return fmt.Errorf("expected topic validator to parse and validate data into frag, but got %T", msg)
		}
		return onNewFrag(ctx, from, frag)
	}
}

func SealFragHandler(onSealFrag func(ctx context.Context, from peer.ID, msg *eth.SignedSeal) error) MessageHandler {
	return func(ctx context.Context, from peer.ID, msg any) error {
		seal, ok := msg.(*eth.SignedSeal)
		if !ok {
			return fmt.Errorf("expected topic validator to parse and validate data into seal, but got %T", msg)
		}
		return onSealFrag(ctx, from, seal)
	}
}

func EnvHandler(onEnv func(ctx context.Context, from peer.ID, msg *eth.SignedEnv) error) MessageHandler {
	return func(ctx context.Context, from peer.ID, msg any) error {
		env, ok := msg.(*eth.SignedEnv)
		if !ok {
			return fmt.Errorf("expected topic validator to parse and validate data into env, but got %T", msg)
		}
		return onEnv(ctx, from, env)
	}
}

func BlocksHandler(onBlock func(ctx context.Context, from peer.ID, msg *eth.ExecutionPayloadEnvelope) error) MessageHandler {
	return func(ctx context.Context, from peer.ID, msg any) error {
		payload, ok := msg.(*eth.ExecutionPayloadEnvelope)
		if !ok {
			return fmt.Errorf("expected topic validator to parse and validate data into execution payload, but got %T", msg)
		}
		return onBlock(ctx, from, payload)
	}
}

func MakeSubscriber(log log.Logger, msgHandler MessageHandler) TopicSubscriber {
	return func(ctx context.Context, sub *pubsub.Subscription) {
		topicLog := log.New("topic", sub.Topic())
		for {
			msg, err := sub.Next(ctx)
			if err != nil { // ctx was closed, or subscription was closed
				topicLog.Debug("stopped subscriber")
				return
			}
			if msg.ValidatorData == nil {
				topicLog.Error("gossip message with no data", "from", msg.ReceivedFrom)
				continue
			}
			if err := msgHandler(ctx, msg.ReceivedFrom, msg.ValidatorData); err != nil {
				topicLog.Error("failed to process gossip message", "err", err)
			}
		}
	}
}

func LogTopicEvents(ctx context.Context, log log.Logger, evHandler *pubsub.TopicEventHandler) {
	for {
		ev, err := evHandler.NextPeerEvent(ctx)
		if err != nil {
			return // ctx closed
		}
		switch ev.Type {
		case pubsub.PeerJoin:
			log.Debug("peer joined topic", "peer", ev.Peer)
		case pubsub.PeerLeave:
			log.Debug("peer left topic", "peer", ev.Peer)
		default:
			log.Warn("unrecognized topic event", "ev", ev)
		}
	}
}

type gossipTracer struct {
	m GossipMetricer
}

func (g *gossipTracer) Trace(evt *pb.TraceEvent) {
	if g.m != nil {
		g.m.RecordGossipEvent(int32(*evt.Type))
	}
}
