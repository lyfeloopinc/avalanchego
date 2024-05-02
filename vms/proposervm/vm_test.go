// Copyright (C) 2019-2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proposervm

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/memdb"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman/snowmantest"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/timer/mockable"
	"github.com/ava-labs/avalanchego/vms/proposervm/proposer"
	"github.com/ava-labs/avalanchego/vms/proposervm/state"

	statelessblock "github.com/ava-labs/avalanchego/vms/proposervm/block"
)

var (
	_ block.ChainVM         = (*fullVM)(nil)
	_ block.StateSyncableVM = (*fullVM)(nil)
)

type fullVM struct {
	*block.TestVM
	*block.TestStateSyncableVM
}

var (
	pTestSigner crypto.Signer
	pTestCert   *staking.Certificate

	defaultPChainHeight uint64 = 2000

	errUnknownBlock      = errors.New("unknown block")
	errUnverifiedBlock   = errors.New("unverified block")
	errMarshallingFailed = errors.New("marshalling failed")
	errTooHigh           = errors.New("too high")
	errUnexpectedCall    = errors.New("unexpected call")
)

func init() {
	tlsCert, err := staking.NewTLSCert()
	if err != nil {
		panic(err)
	}
	pTestSigner = tlsCert.PrivateKey.(crypto.Signer)
	pTestCert, err = staking.ParseCertificate(tlsCert.Leaf.Raw)
	if err != nil {
		panic(err)
	}
}

func initTestProposerVM(
	t *testing.T,
	proBlkStartTime time.Time,
	durangoTime time.Time,
	minPChainHeight uint64,
) (
	*fullVM,
	*validators.TestState,
	*VM,
	database.Database,
) {
	require := require.New(t)

	initialState := []byte("genesis state")
	coreVM := &fullVM{
		TestVM: &block.TestVM{
			TestVM: common.TestVM{
				T: t,
			},
		},
		TestStateSyncableVM: &block.TestStateSyncableVM{
			T: t,
		},
	}

	coreVM.InitializeF = func(context.Context, *snow.Context, database.Database,
		[]byte, []byte, []byte, chan<- common.Message,
		[]*common.Fx, common.AppSender,
	) error {
		return nil
	}
	coreVM.LastAcceptedF = func(context.Context) (ids.ID, error) {
		return snowmantest.GenesisID, nil
	}
	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}

	proVM := New(
		coreVM,
		Config{
			ActivationTime:      proBlkStartTime,
			DurangoTime:         durangoTime,
			MinimumPChainHeight: minPChainHeight,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	valState := &validators.TestState{
		T: t,
	}
	valState.GetMinimumHeightF = func(context.Context) (uint64, error) {
		return snowmantest.GenesisHeight, nil
	}
	valState.GetCurrentHeightF = func(context.Context) (uint64, error) {
		return defaultPChainHeight, nil
	}
	valState.GetValidatorSetF = func(context.Context, uint64, ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
		var (
			thisNode = proVM.ctx.NodeID
			nodeID1  = ids.BuildTestNodeID([]byte{1})
			nodeID2  = ids.BuildTestNodeID([]byte{2})
			nodeID3  = ids.BuildTestNodeID([]byte{3})
		)
		return map[ids.NodeID]*validators.GetValidatorOutput{
			thisNode: {
				NodeID: thisNode,
				Weight: 10,
			},
			nodeID1: {
				NodeID: nodeID1,
				Weight: 5,
			},
			nodeID2: {
				NodeID: nodeID2,
				Weight: 6,
			},
			nodeID3: {
				NodeID: nodeID3,
				Weight: 7,
			},
		}, nil
	}

	ctx := snowtest.Context(t, ids.ID{1})
	ctx.NodeID = ids.NodeIDFromCert(pTestCert)
	ctx.ValidatorState = valState

	db := prefixdb.New([]byte{0}, memdb.New())

	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		db,
		initialState,
		nil,
		nil,
		nil,
		nil,
		nil,
	))

	// Initialize shouldn't be called again
	coreVM.InitializeF = nil

	require.NoError(proVM.SetState(context.Background(), snow.NormalOp))
	require.NoError(proVM.SetPreference(context.Background(), snowmantest.GenesisID))

	proVM.Set(snowmantest.GenesisTimestamp)

	return coreVM, valState, proVM, db
}

func waitForProposerWindow(vm *VM, chainTip snowman.Block, pchainHeight uint64) error {
	var (
		ctx              = context.Background()
		childBlockHeight = chainTip.Height() + 1
		parentTimestamp  = chainTip.Timestamp()
	)

	for {
		slot := proposer.TimeToSlot(parentTimestamp, vm.Clock.Time().Truncate(time.Second))
		delay, err := vm.MinDelayForProposer(
			ctx,
			childBlockHeight,
			pchainHeight,
			vm.ctx.NodeID,
			slot,
		)
		if err != nil {
			return err
		}

		vm.Clock.Set(parentTimestamp.Add(delay))
		if delay < proposer.MaxLookAheadWindow {
			return nil
		}
	}
}

// VM.BuildBlock tests section

func TestBuildBlockTimestampAreRoundedToSeconds(t *testing.T) {
	require := require.New(t)

	// given the same core block, BuildBlock returns the same proposer block
	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	skewedTimestamp := time.Now().Truncate(time.Second).Add(time.Millisecond)
	proVM.Set(skewedTimestamp)

	coreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk, nil
	}

	// test
	builtBlk, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	require.Equal(builtBlk.Timestamp().Truncate(time.Second), builtBlk.Timestamp())
}

func TestBuildBlockIsIdempotent(t *testing.T) {
	require := require.New(t)

	// given the same core block, BuildBlock returns the same proposer block
	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	coreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk, nil
	}

	// Mock the clock time to make sure that block timestamps will be equal
	proVM.Clock.Set(time.Now())

	builtBlk1, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	builtBlk2, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	require.Equal(builtBlk1.Bytes(), builtBlk2.Bytes())
}

func TestFirstProposerBlockIsBuiltOnTopOfGenesis(t *testing.T) {
	require := require.New(t)

	// setup
	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	coreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk, nil
	}

	// test
	snowBlock, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	// checks
	require.IsType(&postForkBlock{}, snowBlock)
	proBlock := snowBlock.(*postForkBlock)

	require.Equal(coreBlk, proBlock.innerBlk)
}

// both core blocks and pro blocks must be built on preferred
func TestProposerBlocksAreBuiltOnPreferredProBlock(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// add two proBlks...
	coreBlk1 := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk1, nil
	}
	proBlk1, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	coreBlk2 := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk2, nil
	}
	proBlk2, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	require.NotEqual(proBlk2.ID(), proBlk1.ID())
	require.NoError(proBlk2.Verify(context.Background()))

	// ...and set one as preferred
	var prefcoreBlk *snowmantest.Block
	coreVM.SetPreferenceF = func(_ context.Context, prefID ids.ID) error {
		switch prefID {
		case coreBlk1.ID():
			prefcoreBlk = coreBlk1
			return nil
		case coreBlk2.ID():
			prefcoreBlk = coreBlk2
			return nil
		default:
			require.FailNow("prefID does not match coreBlk1 or coreBlk2")
			return nil
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, coreBlk1.Bytes()):
			return coreBlk1, nil
		case bytes.Equal(b, coreBlk2.Bytes()):
			return coreBlk2, nil
		default:
			require.FailNow("bytes do not match coreBlk1 or coreBlk2")
			return nil, nil
		}
	}

	require.NoError(proVM.SetPreference(context.Background(), proBlk2.ID()))

	// build block...
	coreBlk3 := snowmantest.BuildChild(prefcoreBlk)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk3, nil
	}

	require.NoError(waitForProposerWindow(proVM, proBlk2, proBlk2.(*postForkBlock).PChainHeight()))
	builtBlk, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	// ...show that parent is the preferred one
	require.Equal(proBlk2.ID(), builtBlk.Parent())
}

func TestCoreBlocksMustBeBuiltOnPreferredCoreBlock(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	coreBlk1 := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk1, nil
	}
	proBlk1, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	coreBlk2 := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk2, nil
	}
	proBlk2, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	require.NotEqual(proBlk1.ID(), proBlk2.ID())

	require.NoError(proBlk2.Verify(context.Background()))

	// ...and set one as preferred
	var wronglyPreferredcoreBlk *snowmantest.Block
	coreVM.SetPreferenceF = func(_ context.Context, prefID ids.ID) error {
		switch prefID {
		case coreBlk1.ID():
			wronglyPreferredcoreBlk = coreBlk2
			return nil
		case coreBlk2.ID():
			wronglyPreferredcoreBlk = coreBlk1
			return nil
		default:
			require.FailNow("Unknown core Blocks set as preferred")
			return nil
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, coreBlk1.Bytes()):
			return coreBlk1, nil
		case bytes.Equal(b, coreBlk2.Bytes()):
			return coreBlk2, nil
		default:
			require.FailNow("Wrong bytes")
			return nil, nil
		}
	}

	require.NoError(proVM.SetPreference(context.Background(), proBlk2.ID()))

	// build block...
	coreBlk3 := snowmantest.BuildChild(wronglyPreferredcoreBlk)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk3, nil
	}

	require.NoError(waitForProposerWindow(proVM, proBlk2, proBlk2.(*postForkBlock).PChainHeight()))
	blk, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	err = blk.Verify(context.Background())
	require.ErrorIs(err, errInnerParentMismatch)
}

