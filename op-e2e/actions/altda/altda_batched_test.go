package altda

import (
	"math/rand"
	"testing"

	"github.com/ethereum-optimism/optimism/op-e2e/config"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/event"

	"github.com/ethereum-optimism/optimism/op-e2e/actions/helpers"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"

	altda "github.com/ethereum-optimism/optimism/op-alt-da"
	"github.com/ethereum-optimism/optimism/op-alt-da/bindings"
	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils"
	"github.com/ethereum-optimism/optimism/op-service/sources"
	"github.com/ethereum-optimism/optimism/op-service/testlog"
)

// L2AltDA is a test harness for manipulating AltDA state.

type AltDAParamBatched func(p *e2eutils.TestParams)

// Same as altda_test.go, but with a batched batcher config
func NewL2AltDABatched(t helpers.Testing, params ...AltDAParamBatched) *L2AltDA {
	p := &e2eutils.TestParams{
		MaxSequencerDrift:   40,
		SequencerWindowSize: 12,
		ChannelTimeout:      12,
		L1BlockTime:         12,
		UseAltDA:            true,
		AllocType:           config.AllocTypeAltDA,
	}
	for _, apply := range params {
		apply(p)
	}
	log := testlog.Logger(t, log.LvlDebug)

	dp := e2eutils.MakeDeployParams(t, p)
	sd := e2eutils.Setup(t, dp, helpers.DefaultAlloc)

	require.True(t, sd.RollupCfg.AltDAEnabled())

	miner := helpers.NewL1Miner(t, log, sd.L1Cfg)
	l1Client := miner.EthClient()

	jwtPath := e2eutils.WriteDefaultJWT(t)
	engine := helpers.NewL2Engine(t, log, sd.L2Cfg, jwtPath)
	engCl := engine.EngineClient(t, sd.RollupCfg)

	storage := &altda.DAErrFaker{Client: altda.NewMockDAClient(log)}

	l1F, err := sources.NewL1Client(miner.RPCClient(), log, nil, sources.L1ClientDefaultConfig(sd.RollupCfg, false, sources.RPCKindBasic))
	require.NoError(t, err)

	altDACfg, err := sd.RollupCfg.GetOPAltDAConfig()
	require.NoError(t, err)

	daMgr := altda.NewAltDAWithStorage(log, altDACfg, storage, &altda.NoopMetrics{})

	sequencer := helpers.NewL2Sequencer(t, log, l1F, miner.BlobStore(), daMgr, engCl, sd.RollupCfg, 0)
	miner.ActL1SetFeeRecipient(common.Address{'A'})
	sequencer.ActL2PipelineFull(t)

	batcher := helpers.NewL2Batcher(log, sd.RollupCfg, helpers.BatchedCommsBatcherCfg(dp, storage), sequencer.RollupClient(), l1Client, engine.EthClient(), engCl)

	addresses := e2eutils.CollectAddresses(sd, dp)
	cl := engine.EthClient()
	l2UserEnv := &helpers.BasicUserEnv[*helpers.L2Bindings]{
		EthCl:          cl,
		Signer:         types.LatestSigner(sd.L2Cfg.Config),
		AddressCorpora: addresses,
		Bindings:       helpers.NewL2Bindings(t, cl, engine.GethClient()),
	}
	alice := helpers.NewCrossLayerUser(log, dp.Secrets.Alice, rand.New(rand.NewSource(0xa57b)), p.AllocType)
	alice.L2.SetUserEnv(l2UserEnv)

	contract, err := bindings.NewDataAvailabilityChallenge(sd.RollupCfg.AltDAConfig.DAChallengeAddress, l1Client)
	require.NoError(t, err)

	challengeWindow, err := contract.ChallengeWindow(nil)
	require.NoError(t, err)
	require.Equal(t, altDACfg.ChallengeWindow, challengeWindow.Uint64())

	resolveWindow, err := contract.ResolveWindow(nil)
	require.NoError(t, err)
	require.Equal(t, altDACfg.ResolveWindow, resolveWindow.Uint64())

	return &L2AltDA{
		log:       log,
		storage:   storage,
		daMgr:     daMgr,
		altDACfg:  altDACfg,
		contract:  contract,
		batcher:   batcher,
		sequencer: sequencer,
		engine:    engine,
		engCl:     engCl,
		sd:        sd,
		dp:        dp,
		miner:     miner,
		alice:     alice,
	}
}

