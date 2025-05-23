use alloy_consensus::proofs::ordered_trie_root_with_encoder;
use alloy_eips::eip2718::Encodable2718;
use alloy_primitives::{Bloom, U256};
use bop_common::{p2p::FragV0, time::Instant, transaction::SimulatedTx};
use revm_primitives::{Bytes, B256};

use super::{sorting_data::SortingTelemetry, SortingData};
use crate::context::SequencerContext;

/// Sequence of frags applied on the last block
#[derive(Clone, Debug)]
pub struct FragSequence {
    pub start_t: Instant,
    pub gas_remaining: u64,
    pub gas_used: u64,
    pub payment: U256,
    pub txs: Vec<SimulatedTx>,
    /// Next frag index
    pub next_seq: u64,
    /// Block number and timestamp shared by all frags of this sequence
    block_number: u64,
    block_timestamp: u64,

    pub sorting_telemetry: SortingTelemetry,
}
impl FragSequence {
    pub fn new(gas_remaining: u64, block_number: u64, block_timestamp: u64) -> Self {
        Self {
            start_t: Instant::now(),
            gas_remaining,
            gas_used: 0,
            payment: U256::ZERO,
            txs: vec![],
            block_number,
            block_timestamp,
            next_seq: 0,
            sorting_telemetry: Default::default(),
        }
    }

    pub fn set_gas_limit(&mut self, gas_limit: u64) {
        self.gas_remaining = gas_limit;
    }

    pub fn apply_sorted_frag<Db>(&mut self, in_sort: SortingData<Db>, ctx: &mut SequencerContext<Db>) -> FragV0 {
        let gas_used = in_sort.gas_used();
        self.gas_remaining -= gas_used;
        self.payment += in_sort.payment();

        let msg = FragV0::new(self.block_number, self.next_seq, in_sort.txs.iter().map(|tx| tx.tx.as_ref()), false);
        for tx in in_sort.txs {
            self.gas_used += tx.gas_used();
            let receipt = tx.op_tx_receipt(
                self.gas_used,
                self.block_number,
                self.block_timestamp,
                ctx.base_fee(),
                self.txs.len() as u64,
            );
            ctx.shared_state.insert_confirmed_tx(tx.tx.tx.clone(), receipt);
            self.txs.push(tx);
        }

        self.next_seq += 1;
        self.sorting_telemetry += in_sort.telemetry;
        msg
    }