// VM.ParseBlock tests section
func TestCoreBlockFailureCauseProposerBlockParseFailure(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	coreVM.ParseBlockF = func(context.Context, []byte) (snowman.Block, error) {
		return nil, errMarshallingFailed
	}

	innerBlk := snowmantest.BuildChild(snowmantest.Genesis)
	slb, err := statelessblock.Build(
		proVM.preferred,
		proVM.Time(),
		100, // pChainHeight,
		proVM.StakingCertLeaf,
		innerBlk.Bytes(),
		proVM.ctx.ChainID,
		proVM.StakingLeafSigner,
	)
	require.NoError(err)
	proBlk := postForkBlock{
		SignedBlock: slb,
		postForkCommonComponents: postForkCommonComponents{
			vm:       proVM,
			innerBlk: innerBlk,
			status:   choices.Processing,
		},
	}

	// test
	_, err = proVM.ParseBlock(context.Background(), proBlk.Bytes())
	require.ErrorIs(err, errMarshallingFailed)
}

func TestTwoProBlocksWrappingSameCoreBlockCanBeParsed(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// create two Proposer blocks at the same height
	innerBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		require.Equal(innerBlk.Bytes(), b)
		return innerBlk, nil
	}

	blkTimestamp := proVM.Time()

	slb1, err := statelessblock.Build(
		proVM.preferred,
		blkTimestamp,
		100, // pChainHeight,
		proVM.StakingCertLeaf,
		innerBlk.Bytes(),
		proVM.ctx.ChainID,
		proVM.StakingLeafSigner,
	)
	require.NoError(err)
	proBlk1 := postForkBlock{
		SignedBlock: slb1,
		postForkCommonComponents: postForkCommonComponents{
			vm:       proVM,
			innerBlk: innerBlk,
			status:   choices.Processing,
		},
	}

	slb2, err := statelessblock.Build(
		proVM.preferred,
		blkTimestamp,
		200, // pChainHeight,
		proVM.StakingCertLeaf,
		innerBlk.Bytes(),
		proVM.ctx.ChainID,
		proVM.StakingLeafSigner,
	)
	require.NoError(err)
	proBlk2 := postForkBlock{
		SignedBlock: slb2,
		postForkCommonComponents: postForkCommonComponents{
			vm:       proVM,
			innerBlk: innerBlk,
			status:   choices.Processing,
		},
	}

	require.NotEqual(proBlk1.ID(), proBlk2.ID())

	// Show that both can be parsed and retrieved
	parsedBlk1, err := proVM.ParseBlock(context.Background(), proBlk1.Bytes())
	require.NoError(err)
	parsedBlk2, err := proVM.ParseBlock(context.Background(), proBlk2.Bytes())
	require.NoError(err)

	require.Equal(proBlk1.ID(), parsedBlk1.ID())
	require.Equal(proBlk2.ID(), parsedBlk2.ID())
}

// VM.BuildBlock and VM.ParseBlock interoperability tests section
func TestTwoProBlocksWithSameParentCanBothVerify(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// one block is built from this proVM
	localcoreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return localcoreBlk, nil
	}

	builtBlk, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	require.NoError(builtBlk.Verify(context.Background()))

	// another block with same parent comes from network and is parsed
	netcoreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		case bytes.Equal(b, localcoreBlk.Bytes()):
			return localcoreBlk, nil
		case bytes.Equal(b, netcoreBlk.Bytes()):
			return netcoreBlk, nil
		default:
			require.FailNow("Unknown bytes")
			return nil, nil
		}
	}

	pChainHeight, err := proVM.ctx.ValidatorState.GetCurrentHeight(context.Background())
	require.NoError(err)

	netSlb, err := statelessblock.BuildUnsigned(
		proVM.preferred,
		proVM.Time(),
		pChainHeight,
		netcoreBlk.Bytes(),
	)
	require.NoError(err)
	netProBlk := postForkBlock{
		SignedBlock: netSlb,
		postForkCommonComponents: postForkCommonComponents{
			vm:       proVM,
			innerBlk: netcoreBlk,
			status:   choices.Processing,
		},
	}

	// prove that also block from network verifies
	require.NoError(netProBlk.Verify(context.Background()))
}

// Pre Fork tests section
func TestPreFork_Initialize(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = mockable.MaxTime
		durangoTime    = activationTime
	)
	_, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// checks
	blkID, err := proVM.LastAccepted(context.Background())
	require.NoError(err)

	rtvdBlk, err := proVM.GetBlock(context.Background(), blkID)
	require.NoError(err)

	require.IsType(&preForkBlock{}, rtvdBlk)
	require.Equal(snowmantest.GenesisBytes, rtvdBlk.Bytes())
}

func TestPreFork_BuildBlock(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = mockable.MaxTime
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	coreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk, nil
	}

	// test
	builtBlk, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	require.IsType(&preForkBlock{}, builtBlk)
	require.Equal(coreBlk.ID(), builtBlk.ID())
	require.Equal(coreBlk.Bytes(), builtBlk.Bytes())

	// test
	coreVM.GetBlockF = func(context.Context, ids.ID) (snowman.Block, error) {
		return coreBlk, nil
	}
	storedBlk, err := proVM.GetBlock(context.Background(), builtBlk.ID())
	require.NoError(err)
	require.Equal(builtBlk.ID(), storedBlk.ID())
}

func TestPreFork_ParseBlock(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = mockable.MaxTime
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	coreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		require.Equal(coreBlk.Bytes(), b)
		return coreBlk, nil
	}

	parsedBlk, err := proVM.ParseBlock(context.Background(), coreBlk.Bytes())
	require.NoError(err)
	require.IsType(&preForkBlock{}, parsedBlk)
	require.Equal(coreBlk.ID(), parsedBlk.ID())
	require.Equal(coreBlk.Bytes(), parsedBlk.Bytes())

	coreVM.GetBlockF = func(_ context.Context, id ids.ID) (snowman.Block, error) {
		require.Equal(coreBlk.ID(), id)
		return coreBlk, nil
	}
	storedBlk, err := proVM.GetBlock(context.Background(), parsedBlk.ID())
	require.NoError(err)
	require.Equal(parsedBlk.ID(), storedBlk.ID())
}

func TestPreFork_SetPreference(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = mockable.MaxTime
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	coreBlk0 := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk0, nil
	}
	builtBlk, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		case coreBlk0.ID():
			return coreBlk0, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		case bytes.Equal(b, coreBlk0.Bytes()):
			return coreBlk0, nil
		default:
			return nil, errUnknownBlock
		}
	}
	require.NoError(proVM.SetPreference(context.Background(), builtBlk.ID()))

	coreBlk1 := snowmantest.BuildChild(coreBlk0)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk1, nil
	}
	nextBlk, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	require.Equal(builtBlk.ID(), nextBlk.Parent())
}

func TestExpiredBuildBlock(t *testing.T) {
	require := require.New(t)

	coreVM := &block.TestVM{}
	coreVM.T = t

	coreVM.LastAcceptedF = func(context.Context) (ids.ID, error) {
		return snowmantest.GenesisID, nil
	}
	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}

	proVM := New(
		coreVM,
		Config{
			ActivationTime:      time.Time{},
			DurangoTime:         mockable.MaxTime,
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	valState := &validators.TestState{
		T: t,
	}
	valState.GetMinimumHeightF = func(context.Context) (uint64, error) {
		return snowmantest.GenesisHeight, nil
	}
	valState.GetCurrentHeightF = func(context.Context) (uint64, error) {
		return defaultPChainHeight, nil
	}
	valState.GetValidatorSetF = func(context.Context, uint64, ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
		nodeID := ids.BuildTestNodeID([]byte{1})
		return map[ids.NodeID]*validators.GetValidatorOutput{
			nodeID: {
				NodeID: nodeID,
				Weight: 100,
			},
		}, nil
	}

	ctx := snowtest.Context(t, snowtest.CChainID)
	ctx.NodeID = ids.NodeIDFromCert(pTestCert)
	ctx.ValidatorState = valState

	toEngine := make(chan common.Message, 1)
	var toScheduler chan<- common.Message

	coreVM.InitializeF = func(
		_ context.Context,
		_ *snow.Context,
		_ database.Database,
		_ []byte,
		_ []byte,
		_ []byte,
		toEngineChan chan<- common.Message,
		_ []*common.Fx,
		_ common.AppSender,
	) error {
		toScheduler = toEngineChan
		return nil
	}

	// make sure that DBs are compressed correctly
	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		memdb.New(),
		nil,
		nil,
		nil,
		toEngine,
		nil,
		nil,
	))
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// Initialize shouldn't be called again
	coreVM.InitializeF = nil

	require.NoError(proVM.SetState(context.Background(), snow.NormalOp))
	require.NoError(proVM.SetPreference(context.Background(), snowmantest.GenesisID))

	// Notify the proposer VM of a new block on the inner block side
	toScheduler <- common.PendingTxs
	// The first notification will be read from the consensus engine
	<-toEngine

	// Before calling BuildBlock, verify a remote block and set it as the
	// preferred block.
	coreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	statelessBlock, err := statelessblock.BuildUnsigned(
		snowmantest.GenesisID,
		proVM.Time(),
		0,
		coreBlk.Bytes(),
	)
	require.NoError(err)

	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		case coreBlk.ID():
			return coreBlk, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		case bytes.Equal(b, coreBlk.Bytes()):
			return coreBlk, nil
		default:
			return nil, errUnknownBlock
		}
	}

	proVM.Clock.Set(statelessBlock.Timestamp())

	parsedBlock, err := proVM.ParseBlock(context.Background(), statelessBlock.Bytes())
	require.NoError(err)

	require.NoError(parsedBlock.Verify(context.Background()))
	require.NoError(proVM.SetPreference(context.Background(), parsedBlock.ID()))

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		require.FailNow(fmt.Errorf("%w: BuildBlock", errUnexpectedCall).Error())
		return nil, errUnexpectedCall
	}

	// Because we are now building on a different block, the proposer window
	// shouldn't have started.
	_, err = proVM.BuildBlock(context.Background())
	require.ErrorIs(err, errProposerWindowNotStarted)

	proVM.Set(statelessBlock.Timestamp().Add(proposer.MaxBuildDelay))
	proVM.Scheduler.SetBuildBlockTime(time.Now())

	// The engine should have been notified to attempt to build a block now that
	// the window has started again. This is to guarantee that the inner VM has
	// build block called after it sent a pendingTxs message on its internal
	// engine channel.
	<-toEngine
}