func (a *L2AltDA) ActSequencerIncludeBigTxs(t helpers.Testing, n int) {
	rng := rand.New(rand.NewSource(555))

	a.sequencer.ActL2StartBlock(t)
	// build an L2 block with i large txs of random data (each should take a whole frame)
	for i := 0; i < n ; i++ {
		data := make([]byte, 120_000) // very large L2 txs, as large as the tx-pool will accept
		_, err := rng.Read(data[:])   // fill with random bytes, to make compression ineffective
		require.NoError(t, err)

		a.alice.L2.ActResetTxOpts(t)
		a.alice.L2.ActSetTxToAddr(&a.dp.Addresses.Bob)(t)
		a.alice.L2.ActSetTxCalldata(data)(t)
		a.alice.L2.ActMakeTx(t)
		a.engine.ActL2IncludeTx(a.alice.Address())(t)
	}
	a.sequencer.ActL2EndBlock(t)
}

func (a *L2AltDA) ActSubmitBatchedCommitments(t helpers.Testing, n int) {
	a.ActSequencerIncludeBigTxs(t, n)

	// This should buffer 1 block, which will be consumed as 2 frames because of the size
	a.batcher.ActBufferAll(t)

	// close the channel
	a.batcher.ActL2ChannelClose(t)

	// Batch submit 2 commitments
	a.batcher.ActL2SubmitBatchedCommitments(t, n, func(tx *types.DynamicFeeTx) {
		// skip txdata version byte, and only store the second commitment
		comm, err := altda.DecodeCommitmentData(tx.Data[1:])
		require.NoError(t, err)

		if batchedComm, ok := comm.(altda.BatchedCommitment); ok {
			// The commitment implements BatchedCommitmentData
			comms, err := batchedComm.GetCommitments()
			require.NoError(t, err)

			require.Equal(t, len(comms), n)

			// Store last commitment
			a.lastComm = comms[n - 1].Encode()
		} else {
			require.Fail(t, "Decoded commitment is not BatchedCommitment")
		}
	})

	// Include batched commitments in L1 block
	a.miner.ActL1StartBlock(12)(t)
	a.miner.ActL1IncludeTx(a.dp.Addresses.Batcher)(t)
	a.miner.ActL1EndBlock(t)

	a.lastCommBn = a.miner.L1Chain().CurrentBlock().Number.Uint64()
}


func TestAltDABatched_Derivation(gt *testing.T) {
	t := helpers.NewDefaultTesting(gt)
	harness := NewL2AltDABatched(t)
	verifier := harness.NewVerifier(t)

	harness.ActSubmitBatchedCommitments(t, 2)

	// Send a head signal to the verifier
	verifier.ActL1HeadSignal(t)
	harness.sequencer.ActL1HeadSignal(t)

	verifier.ActL2PipelineFull(t)
	harness.sequencer.ActL2PipelineFull(t)

	require.Equal(t, harness.sequencer.SyncStatus().UnsafeL2, verifier.SyncStatus().SafeL2, "verifier synced sequencer data")
}

// Commitment is challenged but never resolved, chain reorgs when challenge window expires.
func TestAltDABatched_ChallengeExpired(gt *testing.T) {
	t := helpers.NewDefaultTesting(gt)
	harness := NewL2AltDABatched(t)

	// generate enough initial l1 blocks to have a finalized head.
	harness.ActL1Blocks(t, 5)

	// Include a new l2 transaction, submitting an 2 batched commitments to the l1.
	harness.ActSubmitBatchedCommitments(t, 2)

	// Challenge the input commitment on the l1 challenge contract.
	harness.ActChallengeLastInput(t)

	blk := harness.GetLastTxBlock(t)

	// catch up the sequencer derivation pipeline with the new l1 blocks.
	harness.sequencer.ActL2PipelineFull(t)

	// create enough l1 blocks to expire the resolve window.
	harness.ActExpireLastInput(t)

	// catch up the sequencer derivation pipeline with the new l1 blocks.
	harness.sequencer.ActL2PipelineFull(t)

	// the L1 finalized signal should trigger altDA to finalize the engine queue.
	harness.ActL1Finalized(t)

	// move one more block for engine controller to update.
	harness.ActL1Blocks(t, 1)
	harness.sequencer.ActL2PipelineFull(t)

	// get new block with same number to compare
	newBlk, err := harness.engine.EthClient().BlockByNumber(t.Ctx(), blk.Number())
	require.NoError(t, err)

	// reorg happened even though data was available
	require.NotEqual(t, blk.Hash(), newBlk.Hash())

	// now delete the data from the storage service so it is not available at all
	// to the verifier derivation pipeline.
	harness.ActDeleteLastInput(t)

	syncStatus := harness.sequencer.SyncStatus()

	// verifier is able to sync with expired missing data
	verifier := harness.NewVerifier(t)
	verifier.ActL2PipelineFull(t)
	verifier.ActL1FinalizedSignal(t)

	verifSyncStatus := verifier.SyncStatus()

	require.Equal(t, syncStatus.FinalizedL2, verifSyncStatus.FinalizedL2)
}