    /// Returns encoded_2718 txs, transactions root, receipts root, and receipts bloom
    pub fn encoded_txs_roots_bloom(&self, canyon_active: bool) -> (Vec<Bytes>, B256, B256, Bloom) {
        let mut receipts = Vec::with_capacity(self.txs.len());
        let mut transactions = Vec::with_capacity(self.txs.len());
        let mut logs_bloom = Bloom::ZERO;
        let mut gas_used = 0;
        for t in self.txs.iter() {
            gas_used += t.gas_used();
            let receipt = t.receipt(gas_used, canyon_active);
            logs_bloom |= receipt.logs_bloom;
            receipts.push(receipt);
            transactions.push(t.tx.encode());
        }

        let receipts_root = ordered_trie_root_with_encoder(&receipts, |r, buf| {
            r.encode_2718(buf);
        });
        debug_assert_eq!(
            self.gas_used, gas_used,
            "somehow gas used tracked by frag seq is not identical to total gas used by txs"
        );

        let transactions_root = ordered_trie_root_with_encoder(&transactions, |tx, buf| *buf = tx.clone().into());
        (transactions, transactions_root, receipts_root, logs_bloom)
    }
}
#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use alloy_consensus::Signed;
    use alloy_eips::eip2718::Encodable2718;
    use alloy_primitives::Bytes;
    use alloy_provider::ProviderBuilder;
    use alloy_rpc_types::engine::PayloadAttributes;
    use bop_common::{
        communication::Spine, db::DBFrag, shared::SharedState, time::Duration, transaction::Transaction,
        utils::initialize_test_tracing,
    };
    use bop_db::AlloyDB;
    use op_alloy_consensus::{OpTxEnvelope, OpTypedTransaction};
    use op_alloy_rpc_types_engine::OpPayloadAttributes;
    use reqwest::Url;
    use reth_optimism_chainspec::BASE_SEPOLIA;
    use reth_optimism_evm::OpEvmConfig;
    use reth_primitives_traits::{Block, SignedTransaction};
    use tracing::level_filters::LevelFilter;

    use crate::{
        block_sync::fetch_blocks::fetch_block, context::SequencerContext, simulator::simulate_tx_inner,
        SequencerConfig, Simulator,
    };

    const ENV_RPC_URL: &str = "BASE_RPC_URL";
    const TEST_BASE_RPC_URL: &str = "https://base-rpc.publicnode.com";

    #[ignore = "Requires RPC calls"]
    #[test]
    fn test_block_seal_with_alloydb() {
        initialize_test_tracing(LevelFilter::INFO);

        let rt = Arc::new(tokio::runtime::Runtime::new().unwrap());

        // Get RPC URL from environment
        let rpc_url = std::env::var(ENV_RPC_URL).unwrap_or(TEST_BASE_RPC_URL.to_string());
        let rpc_url = Url::parse(&rpc_url).unwrap();
        tracing::info!("RPC URL: {}", rpc_url);

        // Create the block executor.
        let evm_config = OpEvmConfig::new(BASE_SEPOLIA.clone());

        // Fetch the block from the RPC.
        let provider = ProviderBuilder::new().network().on_http(rpc_url.clone());
        let block_number = 21803240;
        let block = rt.block_on(async { fetch_block(block_number, &provider).await });
        let previous_block = rt.block_on(async { fetch_block(block_number - 1, &provider).await });
        let header = block.block.header();
        let previous_header = previous_block.block.header();
        tracing::info!("Testing block header: {:?}", header);

        let config = SequencerConfig {
            frag_duration: Duration::from_millis(200),
            n_per_loop: 5,
            rpc_url: rpc_url.clone(),
            evm_config: evm_config.clone(),
            simulate_tof_in_pools: false,
            commit_sealed_frags_to_db: false,
        };

        // Create the alloydb.
        let client = ProviderBuilder::new().network().on_http(rpc_url);
        let alloy_db = AlloyDB::new(client, block.block.header.number, rt);

        let db_frag: DBFrag<AlloyDB> = alloy_db.clone().into();
        let sim_db = db_frag.clone();

        let shared_state = SharedState::new(db_frag.clone());

        // Setup channels for sim messaging
        let spine = Spine::default();
        let sim_connections = spine.to_connections("sim");

        let mut ctx: SequencerContext<AlloyDB> = SequencerContext::new(alloy_db.clone(), shared_state, config);
        ctx.parent_header = previous_header.clone();
        ctx.parent_hash = previous_block.hash_slow();
        ctx.base_fee = block.base_fee_per_gas.unwrap();

        let mut must_include_txs = Vec::with_capacity(10);
        let mut non_must_include_txs = Vec::with_capacity(block.block.body.transactions.len().saturating_sub(10));
        // Split into must include and non-must include txs
        // Note: for this test as assume the first 10 txs are must include txs, the rest are not
        for (index, signed_tx) in block.block.body.transactions.iter().enumerate() {
            let sender = signed_tx.recover_signer().unwrap();
            let typed_tx: &OpTypedTransaction = &signed_tx.transaction;
            let envelope: OpTxEnvelope = match typed_tx {
                OpTypedTransaction::Legacy(x) => {
                    Signed::new_unchecked(x.clone(), signed_tx.signature().clone(), *signed_tx.tx_hash()).into()
                }
                OpTypedTransaction::Eip2930(x) => {
                    Signed::new_unchecked(x.clone(), signed_tx.signature().clone(), *signed_tx.tx_hash()).into()
                }
                OpTypedTransaction::Eip1559(x) => {
                    Signed::new_unchecked(x.clone(), signed_tx.signature().clone(), *signed_tx.tx_hash()).into()
                }
                OpTypedTransaction::Eip7702(x) => {
                    Signed::new_unchecked(x.clone(), signed_tx.signature().clone(), *signed_tx.tx_hash()).into()
                }
                OpTypedTransaction::Deposit(x) => x.clone().into(),
            };

            let rlp_tx = Bytes::from(envelope.encoded_2718());
            if index < 10 {
                must_include_txs.push(rlp_tx);
            } else {
                let bop_tx = Arc::new(Transaction::new(envelope, sender, rlp_tx.into()));
                non_must_include_txs.push(bop_tx);
            }
        }

        // Apply must include txs as start state init
        let attributes = Box::new(OpPayloadAttributes {
            payload_attributes: PayloadAttributes {
                timestamp: block.timestamp,
                prev_randao: header.mix_hash,
                suggested_fee_recipient: header.beneficiary,
                withdrawals: None,
                parent_beacon_block_root: header.parent_beacon_block_root,
            },
            transactions: Some(must_include_txs),
            no_tx_pool: None,
            gas_limit: Some(block.gas_limit),
            eip_1559_params: Some(revm_primitives::FixedBytes::from_slice(&block.extra_data[1..9])),
        });

        let (mut seq, mut sorting_db) = ctx.start_sequencing(attributes, sim_connections.senders());

        // Apply non-must include txs using simulator
        let mut sim = Simulator::new(sim_db, &evm_config, 0);
        let (simulator_evm_block_params, _) = ctx.new_block_params();
        sim.update_evm_environments(simulator_evm_block_params);

        for tx in non_must_include_txs {
            let evm = &mut sim.evm_sorting;
            let db = sorting_db.state();

            let new_state = bop_common::db::State::new(db);
            let _ = std::mem::replace(evm.db_mut(), new_state);
            let result = simulate_tx_inner(tx, evm, true, true, true).unwrap();
            sorting_db.apply_tx(result);
        }

        // Apply the frag of non-must include txs
        let (_frag, _sorting_db) = ctx.seal_frag(sorting_db, &mut seq);

        // Seal the block
        let (_seal, payload) = ctx.seal_block(seq);
        assert_eq!(block.block.header.hash_slow(), payload.execution_payload.payload_inner.payload_inner.block_hash);
    }
}