type wrappedBlock struct {
	snowman.Block
	verified bool
}

func (b *wrappedBlock) Accept(ctx context.Context) error {
	if !b.verified {
		return errUnverifiedBlock
	}
	return b.Block.Accept(ctx)
}

func (b *wrappedBlock) Verify(ctx context.Context) error {
	if err := b.Block.Verify(ctx); err != nil {
		return err
	}
	b.verified = true
	return nil
}

func TestInnerBlockDeduplication(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	coreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	coreBlk0 := &wrappedBlock{
		Block: coreBlk,
	}
	coreBlk1 := &wrappedBlock{
		Block: coreBlk,
	}
	statelessBlock0, err := statelessblock.BuildUnsigned(
		snowmantest.GenesisID,
		coreBlk.Timestamp(),
		0,
		coreBlk.Bytes(),
	)
	require.NoError(err)
	statelessBlock1, err := statelessblock.BuildUnsigned(
		snowmantest.GenesisID,
		coreBlk.Timestamp(),
		1,
		coreBlk.Bytes(),
	)
	require.NoError(err)

	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		case coreBlk0.ID():
			return coreBlk0, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		case bytes.Equal(b, coreBlk0.Bytes()):
			return coreBlk0, nil
		default:
			return nil, errUnknownBlock
		}
	}

	parsedBlock0, err := proVM.ParseBlock(context.Background(), statelessBlock0.Bytes())
	require.NoError(err)

	require.NoError(parsedBlock0.Verify(context.Background()))

	require.NoError(proVM.SetPreference(context.Background(), parsedBlock0.ID()))

	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		case coreBlk1.ID():
			return coreBlk1, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		case bytes.Equal(b, coreBlk1.Bytes()):
			return coreBlk1, nil
		default:
			return nil, errUnknownBlock
		}
	}

	parsedBlock1, err := proVM.ParseBlock(context.Background(), statelessBlock1.Bytes())
	require.NoError(err)

	require.NoError(parsedBlock1.Verify(context.Background()))

	require.NoError(proVM.SetPreference(context.Background(), parsedBlock1.ID()))

	require.NoError(parsedBlock1.Accept(context.Background()))
}

func TestInnerVMRollback(t *testing.T) {
	require := require.New(t)

	valState := &validators.TestState{
		T: t,
	}
	valState.GetCurrentHeightF = func(context.Context) (uint64, error) {
		return defaultPChainHeight, nil
	}
	valState.GetValidatorSetF = func(context.Context, uint64, ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
		nodeID := ids.BuildTestNodeID([]byte{1})
		return map[ids.NodeID]*validators.GetValidatorOutput{
			nodeID: {
				NodeID: nodeID,
				Weight: 100,
			},
		}, nil
	}

	coreVM := &block.TestVM{}
	coreVM.T = t

	coreVM.LastAcceptedF = func(context.Context) (ids.ID, error) {
		return snowmantest.GenesisID, nil
	}
	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}

	ctx := snowtest.Context(t, snowtest.CChainID)
	ctx.NodeID = ids.NodeIDFromCert(pTestCert)
	ctx.ValidatorState = valState

	coreVM.InitializeF = func(
		context.Context,
		*snow.Context,
		database.Database,
		[]byte,
		[]byte,
		[]byte,
		chan<- common.Message,
		[]*common.Fx,
		common.AppSender,
	) error {
		return nil
	}

	db := memdb.New()

	proVM := New(
		coreVM,
		Config{
			ActivationTime:      time.Time{},
			DurangoTime:         mockable.MaxTime,
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		db,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	))

	require.NoError(proVM.SetState(context.Background(), snow.NormalOp))
	require.NoError(proVM.SetPreference(context.Background(), snowmantest.GenesisID))

	coreBlk := snowmantest.BuildChild(snowmantest.Genesis)
	statelessBlock, err := statelessblock.BuildUnsigned(
		snowmantest.GenesisID,
		coreBlk.Timestamp(),
		0,
		coreBlk.Bytes(),
	)
	require.NoError(err)

	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		case coreBlk.ID():
			return coreBlk, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		case bytes.Equal(b, coreBlk.Bytes()):
			return coreBlk, nil
		default:
			return nil, errUnknownBlock
		}
	}

	proVM.Clock.Set(statelessBlock.Timestamp())

	parsedBlock, err := proVM.ParseBlock(context.Background(), statelessBlock.Bytes())
	require.NoError(err)

	require.Equal(choices.Processing, parsedBlock.Status())

	require.NoError(parsedBlock.Verify(context.Background()))
	require.NoError(proVM.SetPreference(context.Background(), parsedBlock.ID()))
	require.NoError(parsedBlock.Accept(context.Background()))

	fetchedBlock, err := proVM.GetBlock(context.Background(), parsedBlock.ID())
	require.NoError(err)

	require.Equal(choices.Accepted, fetchedBlock.Status())

	// Restart the node and have the inner VM rollback state.
	require.NoError(proVM.Shutdown(context.Background()))
	coreBlk.StatusV = choices.Processing

	proVM = New(
		coreVM,
		Config{
			ActivationTime:      time.Time{},
			DurangoTime:         mockable.MaxTime,
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		db,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	))
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	lastAcceptedID, err := proVM.LastAccepted(context.Background())
	require.NoError(err)

	require.Equal(snowmantest.GenesisID, lastAcceptedID)

	parsedBlock, err = proVM.ParseBlock(context.Background(), statelessBlock.Bytes())
	require.NoError(err)

	require.Equal(choices.Processing, parsedBlock.Status())
}

func TestBuildBlockDuringWindow(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = mockable.MaxTime
	)
	coreVM, valState, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	valState.GetValidatorSetF = func(context.Context, uint64, ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
		return map[ids.NodeID]*validators.GetValidatorOutput{
			proVM.ctx.NodeID: {
				NodeID: proVM.ctx.NodeID,
				Weight: 10,
			},
		}, nil
	}

	coreBlk0 := snowmantest.BuildChild(snowmantest.Genesis)
	coreBlk1 := snowmantest.BuildChild(coreBlk0)
	statelessBlock0, err := statelessblock.BuildUnsigned(
		snowmantest.GenesisID,
		proVM.Time(),
		0,
		coreBlk0.Bytes(),
	)
	require.NoError(err)

	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		case coreBlk0.ID():
			return coreBlk0, nil
		case coreBlk1.ID():
			return coreBlk1, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		case bytes.Equal(b, coreBlk0.Bytes()):
			return coreBlk0, nil
		case bytes.Equal(b, coreBlk1.Bytes()):
			return coreBlk1, nil
		default:
			return nil, errUnknownBlock
		}
	}

	proVM.Clock.Set(statelessBlock0.Timestamp())

	statefulBlock0, err := proVM.ParseBlock(context.Background(), statelessBlock0.Bytes())
	require.NoError(err)

	require.NoError(statefulBlock0.Verify(context.Background()))

	require.NoError(proVM.SetPreference(context.Background(), statefulBlock0.ID()))

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return coreBlk1, nil
	}

	statefulBlock1, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	require.NoError(statefulBlock1.Verify(context.Background()))

	require.NoError(proVM.SetPreference(context.Background(), statefulBlock1.ID()))

	require.NoError(statefulBlock0.Accept(context.Background()))

	require.NoError(statefulBlock1.Accept(context.Background()))
}