// Commitment is challenged after sequencer derived the chain but data disappears. A verifier
// derivation pipeline stalls until the challenge is resolved and then resumes with data from the contract.
func TestAltDABatched_ChallengeResolved(gt *testing.T) {
	t := helpers.NewDefaultTesting(gt)
	harness := NewL2AltDABatched(t)

	harness.ActSubmitBatchedCommitments(t, 2)

	// generate 3 l1 blocks.
	harness.ActL1Blocks(t, 3)

	// challenge the input commitment for that l2 transaction on the l1 challenge contract.
	harness.ActChallengeLastInput(t)

	// catch up sequencer derivation pipeline.
	// this syncs the latest event within the AltDA manager.
	harness.sequencer.ActL2PipelineFull(t)

	// resolve the challenge on the l1 challenge contract.
	harness.ActResolveLastChallenge(t)

	// catch up the sequencer derivation pipeline with the new l1 blocks.
	// this syncs the resolved status and input data within the AltDA manager.
	harness.sequencer.ActL2PipelineFull(t)

	// finalize l1
	harness.ActL1Finalized(t)

	// delete the data from the storage service so it is not available at all
	// to the verifier derivation pipeline.
	harness.ActDeleteLastInput(t)

	syncStatus := harness.sequencer.SyncStatus()

	// new verifier is able to sync and resolve the input from calldata
	verifier := harness.NewVerifier(t)
	verifier.ActL2PipelineFull(t)
	verifier.ActL1FinalizedSignal(t)

	verifSyncStatus := verifier.SyncStatus()

	require.Equal(t, syncStatus.SafeL2, verifSyncStatus.SafeL2)
}

// DA storage service goes offline while sequencer keeps making blocks. When storage comes back online, it should be able to catch up.
func TestAltDABatched_StorageError(gt *testing.T) {
	t := helpers.NewDefaultTesting(gt)
	harness := NewL2AltDABatched(t)

	harness.ActSubmitBatchedCommitments(t, 2)

	txBlk := harness.GetLastTxBlock(t)

	// mock a storage client error when trying to get the pre-image.
	// this simulates the storage service going offline for example.
	harness.storage.ActGetPreImageFail()

	// try to derive the l2 chain from the submitted inputs commitments.
	// the storage call will fail the first time then succeed.
	harness.sequencer.ActL2PipelineFull(t)

	// sequencer derivation was able to sync to latest l1 origin
	syncStatus := harness.sequencer.SyncStatus()
	require.Equal(t, uint64(1), syncStatus.SafeL2.Number)
	require.Equal(t, txBlk.Hash(), syncStatus.SafeL2.Hash)
}

// L1 chain reorgs a resolved challenge so it expires instead causing
// the l2 chain to reorg as well.
func TestAltDABatched_ChallengeReorg(gt *testing.T) {
	t := helpers.NewDefaultTesting(gt)
	harness := NewL2AltDABatched(t)

	harness.ActSubmitBatchedCommitments(t, 2)

	// add a buffer of L1 blocks
	harness.ActL1Blocks(t, 3)

	// challenge the input commitment
	harness.ActChallengeLastInput(t)

	// keep track of the block where the L2 tx was included
	blk := harness.GetLastTxBlock(t)

	// progress derivation pipeline
	harness.sequencer.ActL2PipelineFull(t)

	// resolve the challenge so pipeline can progress
	harness.ActResolveLastChallenge(t)

	// derivation marks the challenge as resolve, chain is not impacted
	harness.sequencer.ActL2PipelineFull(t)

	// Rewind the L1, essentially reorging the challenge resolution
	harness.miner.ActL1RewindToParent(t)

	// Now the L1 chain advances without the challenge resolution
	// so the challenge is expired.
	harness.ActExpireLastInput(t)

	// derivation pipeline reorgs the commitment out of the chain
	harness.sequencer.ActL2PipelineFull(t)

	newBlk, err := harness.engine.EthClient().BlockByNumber(t.Ctx(), blk.Number())
	require.NoError(t, err)

	// confirm the reorg did happen
	require.NotEqual(t, blk.Hash(), newBlk.Hash())
}