// Ensure that Accepting a PostForkBlock (A) containing core block (X) causes
// core block (Y) and (Z) to also be rejected.
//
//	     G
//	   /   \
//	A(X)   B(Y)
//	        |
//	       C(Z)
func TestTwoForks_OneIsAccepted(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = mockable.MaxTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// create pre-fork block X and post-fork block A
	xBlock := snowmantest.BuildChild(snowmantest.Genesis)

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return xBlock, nil
	}
	aBlock, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	coreVM.BuildBlockF = nil
	require.NoError(aBlock.Verify(context.Background()))

	// use a different way to construct pre-fork block Y and post-fork block B
	yBlock := snowmantest.BuildChild(snowmantest.Genesis)

	ySlb, err := statelessblock.BuildUnsigned(
		snowmantest.GenesisID,
		proVM.Time(),
		defaultPChainHeight,
		yBlock.Bytes(),
	)
	require.NoError(err)

	bBlock := postForkBlock{
		SignedBlock: ySlb,
		postForkCommonComponents: postForkCommonComponents{
			vm:       proVM,
			innerBlk: yBlock,
			status:   choices.Processing,
		},
	}

	require.NoError(bBlock.Verify(context.Background()))

	// append Z/C to Y/B
	zBlock := snowmantest.BuildChild(yBlock)

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return zBlock, nil
	}
	require.NoError(proVM.SetPreference(context.Background(), bBlock.ID()))
	proVM.Set(proVM.Time().Add(proposer.MaxBuildDelay))
	cBlock, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	coreVM.BuildBlockF = nil

	require.NoError(cBlock.Verify(context.Background()))

	require.Equal(bBlock.Parent(), aBlock.Parent())
	require.Equal(yBlock.ID(), zBlock.Parent())
	require.Equal(bBlock.ID(), cBlock.Parent())

	require.NotEqual(choices.Rejected, yBlock.Status())

	// accept A
	require.NoError(aBlock.Accept(context.Background()))

	require.Equal(choices.Accepted, xBlock.Status())
	require.Equal(choices.Rejected, yBlock.Status())
	require.Equal(choices.Rejected, zBlock.Status())
}

func TestTooFarAdvanced(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = mockable.MaxTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	xBlock := snowmantest.BuildChild(snowmantest.Genesis)
	yBlock := snowmantest.BuildChild(xBlock)

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return xBlock, nil
	}
	aBlock, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	require.NoError(aBlock.Verify(context.Background()))

	ySlb, err := statelessblock.BuildUnsigned(
		aBlock.ID(),
		aBlock.Timestamp().Add(maxSkew),
		defaultPChainHeight,
		yBlock.Bytes(),
	)
	require.NoError(err)

	bBlock := postForkBlock{
		SignedBlock: ySlb,
		postForkCommonComponents: postForkCommonComponents{
			vm:       proVM,
			innerBlk: yBlock,
			status:   choices.Processing,
		},
	}

	err = bBlock.Verify(context.Background())
	require.ErrorIs(err, errProposerWindowNotStarted)

	ySlb, err = statelessblock.BuildUnsigned(
		aBlock.ID(),
		aBlock.Timestamp().Add(proposer.MaxVerifyDelay),
		defaultPChainHeight,
		yBlock.Bytes(),
	)

	require.NoError(err)

	bBlock = postForkBlock{
		SignedBlock: ySlb,
		postForkCommonComponents: postForkCommonComponents{
			vm:       proVM,
			innerBlk: yBlock,
			status:   choices.Processing,
		},
	}

	err = bBlock.Verify(context.Background())
	require.ErrorIs(err, errTimeTooAdvanced)
}

// Ensure that Accepting a PostForkOption (B) causes both the other option and
// the core block in the other option to be rejected.
//
//	   G
//	   |
//	  A(X)
//	 /====\
//	B(...) C(...)
//
// B(...) is B(X.opts[0])
// B(...) is C(X.opts[1])
func TestTwoOptions_OneIsAccepted(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = mockable.MaxTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	xTestBlock := snowmantest.BuildChild(snowmantest.Genesis)
	xBlock := &TestOptionsBlock{
		Block: *xTestBlock,
		opts: [2]snowman.Block{
			snowmantest.BuildChild(xTestBlock),
			snowmantest.BuildChild(xTestBlock),
		},
	}

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return xBlock, nil
	}
	aBlockIntf, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	require.IsType(&postForkBlock{}, aBlockIntf)
	aBlock := aBlockIntf.(*postForkBlock)

	opts, err := aBlock.Options(context.Background())
	require.NoError(err)

	require.NoError(aBlock.Verify(context.Background()))
	bBlock := opts[0]
	require.NoError(bBlock.Verify(context.Background()))
	cBlock := opts[1]
	require.NoError(cBlock.Verify(context.Background()))

	require.NoError(aBlock.Accept(context.Background()))

	require.NoError(bBlock.Accept(context.Background()))

	// the other pre-fork option should be rejected
	require.Equal(choices.Rejected, xBlock.opts[1].Status())

	// the other post-fork option should also be rejected
	require.NoError(cBlock.Reject(context.Background()))

	require.Equal(choices.Rejected, cBlock.Status())
}

// Ensure that given the chance, built blocks will reference a lagged P-chain
// height.
func TestLaggedPChainHeight(t *testing.T) {
	require := require.New(t)

	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	innerBlock := snowmantest.BuildChild(snowmantest.Genesis)
	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return innerBlock, nil
	}
	blockIntf, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	require.IsType(&postForkBlock{}, blockIntf)
	block := blockIntf.(*postForkBlock)

	pChainHeight := block.PChainHeight()
	require.Equal(snowmantest.GenesisHeight, pChainHeight)
}

// Ensure that rejecting a block does not modify the accepted block ID for the
// rejected height.
func TestRejectedHeightNotIndexed(t *testing.T) {
	require := require.New(t)

	coreHeights := []ids.ID{snowmantest.GenesisID}

	initialState := []byte("genesis state")
	coreVM := &block.TestVM{
		TestVM: common.TestVM{
			T: t,
		},
		GetBlockIDAtHeightF: func(_ context.Context, height uint64) (ids.ID, error) {
			if height >= uint64(len(coreHeights)) {
				return ids.ID{}, errTooHigh
			}
			return coreHeights[height], nil
		},
	}

	coreVM.InitializeF = func(context.Context, *snow.Context, database.Database,
		[]byte, []byte, []byte, chan<- common.Message,
		[]*common.Fx, common.AppSender,
	) error {
		return nil
	}
	coreVM.LastAcceptedF = func(context.Context) (ids.ID, error) {
		return snowmantest.GenesisID, nil
	}
	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}

	proVM := New(
		coreVM,
		Config{
			ActivationTime:      time.Unix(0, 0),
			DurangoTime:         time.Unix(0, 0),
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	valState := &validators.TestState{
		T: t,
	}
	valState.GetMinimumHeightF = func(context.Context) (uint64, error) {
		return snowmantest.GenesisHeight, nil
	}
	valState.GetCurrentHeightF = func(context.Context) (uint64, error) {
		return defaultPChainHeight, nil
	}
	valState.GetValidatorSetF = func(context.Context, uint64, ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
		var (
			thisNode = proVM.ctx.NodeID
			nodeID1  = ids.BuildTestNodeID([]byte{1})
			nodeID2  = ids.BuildTestNodeID([]byte{2})
			nodeID3  = ids.BuildTestNodeID([]byte{3})
		)
		return map[ids.NodeID]*validators.GetValidatorOutput{
			thisNode: {
				NodeID: thisNode,
				Weight: 10,
			},
			nodeID1: {
				NodeID: nodeID1,
				Weight: 5,
			},
			nodeID2: {
				NodeID: nodeID2,
				Weight: 6,
			},
			nodeID3: {
				NodeID: nodeID3,
				Weight: 7,
			},
		}, nil
	}

	ctx := snowtest.Context(t, snowtest.CChainID)
	ctx.NodeID = ids.NodeIDFromCert(pTestCert)
	ctx.ValidatorState = valState

	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		prefixdb.New([]byte{}, memdb.New()), // make sure that DBs are compressed correctly
		initialState,
		nil,
		nil,
		nil,
		nil,
		nil,
	))
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// Initialize shouldn't be called again
	coreVM.InitializeF = nil

	require.NoError(proVM.SetState(context.Background(), snow.NormalOp))

	require.NoError(proVM.SetPreference(context.Background(), snowmantest.GenesisID))

	// create inner block X and outer block A
	xBlock := snowmantest.BuildChild(snowmantest.Genesis)

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return xBlock, nil
	}
	aBlock, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	coreVM.BuildBlockF = nil
	require.NoError(aBlock.Verify(context.Background()))

	// use a different way to construct inner block Y and outer block B
	yBlock := snowmantest.BuildChild(snowmantest.Genesis)

	ySlb, err := statelessblock.BuildUnsigned(
		snowmantest.GenesisID,
		snowmantest.GenesisTimestamp,
		defaultPChainHeight,
		yBlock.Bytes(),
	)
	require.NoError(err)

	bBlock := postForkBlock{
		SignedBlock: ySlb,
		postForkCommonComponents: postForkCommonComponents{
			vm:       proVM,
			outerBlk: ySlb,
			innerBlk: yBlock,
			status:   choices.Processing,
		},
	}

	require.NoError(bBlock.Verify(context.Background()))

	// accept A
	require.NoError(aBlock.Accept(context.Background()))
	coreHeights = append(coreHeights, xBlock.ID())

	blkID, err := proVM.GetBlockIDAtHeight(context.Background(), aBlock.Height())
	require.NoError(err)
	require.Equal(aBlock.ID(), blkID)

	// reject B
	require.NoError(bBlock.Reject(context.Background()))

	blkID, err = proVM.GetBlockIDAtHeight(context.Background(), aBlock.Height())
	require.NoError(err)
	require.Equal(aBlock.ID(), blkID)
}

// Ensure that rejecting an option block does not modify the accepted block ID
// for the rejected height.
func TestRejectedOptionHeightNotIndexed(t *testing.T) {
	require := require.New(t)

	coreHeights := []ids.ID{snowmantest.GenesisID}

	initialState := []byte("genesis state")
	coreVM := &block.TestVM{
		TestVM: common.TestVM{
			T: t,
		},
		GetBlockIDAtHeightF: func(_ context.Context, height uint64) (ids.ID, error) {
			if height >= uint64(len(coreHeights)) {
				return ids.ID{}, errTooHigh
			}
			return coreHeights[height], nil
		},
	}

	coreVM.InitializeF = func(context.Context, *snow.Context, database.Database,
		[]byte, []byte, []byte, chan<- common.Message,
		[]*common.Fx, common.AppSender,
	) error {
		return nil
	}
	coreVM.LastAcceptedF = func(context.Context) (ids.ID, error) {
		return snowmantest.GenesisID, nil
	}
	coreVM.GetBlockF = func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case snowmantest.GenesisID:
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}
	coreVM.ParseBlockF = func(_ context.Context, b []byte) (snowman.Block, error) {
		switch {
		case bytes.Equal(b, snowmantest.GenesisBytes):
			return snowmantest.Genesis, nil
		default:
			return nil, errUnknownBlock
		}
	}

	proVM := New(
		coreVM,
		Config{
			ActivationTime:      time.Unix(0, 0),
			DurangoTime:         time.Unix(0, 0),
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	valState := &validators.TestState{
		T: t,
	}
	valState.GetMinimumHeightF = func(context.Context) (uint64, error) {
		return snowmantest.GenesisHeight, nil
	}
	valState.GetCurrentHeightF = func(context.Context) (uint64, error) {
		return defaultPChainHeight, nil
	}
	valState.GetValidatorSetF = func(context.Context, uint64, ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
		var (
			thisNode = proVM.ctx.NodeID
			nodeID1  = ids.BuildTestNodeID([]byte{1})
			nodeID2  = ids.BuildTestNodeID([]byte{2})
			nodeID3  = ids.BuildTestNodeID([]byte{3})
		)
		return map[ids.NodeID]*validators.GetValidatorOutput{
			thisNode: {
				NodeID: thisNode,
				Weight: 10,
			},
			nodeID1: {
				NodeID: nodeID1,
				Weight: 5,
			},
			nodeID2: {
				NodeID: nodeID2,
				Weight: 6,
			},
			nodeID3: {
				NodeID: nodeID3,
				Weight: 7,
			},
		}, nil
	}

	ctx := snowtest.Context(t, snowtest.CChainID)
	ctx.NodeID = ids.NodeIDFromCert(pTestCert)
	ctx.ValidatorState = valState

	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		prefixdb.New([]byte{}, memdb.New()), // make sure that DBs are compressed correctly
		initialState,
		nil,
		nil,
		nil,
		nil,
		nil,
	))
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// Initialize shouldn't be called again
	coreVM.InitializeF = nil

	require.NoError(proVM.SetState(context.Background(), snow.NormalOp))

	require.NoError(proVM.SetPreference(context.Background(), snowmantest.GenesisID))

	xTestBlock := snowmantest.BuildChild(snowmantest.Genesis)
	xBlock := &TestOptionsBlock{
		Block: *xTestBlock,
		opts: [2]snowman.Block{
			snowmantest.BuildChild(xTestBlock),
			snowmantest.BuildChild(xTestBlock),
		},
	}

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return xBlock, nil
	}
	aBlockIntf, err := proVM.BuildBlock(context.Background())
	require.NoError(err)

	require.IsType(&postForkBlock{}, aBlockIntf)
	aBlock := aBlockIntf.(*postForkBlock)

	opts, err := aBlock.Options(context.Background())
	require.NoError(err)

	require.NoError(aBlock.Verify(context.Background()))

	bBlock := opts[0]
	require.NoError(bBlock.Verify(context.Background()))

	cBlock := opts[1]
	require.NoError(cBlock.Verify(context.Background()))

	// accept A
	require.NoError(aBlock.Accept(context.Background()))
	coreHeights = append(coreHeights, xBlock.ID())

	blkID, err := proVM.GetBlockIDAtHeight(context.Background(), aBlock.Height())
	require.NoError(err)
	require.Equal(aBlock.ID(), blkID)

	// accept B
	require.NoError(bBlock.Accept(context.Background()))
	coreHeights = append(coreHeights, xBlock.opts[0].ID())

	blkID, err = proVM.GetBlockIDAtHeight(context.Background(), bBlock.Height())
	require.NoError(err)
	require.Equal(bBlock.ID(), blkID)

	// reject C
	require.NoError(cBlock.Reject(context.Background()))

	blkID, err = proVM.GetBlockIDAtHeight(context.Background(), cBlock.Height())
	require.NoError(err)
	require.Equal(bBlock.ID(), blkID)
}

func TestVMInnerBlkCache(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)

	// Create a VM
	innerVM := block.NewMockChainVM(ctrl)
	vm := New(
		innerVM,
		Config{
			ActivationTime:      time.Unix(0, 0),
			DurangoTime:         time.Unix(0, 0),
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	innerVM.EXPECT().Initialize(
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
	).Return(nil)
	innerVM.EXPECT().Shutdown(gomock.Any()).Return(nil)

	{
		innerBlk := snowmantest.NewMockBlock(ctrl)
		innerBlkID := ids.GenerateTestID()
		innerVM.EXPECT().LastAccepted(gomock.Any()).Return(innerBlkID, nil).Times(2)
		innerVM.EXPECT().GetBlock(gomock.Any(), innerBlkID).Return(innerBlk, nil)
	}

	ctx := snowtest.Context(t, snowtest.CChainID)
	ctx.NodeID = ids.NodeIDFromCert(pTestCert)

	require.NoError(vm.Initialize(
		context.Background(),
		ctx,
		prefixdb.New([]byte{}, memdb.New()), // make sure that DBs are compressed correctly
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	))
	defer func() {
		require.NoError(vm.Shutdown(context.Background()))
	}()

	state := state.NewMockState(ctrl) // mock state
	vm.State = state

	// Create a block near the tip (0).
	blkNearTipInnerBytes := []byte{1}
	blkNearTip, err := statelessblock.Build(
		ids.GenerateTestID(), // parent
		time.Time{},          // timestamp
		1,                    // pChainHeight,
		vm.StakingCertLeaf,   // cert
		blkNearTipInnerBytes, // inner blk bytes
		vm.ctx.ChainID,       // chain ID
		vm.StakingLeafSigner, // key
	)
	require.NoError(err)

	// Parse a block.
	// Not in the VM's state so need to parse it.
	state.EXPECT().GetBlock(blkNearTip.ID()).Return(blkNearTip, choices.Accepted, nil).Times(2)
	// We will ask the inner VM to parse.
	mockInnerBlkNearTip := snowmantest.NewMockBlock(ctrl)
	mockInnerBlkNearTip.EXPECT().Height().Return(uint64(1)).Times(2)
	mockInnerBlkNearTip.EXPECT().Bytes().Return(blkNearTipInnerBytes).Times(1)

	innerVM.EXPECT().ParseBlock(gomock.Any(), blkNearTipInnerBytes).Return(mockInnerBlkNearTip, nil).Times(2)
	_, err = vm.ParseBlock(context.Background(), blkNearTip.Bytes())
	require.NoError(err)

	// Block should now be in cache because it's a post-fork block
	// and close to the tip.
	gotBlk, ok := vm.innerBlkCache.Get(blkNearTip.ID())
	require.True(ok)
	require.Equal(mockInnerBlkNearTip, gotBlk)
	require.Zero(vm.lastAcceptedHeight)

	// Clear the cache
	vm.innerBlkCache.Flush()

	// Advance the tip height
	vm.lastAcceptedHeight = innerBlkCacheSize + 1

	// Parse the block again. This time it shouldn't be cached
	// because it's not close to the tip.
	_, err = vm.ParseBlock(context.Background(), blkNearTip.Bytes())
	require.NoError(err)

	_, ok = vm.innerBlkCache.Get(blkNearTip.ID())
	require.False(ok)
}

func TestVMInnerBlkCacheDeduplicationRegression(t *testing.T) {
	require := require.New(t)
	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// create pre-fork block X and post-fork block A
	xBlock := snowmantest.BuildChild(snowmantest.Genesis)

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return xBlock, nil
	}
	aBlock, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	coreVM.BuildBlockF = nil

	bStatelessBlock, err := statelessblock.BuildUnsigned(
		snowmantest.GenesisID,
		snowmantest.GenesisTimestamp,
		defaultPChainHeight,
		xBlock.Bytes(),
	)
	require.NoError(err)

	xBlockCopy := *xBlock
	coreVM.ParseBlockF = func(context.Context, []byte) (snowman.Block, error) {
		return &xBlockCopy, nil
	}

	bBlockBytes := bStatelessBlock.Bytes()
	bBlock, err := proVM.ParseBlock(context.Background(), bBlockBytes)
	require.NoError(err)

	require.NoError(aBlock.Verify(context.Background()))
	require.NoError(bBlock.Verify(context.Background()))
	require.NoError(aBlock.Accept(context.Background()))
	require.NoError(bBlock.Reject(context.Background()))

	require.Equal(
		choices.Accepted,
		aBlock.(*postForkBlock).innerBlk.Status(),
	)

	require.Equal(
		choices.Accepted,
		bBlock.(*postForkBlock).innerBlk.Status(),
	)

	cachedXBlock, ok := proVM.innerBlkCache.Get(bBlock.ID())
	require.True(ok)
	require.Equal(
		choices.Accepted,
		cachedXBlock.Status(),
	)
}