// Sequencer stalls as data is not available, batcher keeps posting, untracked commitments are
// challenged and resolved, then sequencer resumes and catches up.
func TestAltDABatched_SequencerStalledMultiChallenges(gt *testing.T) {
	t := helpers.NewDefaultTesting(gt)
	a := NewL2AltDABatched(t)

	a.ActSubmitBatchedCommitments(t, 2)

	// keep track of the related commitment (second batched commitment)
	comm1 := a.lastComm
	input1, err := a.storage.GetInput(t.Ctx(), altda.Keccak256Commitment(comm1[1:]))
	bn1 := a.lastCommBn
	require.NoError(t, err)

	// delete it from the DA provider so the pipeline cannot verify it
	a.ActDeleteLastInput(t)

	// build more empty l2 unsafe blocks as the l1 origin progresses
	a.ActL1Blocks(t, 10)
	a.sequencer.ActBuildToL1HeadUnsafe(t)

	// build another L2 block without advancing derivation
	a.alice.L2.ActResetTxOpts(t)
	a.alice.L2.ActSetTxToAddr(&a.dp.Addresses.Bob)(t)
	a.alice.L2.ActMakeTx(t)

	a.sequencer.ActL2StartBlock(t)
	a.engine.ActL2IncludeTx(a.alice.Address())(t)
	a.sequencer.ActL2EndBlock(t)

	a.batcher.ActL2BatchBuffer(t)
	a.batcher.ActL2ChannelClose(t)
	a.batcher.ActL2BatchSubmit(t, func(tx *types.DynamicFeeTx) {
		a.lastComm = tx.Data[1:]
	})

	// include it in L1
	a.miner.ActL1StartBlock(12)(t)
	a.miner.ActL1IncludeTx(a.dp.Addresses.Batcher)(t)
	a.miner.ActL1EndBlock(t)

	a.sequencer.ActL1HeadSignal(t)

	unsafe := a.sequencer.L2Unsafe()
	unsafeBlk, err := a.engine.EthClient().BlockByHash(t.Ctx(), unsafe.Hash)
	require.NoError(t, err)

	// advance the pipeline until it errors out as it is still stuck
	// on deriving the first commitment
	a.sequencer.ActL2EventsUntil(t, func(ev event.Event) bool {
		x, ok := ev.(rollup.EngineTemporaryErrorEvent)
		if ok {
			require.ErrorContains(t, x.Err, "failed to fetch input data")
		}
		return ok
	}, 100, false)

	// keep track of the second commitment
	comm2 := a.lastComm
	_, err = a.storage.GetInput(t.Ctx(), altda.Keccak256Commitment(comm2[1:]))
	require.NoError(t, err)
	a.lastCommBn = a.miner.L1Chain().CurrentBlock().Number.Uint64()

	// ensure the second commitment is distinct from the first
	require.NotEqual(t, comm1, comm2)

	// challenge the last commitment while the pipeline is stuck on the first
	a.ActChallengeLastInput(t)

	// resolve the latest commitment before the first one is even challenged.
	a.ActResolveLastChallenge(t)

	// now we delete it to force the pipeline to resolve the second commitment
	// from the challenge data.
	a.ActDeleteLastInput(t)

	// finally challenge the first commitment
	a.ActChallengeInput(t, comm1, bn1)

	// resolve it immediately so we can resume derivation
	a.ActResolveInput(t, comm1, input1, bn1)

	// pipeline can go on
	a.sequencer.ActL2PipelineFull(t)

	// verify that the chain did not reorg out
	safeBlk, err := a.engine.EthClient().BlockByNumber(t.Ctx(), unsafeBlk.Number())
	require.NoError(t, err)
	require.Equal(t, unsafeBlk.Hash(), safeBlk.Hash())
}