func TestVMInnerBlkMarkedAcceptedRegression(t *testing.T) {
	require := require.New(t)
	var (
		activationTime = time.Unix(0, 0)
		durangoTime    = activationTime
	)
	coreVM, _, proVM, _ := initTestProposerVM(t, activationTime, durangoTime, 0)
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	// create an inner block and wrap it in an postForkBlock.
	innerBlock := snowmantest.BuildChild(snowmantest.Genesis)

	coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
		return innerBlock, nil
	}
	outerBlock, err := proVM.BuildBlock(context.Background())
	require.NoError(err)
	coreVM.BuildBlockF = nil

	require.NoError(outerBlock.Verify(context.Background()))
	require.NoError(outerBlock.Accept(context.Background()))

	coreVM.GetBlockF = func(_ context.Context, id ids.ID) (snowman.Block, error) {
		require.Equal(innerBlock.ID(), id)
		return innerBlock, nil
	}

	wrappedInnerBlock, err := proVM.GetBlock(context.Background(), innerBlock.ID())
	require.NoError(err)
	require.Equal(choices.Rejected, wrappedInnerBlock.Status())
}

type blockWithVerifyContext struct {
	*snowmantest.MockBlock
	*block.MockWithVerifyContext
}

// Ensures that we call [VerifyWithContext] rather than [Verify] on blocks that
// implement [block.WithVerifyContext] and that returns true for
// [ShouldVerifyWithContext].
func TestVM_VerifyBlockWithContext(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)

	// Create a VM
	innerVM := block.NewMockChainVM(ctrl)
	vm := New(
		innerVM,
		Config{
			ActivationTime:      time.Unix(0, 0),
			DurangoTime:         time.Unix(0, 0),
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	// make sure that DBs are compressed correctly
	db := prefixdb.New([]byte{}, memdb.New())

	innerVM.EXPECT().Initialize(
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
	).Return(nil)
	innerVM.EXPECT().Shutdown(gomock.Any()).Return(nil)

	{
		innerBlk := snowmantest.NewMockBlock(ctrl)
		innerBlkID := ids.GenerateTestID()
		innerVM.EXPECT().LastAccepted(gomock.Any()).Return(innerBlkID, nil).Times(2)
		innerVM.EXPECT().GetBlock(gomock.Any(), innerBlkID).Return(innerBlk, nil)
	}

	snowCtx := snowtest.Context(t, snowtest.CChainID)
	snowCtx.NodeID = ids.NodeIDFromCert(pTestCert)

	require.NoError(vm.Initialize(
		context.Background(),
		snowCtx,
		db,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	))
	defer func() {
		require.NoError(vm.Shutdown(context.Background()))
	}()

	signedBlk, err := statelessblock.BuildUnsigned(ids.GenerateTestID(), time.Time{}, 0, []byte("foobar"))
	require.NoError(err)
	statelessBlk := postForkBlock{
		SignedBlock: signedBlk,
	}

	{
		pChainHeight := uint64(0)
		innerBlk := blockWithVerifyContext{
			MockBlock:             snowmantest.NewMockBlock(ctrl),
			MockWithVerifyContext: block.NewMockWithVerifyContext(ctrl),
		}
		innerBlk.MockWithVerifyContext.EXPECT().ShouldVerifyWithContext(gomock.Any()).Return(true, nil).Times(2)
		innerBlk.MockWithVerifyContext.EXPECT().VerifyWithContext(context.Background(),
			&block.Context{
				PChainHeight: pChainHeight,
			},
		).Return(nil)
		innerBlk.MockBlock.EXPECT().Parent().Return(ids.GenerateTestID()).AnyTimes()
		innerBlk.MockBlock.EXPECT().ID().Return(ids.GenerateTestID()).AnyTimes()
		innerBlk.MockBlock.EXPECT().Bytes().Return(utils.RandomBytes(1024)).AnyTimes()

		blk := NewMockPostForkBlock(ctrl)
		blk.EXPECT().getInnerBlk().Return(innerBlk).AnyTimes()
		blkID := ids.GenerateTestID()
		blk.EXPECT().ID().Return(blkID).AnyTimes()
		blk.EXPECT().getStatelessBlk().Return(statelessBlk).Times(2)

		require.NoError(vm.verifyAndRecordInnerBlk(
			context.Background(),
			&block.Context{
				PChainHeight: pChainHeight,
			},
			blk,
		))

		// Call VerifyWithContext again but with a different P-Chain height
		blk.EXPECT().setInnerBlk(innerBlk).AnyTimes()
		pChainHeight++
		innerBlk.MockWithVerifyContext.EXPECT().VerifyWithContext(context.Background(),
			&block.Context{
				PChainHeight: pChainHeight,
			},
		).Return(nil)

		require.NoError(vm.verifyAndRecordInnerBlk(
			context.Background(),
			&block.Context{
				PChainHeight: pChainHeight,
			},
			blk,
		))
	}

	{
		// Ensure we call Verify on a block that returns
		// false for ShouldVerifyWithContext
		innerBlk := blockWithVerifyContext{
			MockBlock:             snowmantest.NewMockBlock(ctrl),
			MockWithVerifyContext: block.NewMockWithVerifyContext(ctrl),
		}
		innerBlk.MockWithVerifyContext.EXPECT().ShouldVerifyWithContext(gomock.Any()).Return(false, nil)
		innerBlk.MockBlock.EXPECT().Verify(gomock.Any()).Return(nil)
		innerBlk.MockBlock.EXPECT().Parent().Return(ids.GenerateTestID()).AnyTimes()
		innerBlk.MockBlock.EXPECT().ID().Return(ids.GenerateTestID()).AnyTimes()
		blk := NewMockPostForkBlock(ctrl)
		blk.EXPECT().getInnerBlk().Return(innerBlk).AnyTimes()
		blkID := ids.GenerateTestID()
		blk.EXPECT().ID().Return(blkID).AnyTimes()
		blk.EXPECT().getStatelessBlk().Return(statelessBlk)
		require.NoError(vm.verifyAndRecordInnerBlk(
			context.Background(),
			&block.Context{
				PChainHeight: 1,
			},
			blk,
		))
	}

	{
		// Ensure we call Verify on a block that doesn't have a valid context
		innerBlk := blockWithVerifyContext{
			MockBlock:             snowmantest.NewMockBlock(ctrl),
			MockWithVerifyContext: block.NewMockWithVerifyContext(ctrl),
		}
		innerBlk.MockBlock.EXPECT().Verify(gomock.Any()).Return(nil)
		innerBlk.MockBlock.EXPECT().Parent().Return(ids.GenerateTestID()).AnyTimes()
		innerBlk.MockBlock.EXPECT().ID().Return(ids.GenerateTestID()).AnyTimes()
		blk := NewMockPostForkBlock(ctrl)
		blk.EXPECT().getInnerBlk().Return(innerBlk).AnyTimes()
		blkID := ids.GenerateTestID()
		blk.EXPECT().ID().Return(blkID).AnyTimes()
		blk.EXPECT().getStatelessBlk().Return(statelessBlk)
		require.NoError(vm.verifyAndRecordInnerBlk(context.Background(), nil, blk))
	}
}

func TestHistoricalBlockDeletion(t *testing.T) {
	require := require.New(t)

	acceptedBlocks := []*snowmantest.Block{snowmantest.Genesis}
	currentHeight := uint64(0)

	initialState := []byte("genesis state")
	coreVM := &block.TestVM{
		TestVM: common.TestVM{
			T: t,
			InitializeF: func(context.Context, *snow.Context, database.Database, []byte, []byte, []byte, chan<- common.Message, []*common.Fx, common.AppSender) error {
				return nil
			},
		},
		LastAcceptedF: func(context.Context) (ids.ID, error) {
			return acceptedBlocks[currentHeight].ID(), nil
		},
		GetBlockF: func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
			for _, blk := range acceptedBlocks {
				if blkID == blk.ID() {
					return blk, nil
				}
			}
			return nil, errUnknownBlock
		},
		ParseBlockF: func(_ context.Context, b []byte) (snowman.Block, error) {
			for _, blk := range acceptedBlocks {
				if bytes.Equal(b, blk.Bytes()) {
					return blk, nil
				}
			}
			return nil, errUnknownBlock
		},
		GetBlockIDAtHeightF: func(_ context.Context, height uint64) (ids.ID, error) {
			if height >= uint64(len(acceptedBlocks)) {
				return ids.ID{}, errTooHigh
			}
			return acceptedBlocks[height].ID(), nil
		},
	}

	ctx := snowtest.Context(t, snowtest.CChainID)
	ctx.NodeID = ids.NodeIDFromCert(pTestCert)
	ctx.ValidatorState = &validators.TestState{
		T: t,
		GetMinimumHeightF: func(context.Context) (uint64, error) {
			return snowmantest.GenesisHeight, nil
		},
		GetCurrentHeightF: func(context.Context) (uint64, error) {
			return defaultPChainHeight, nil
		},
		GetValidatorSetF: func(context.Context, uint64, ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
			return nil, nil
		},
	}

	// make sure that DBs are compressed correctly
	db := prefixdb.New([]byte{}, memdb.New())

	proVM := New(
		coreVM,
		Config{
			ActivationTime:      time.Unix(0, 0),
			DurangoTime:         mockable.MaxTime,
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: DefaultNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		db,
		initialState,
		nil,
		nil,
		nil,
		nil,
		nil,
	))

	lastAcceptedID, err := proVM.LastAccepted(context.Background())
	require.NoError(err)

	require.NoError(proVM.SetState(context.Background(), snow.NormalOp))
	require.NoError(proVM.SetPreference(context.Background(), lastAcceptedID))

	issueBlock := func() {
		lastAcceptedBlock := acceptedBlocks[currentHeight]
		innerBlock := snowmantest.BuildChild(lastAcceptedBlock)

		coreVM.BuildBlockF = func(context.Context) (snowman.Block, error) {
			return innerBlock, nil
		}
		proBlock, err := proVM.BuildBlock(context.Background())
		require.NoError(err)

		require.NoError(proBlock.Verify(context.Background()))
		require.NoError(proVM.SetPreference(context.Background(), proBlock.ID()))
		require.NoError(proBlock.Accept(context.Background()))

		acceptedBlocks = append(acceptedBlocks, innerBlock)
		currentHeight++
	}

	requireHeights := func(start, end uint64) {
		for i := start; i <= end; i++ {
			_, err := proVM.GetBlockIDAtHeight(context.Background(), i)
			require.NoError(err)
		}
	}

	requireMissingHeights := func(start, end uint64) {
		for i := start; i <= end; i++ {
			_, err := proVM.GetBlockIDAtHeight(context.Background(), i)
			require.ErrorIs(err, database.ErrNotFound)
		}
	}

	requireNumHeights := func(numIndexed uint64) {
		requireHeights(0, 0)
		requireMissingHeights(1, currentHeight-numIndexed-1)
		requireHeights(currentHeight-numIndexed, currentHeight)
	}

	// Because block pruning is disabled by default, the heights should be
	// populated for every accepted block.
	requireHeights(0, currentHeight)

	issueBlock()
	requireHeights(0, currentHeight)

	issueBlock()
	requireHeights(0, currentHeight)

	issueBlock()
	requireHeights(0, currentHeight)

	issueBlock()
	requireHeights(0, currentHeight)

	issueBlock()
	requireHeights(0, currentHeight)

	require.NoError(proVM.Shutdown(context.Background()))

	numHistoricalBlocks := uint64(2)
	proVM = New(
		coreVM,
		Config{
			ActivationTime:      time.Time{},
			DurangoTime:         mockable.MaxTime,
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: numHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		db,
		initialState,
		nil,
		nil,
		nil,
		nil,
		nil,
	))

	lastAcceptedID, err = proVM.LastAccepted(context.Background())
	require.NoError(err)

	require.NoError(proVM.SetState(context.Background(), snow.NormalOp))
	require.NoError(proVM.SetPreference(context.Background(), lastAcceptedID))

	// Verify that old blocks were pruned during startup
	requireNumHeights(numHistoricalBlocks)

	// As we issue new blocks, the oldest indexed height should be pruned.
	issueBlock()
	requireNumHeights(numHistoricalBlocks)

	issueBlock()
	requireNumHeights(numHistoricalBlocks)

	require.NoError(proVM.Shutdown(context.Background()))

	newNumHistoricalBlocks := numHistoricalBlocks + 2
	proVM = New(
		coreVM,
		Config{
			ActivationTime:      time.Time{},
			DurangoTime:         mockable.MaxTime,
			MinimumPChainHeight: 0,
			MinBlkDelay:         DefaultMinBlockDelay,
			NumHistoricalBlocks: newNumHistoricalBlocks,
			StakingLeafSigner:   pTestSigner,
			StakingCertLeaf:     pTestCert,
		},
	)

	require.NoError(proVM.Initialize(
		context.Background(),
		ctx,
		db,
		initialState,
		nil,
		nil,
		nil,
		nil,
		nil,
	))
	defer func() {
		require.NoError(proVM.Shutdown(context.Background()))
	}()

	lastAcceptedID, err = proVM.LastAccepted(context.Background())
	require.NoError(err)

	require.NoError(proVM.SetState(context.Background(), snow.NormalOp))
	require.NoError(proVM.SetPreference(context.Background(), lastAcceptedID))

	// The height index shouldn't be modified at this point
	requireNumHeights(numHistoricalBlocks)

	// As we issue new blocks, the number of indexed blocks should increase
	// until we hit our target again.
	issueBlock()
	requireNumHeights(numHistoricalBlocks + 1)

	issueBlock()
	requireNumHeights(newNumHistoricalBlocks)

	issueBlock()
	requireNumHeights(newNumHistoricalBlocks)
}

// Tests end-to-end flow for block preferences. Blocks should be persisted upon
// verification and deleted upon accept/reject.
func TestBlockDecision(t *testing.T) {
	innerGenesisBlk := &snowman.TestBlock{
		TestDecidable: choices.TestDecidable{
			StatusV: choices.Accepted,
		},
		HeightV: 0,
		BytesV:  []byte("genesis"),
	}
	innerBuiltBlk := &snowman.TestBlock{
		TestDecidable: choices.TestDecidable{
			IDV:     ids.GenerateTestID(),
			StatusV: choices.Processing,
		},
		HeightV: 1,
		BytesV:  []byte("block 1"),
	}

	tests := []struct {
		name   string
		reject bool
	}{
		{
			name: "accept",
		},
		{
			name:   "reject",
			reject: true,
		},
	}

	for _, tt := range tests {
		require := require.New(t)
		ctx := context.Background()

		vm := &block.TestVM{
			TestVM: common.TestVM{
				InitializeF: func(
					context.Context,
					*snow.Context,
					database.Database,
					[]byte,
					[]byte,
					[]byte,
					chan<- common.Message,
					[]*common.Fx,
					common.AppSender,
				) error {
					return nil
				},
			},
			BuildBlockF: func(context.Context) (snowman.Block, error) {
				return innerBuiltBlk, nil
			},
			LastAcceptedF: func(context.Context) (ids.ID, error) {
				return innerGenesisBlk.ID(), nil
			},
			GetBlockF: func(_ context.Context, blkID ids.ID) (snowman.Block, error) {
				switch blkID {
				case innerGenesisBlk.ID():
					return innerGenesisBlk, nil
				case innerBuiltBlk.ID():
					return innerBuiltBlk, nil
				default:
					return nil, fmt.Errorf("%w: %s", database.ErrNotFound, blkID)
				}
			},
			ParseBlockF: func(_ context.Context, blkBytes []byte) (snowman.Block, error) {
				switch {
				case bytes.Equal(blkBytes, innerGenesisBlk.Bytes()):
					return innerGenesisBlk, nil
				case bytes.Equal(blkBytes, innerBuiltBlk.Bytes()):
					return innerBuiltBlk, nil
				default:
					return nil, errors.New("unexpected block")
				}
			},
		}

		config := Config{}
		chainID := ids.GenerateTestID()
		chainCtx := snowtest.Context(t, chainID)
		chainCtx.ValidatorState = &validators.TestState{
			GetMinimumHeightF: func(context.Context) (uint64, error) {
				return 0, nil
			},
			GetCurrentHeightF: func(context.Context) (uint64, error) {
				return 0, nil
			},
			GetValidatorSetF: func(context.Context, uint64, ids.ID) (map[ids.NodeID]*validators.GetValidatorOutput, error) {
				return map[ids.NodeID]*validators.GetValidatorOutput{}, nil
			},
		}

		db := memdb.New()

		proposerVM := New(vm, config)
		require.NoError(proposerVM.Initialize(
			ctx,
			chainCtx,
			db,
			[]byte("genesis"),
			[]byte("upgrade"),
			[]byte("config"),
			nil,
			nil,
			nil,
		))

		genesisBlk, err := statelessblock.BuildUnsigned(
			ids.Empty,
			time.Time{},
			0,
			innerGenesisBlk.Bytes(),
		)
		require.NoError(err)
		require.NoError(proposerVM.PutBlock(genesisBlk, choices.Accepted))
		require.NoError(proposerVM.SetPreference(ctx, genesisBlk.ID()))
		require.NoError(proposerVM.SetState(ctx, snow.NormalOp))

		builtBlk, err := proposerVM.BuildBlock(ctx)
		require.NoError(err)

		require.NoError(builtBlk.Verify(ctx))
		gotBlk, err := proposerVM.GetBlock(ctx, builtBlk.ID())
		require.NoError(err)
		require.Equal(choices.Processing, gotBlk.Status())

		ok, err := proposerVM.HasProcessingBlock(builtBlk.ID())
		require.NoError(err)
		require.True(ok)

		require.NoError(proposerVM.Shutdown(ctx))

		// Restart the VM
		proposerVM = New(vm, config)
		require.NoError(proposerVM.Initialize(
			ctx,
			chainCtx,
			db,
			[]byte("genesis"),
			[]byte("upgrade"),
			[]byte("config"),
			nil,
			nil,
			nil,
		))

		builtBlk, err = proposerVM.BuildBlock(ctx)
		require.NoError(err)

		require.NoError(builtBlk.Verify(ctx))
		gotBlk, err = proposerVM.GetBlock(ctx, builtBlk.ID())
		require.NoError(err)
		require.Equal(choices.Processing, gotBlk.Status())

		if tt.reject {
			require.NoError(builtBlk.Reject(ctx))
		} else {
			require.NoError(builtBlk.Accept(ctx))
		}

		ok, err = proposerVM.HasProcessingBlock(builtBlk.ID())
		require.NoError(err)
		require.False(ok)

		if tt.reject {
			require.NoError(builtBlk.Reject(ctx))
			_, err = proposerVM.GetBlock(ctx, builtBlk.ID())
			require.ErrorIs(err, database.ErrNotFound)
		} else {
			require.NoError(builtBlk.Accept(ctx))
			gotBlk, err = proposerVM.GetBlock(ctx, builtBlk.ID())
			require.NoError(err, nil)
			require.Equal(choices.Accepted, gotBlk.Status())
		}

		require.NoError(proposerVM.Shutdown(ctx))
	}
}

// The VM should set its initial preference to its previous preference
func TestProposerVMInitialPreference(t *testing.T) {
	cert, err := staking.NewTLSCert()
	require.NoError(t, err)
	stakingCert, err := staking.ParseCertificate(cert.Leaf.Raw)
	require.NoError(t, err)
	signer := cert.PrivateKey.(crypto.Signer)

	// Create a blockchain with one accepted block and one processing block
	//	   [0]
	//		|
	//	  *[1]*
	blk0, err := statelessblock.Build(
		ids.GenerateTestID(),
		time.Time{},
		0,
		stakingCert,
		[]byte{0},
		ids.Empty,
		signer,
	)

	blk1, err := statelessblock.Build(
		blk0.ID(),
		time.Time{},
		0,
		stakingCert,
		[]byte{0},
		ids.Empty,
		signer,
	)

	tests := []struct {
		name                    string
		lastAccepted            statelessblock.Block
		initialPreference       statelessblock.Block
		initialPreferenceStatus choices.Status
		wantInitialPreferenceID ids.ID
	}{
		{
			// If there is no prior preference we should default to the last
			// accepted block
			name:                    "previous preference is last accepted",
			lastAccepted:            blk0,
			initialPreference:       blk0,
			initialPreferenceStatus: choices.Accepted,
			wantInitialPreferenceID: blk0.ID(),
		},
		{
			// If there is a prior preference we should prefer it
			name:                    "previous preference is not last accepted",
			lastAccepted:            blk0,
			initialPreference:       blk1,
			initialPreferenceStatus: choices.Processing,
			wantInitialPreferenceID: blk1.ID(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			ctx := context.Background()

			innerVM := &block.TestVM{
				TestVM: common.TestVM{
					InitializeF: func(
						context.Context,
						*snow.Context,
						database.Database,
						[]byte,
						[]byte,
						[]byte,
						chan<- common.Message,
						[]*common.Fx,
						common.AppSender,
					) error {
						return nil
					},
				},
				LastAcceptedF: func(context.Context) (ids.ID, error) {
					return tt.lastAccepted.ID(), nil
				},
				GetBlockF: func(context.Context, ids.ID) (snowman.Block, error) {
					return nil, nil
				},
			}

			chainCtx := snowtest.Context(t, ids.GenerateTestID())
			db := memdb.New()

			vm := New(innerVM, Config{})
			require.NoError(vm.Initialize(
				ctx,
				chainCtx,
				db,
				nil,
				nil,
				nil,
				make(chan common.Message),
				nil,
				&common.SenderTest{},
			))

			require.NoError(vm.State.PutBlock(tt.lastAccepted, choices.Accepted))

			require.NoError(vm.State.PutBlock(tt.initialPreference, tt.initialPreferenceStatus))
			require.NoError(vm.State.SetPreference(tt.initialPreference.ID()))

			require.NoError(vm.Shutdown(ctx))

			vm = New(innerVM, Config{})
			require.NoError(vm.Initialize(
				ctx,
				chainCtx,
				db,
				nil,
				nil,
				nil,
				make(chan common.Message),
				nil,
				&common.SenderTest{},
			))

			got, err := vm.GetInitialPreference(ctx)
			require.NoError(err)
			require.Equal(tt.wantInitialPreferenceID, got)

			require.NoError(vm.Shutdown(ctx))
		})
	}
}

func TestOnlyPreferredOptionPersisted(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()

	chainCtx := snowtest.Context(t, ids.GenerateTestID())
	chainCtx.ValidatorState = &validators.TestState{
		GetCurrentHeightF: func(context.Context) (uint64, error) {
			return 0, nil
		},
	}

	innerVM := &block.TestVM{
		TestVM: common.TestVM{
			InitializeF: func(
				context.Context,
				*snow.Context,
				database.Database,
				[]byte,
				[]byte,
				[]byte,
				chan<- common.Message,
				[]*common.Fx,
				common.AppSender,
			) error {
				return nil
			},
		},
	}
	vm := New(innerVM, Config{})

	blk0 := &snowman.TestBlock{}

	outerBlk1, err := statelessblock.BuildUnsigned(
		blk0.ID(),
		time.Time{},
		0,
		[]byte("foobar"),
	)
	require.NoError(err)

	innerBlk1 := &snowman.TestOracleBlock{
		TestBlock: &snowman.TestBlock{
			TestDecidable: choices.TestDecidable{
				IDV:     ids.GenerateTestID(),
				StatusV: choices.Processing,
			},
			ParentV: blk0.ID(),
			HeightV: blk0.HeightV + 1,
			BytesV:  []byte{0},
		},
	}

	innerBlk1.OptionsV = [2]snowman.Block{
		&snowman.TestBlock{
			TestDecidable: choices.TestDecidable{
				IDV:     ids.GenerateTestID(),
				StatusV: choices.Processing,
			},
			ParentV: innerBlk1.IDV,
			HeightV: innerBlk1.HeightV + 1,
			BytesV:  []byte{1},
		},
		&snowman.TestBlock{
			TestDecidable: choices.TestDecidable{
				IDV:     ids.GenerateTestID(),
				StatusV: choices.Processing,
			},
			ParentV: innerBlk1.IDV,
			HeightV: innerBlk1.HeightV + 1,
			BytesV:  []byte{2},
		},
	}

	blk1 := &postForkBlock{
		SignedBlock: outerBlk1,
		postForkCommonComponents: postForkCommonComponents{
			vm:       vm,
			outerBlk: outerBlk1,
			innerBlk: innerBlk1,
			status:   choices.Processing,
		},
	}

	innerVM.LastAcceptedF = func(context.Context) (ids.ID, error) {
		return blk0.ID(), nil
	}

	innerVM.GetBlockF = func(ctx context.Context, blkID ids.ID) (snowman.Block, error) {
		switch blkID {
		case blk0.ID():
			return blk0, nil
		default:
			return nil, fmt.Errorf("unexpected block")
		}
	}

	db := memdb.New()
	require.NoError(vm.Initialize(
		ctx,
		chainCtx,
		db,
		nil,
		nil,
		nil,
		make(chan common.Message),
		nil,
		&common.SenderTest{},
	))

	require.NoError(blk1.Verify(ctx))

	options, err := blk1.Options(ctx)
	optionBlk0 := options[0]
	optionBlk1 := options[1]

	require.NoError(optionBlk0.Verify(ctx))
	require.NoError(optionBlk1.Verify(ctx))

	// invariant: we prefer the first option
	require.NoError(vm.SetPreference(ctx, optionBlk0.ID()))

	// invariant: we only store the preferred chain
	require.True(vm.HasProcessingBlock(blk1.ID()))
	require.True(vm.HasProcessingBlock(optionBlk0.ID()))
	require.False(vm.HasProcessingBlock(optionBlk1.ID()))

	// changing preferences should still persist the correct chain
	require.NoError(vm.SetPreference(ctx, optionBlk1.ID()))

	require.True(vm.HasProcessingBlock(blk1.ID()))
	require.False(vm.HasProcessingBlock(optionBlk0.ID()))
	require.True(vm.HasProcessingBlock(optionBlk1.ID()))

	require.NoError(vm.Shutdown(ctx))
}
