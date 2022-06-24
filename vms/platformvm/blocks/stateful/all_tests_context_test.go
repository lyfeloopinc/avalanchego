// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package stateful

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/chains"
	"github.com/ava-labs/avalanchego/chains/atomic"
	"github.com/ava-labs/avalanchego/codec"
	"github.com/ava-labs/avalanchego/codec/linearcodec"
	"github.com/ava-labs/avalanchego/database/manager"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/database/versiondb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/uptime"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto"
	"github.com/ava-labs/avalanchego/utils/formatting"
	"github.com/ava-labs/avalanchego/utils/formatting/address"
	"github.com/ava-labs/avalanchego/utils/json"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/timer/mockable"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/utils/window"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/platformvm/api"
	"github.com/ava-labs/avalanchego/vms/platformvm/config"
	"github.com/ava-labs/avalanchego/vms/platformvm/fx"
	"github.com/ava-labs/avalanchego/vms/platformvm/reward"
	"github.com/ava-labs/avalanchego/vms/platformvm/state"
	"github.com/ava-labs/avalanchego/vms/platformvm/status"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs/executor"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs/mempool"
	"github.com/ava-labs/avalanchego/vms/platformvm/utxos"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/golang/mock/gomock"
	"github.com/prometheus/client_golang/prometheus"

	p_metrics "github.com/ava-labs/avalanchego/vms/platformvm/metrics"
	tx_builder "github.com/ava-labs/avalanchego/vms/platformvm/txs/builder"
)

var (
	_ mempool.BlockTimer = &testHelpersCollection{}

	defaultMinStakingDuration = 24 * time.Hour
	defaultMaxStakingDuration = 365 * 24 * time.Hour
	defaultGenesisTime        = time.Date(1997, 1, 1, 0, 0, 0, 0, time.UTC)
	defaultValidateStartTime  = defaultGenesisTime
	defaultValidateEndTime    = defaultValidateStartTime.Add(10 * defaultMinStakingDuration)
	defaultMinValidatorStake  = 5 * units.MilliAvax
	defaultBalance            = 100 * defaultMinValidatorStake
	preFundedKeys             []*crypto.PrivateKeySECP256K1R
	avaxAssetID               = ids.ID{'y', 'e', 'e', 't'}
	defaultTxFee              = uint64(100)
	xChainID                  = ids.Empty.Prefix(0)
	cChainID                  = ids.Empty.Prefix(1)
	testSubnet1               *txs.Tx
	testSubnet1ControlKeys    []*crypto.PrivateKeySECP256K1R
)

const (
	testNetworkID                 = 10 // To be used in tests
	defaultWeight                 = 10000
	maxRecentlyAcceptedWindowSize = 256
	recentlyAcceptedWindowTTL     = 5 * time.Minute
)

type testHelpersCollection struct {
	blkVerifier Verifier
	mpool       mempool.Mempool
	sender      *common.SenderTest

	isBootstrapped  *utils.AtomicBool
	cfg             *config.Config
	clk             *mockable.Clock
	baseDB          *versiondb.Database
	ctx             *snow.Context
	fx              fx.Fx
	fullState       state.State
	mockedFullState *state.MockState
	atomicUtxosMan  avax.AtomicUTXOManager
	uptimeMan       uptime.Manager
	utxosMan        utxos.SpendHandler
	txBuilder       tx_builder.Builder
	txExecBackend   executor.Backend
}

func (t *testHelpersCollection) ResetBlockTimer() {
	// dummy call, do nothing for now
}

// TODO snLookup currently duplicated in vm_test.go. Consider removing duplication
type snLookup struct {
	chainsToSubnet map[ids.ID]ids.ID
}

func (sn *snLookup) SubnetID(chainID ids.ID) (ids.ID, error) {
	subnetID, ok := sn.chainsToSubnet[chainID]
	if !ok {
		return ids.ID{}, errors.New("")
	}
	return subnetID, nil
}

func init() {
	preFundedKeys = defaultKeys()
	testSubnet1ControlKeys = preFundedKeys[0:3]
}

func newTestHelpersCollection(t *testing.T, ctrl *gomock.Controller) *testHelpersCollection {
	var (
		res = &testHelpersCollection{}
		err error
	)

	res.isBootstrapped = &utils.AtomicBool{}
	res.isBootstrapped.SetValue(true)

	res.cfg = defaultCfg()
	res.clk = defaultClock()

	baseDBManager := manager.NewMemDB(version.DefaultVersion1_0_0)
	res.baseDB = versiondb.New(baseDBManager.Current().Database)
	res.ctx = defaultCtx(res.baseDB)
	res.fx = defaultFx(res.clk, res.ctx.Log, res.isBootstrapped.GetValue())

	rewardsCalc := reward.NewCalculator(res.cfg.RewardConfig)
	res.atomicUtxosMan = avax.NewAtomicUTXOManager(res.ctx.SharedMemory, txs.Codec)

	if ctrl == nil {
		res.fullState = defaultState(res.cfg, res.ctx, res.baseDB, rewardsCalc)
		res.uptimeMan = uptime.NewManager(res.fullState)
		res.utxosMan = utxos.NewHandler(res.ctx, *res.clk, res.fullState, res.fx)
		res.txBuilder = tx_builder.New(
			res.ctx,
			*res.cfg,
			*res.clk,
			res.fx,
			res.fullState,
			res.atomicUtxosMan,
			res.utxosMan,
			rewardsCalc,
		)
	} else {
		res.mockedFullState = state.NewMockState(ctrl)
		res.uptimeMan = uptime.NewManager(res.mockedFullState)
		res.utxosMan = utxos.NewHandler(res.ctx, *res.clk, res.mockedFullState, res.fx)
		res.txBuilder = tx_builder.New(
			res.ctx,
			*res.cfg,
			*res.clk,
			res.fx,
			res.mockedFullState,
			res.atomicUtxosMan,
			res.utxosMan,
			rewardsCalc,
		)
	}

	res.txExecBackend = executor.Backend{
		Cfg:          res.cfg,
		Ctx:          res.ctx,
		Clk:          res.clk,
		Bootstrapped: res.isBootstrapped,
		Fx:           res.fx,
		SpendHandler: res.utxosMan,
		UptimeMan:    res.uptimeMan,
		Rewards:      rewardsCalc,
	}

	registerer := prometheus.NewRegistry()
	window := window.New(
		window.Config{
			Clock:   res.clk,
			MaxSize: maxRecentlyAcceptedWindowSize,
			TTL:     recentlyAcceptedWindowTTL,
		},
	)
	res.sender = &common.SenderTest{T: t}

	metrics, err := p_metrics.NewMetrics("", registerer, res.cfg.WhitelistedSubnets)
	if err != nil {
		panic(fmt.Errorf("failed to create metrics: %w", err))
	}

	res.mpool, err = mempool.NewMempool("mempool", registerer, res)
	if err != nil {
		panic(fmt.Errorf("failed to create mempool: %w", err))
	}

	if ctrl == nil {
		res.blkVerifier = NewBlockVerifier(
			res.mpool,
			res.fullState,
			res.txExecBackend,
			metrics,
			window,
		)
		addSubnet(res.fullState, res.txBuilder, res.txExecBackend)
	} else {
		res.blkVerifier = NewBlockVerifier(
			res.mpool,
			res.mockedFullState,
			res.txExecBackend,
			metrics,
			window,
		)
		// we do not add any subnet to state, since we can mock
		// whatever we need
	}

	return res
}

func addSubnet(
	tState state.State,
	txBuilder tx_builder.Builder,
	txExecBackend executor.Backend,
) {
	// Create a subnet
	var err error
	testSubnet1, err = txBuilder.NewCreateSubnetTx(
		2, // threshold; 2 sigs from keys[0], keys[1], keys[2] needed to add validator to this subnet
		[]ids.ShortID{ // control keys
			preFundedKeys[0].PublicKey().Address(),
			preFundedKeys[1].PublicKey().Address(),
			preFundedKeys[2].PublicKey().Address(),
		},
		[]*crypto.PrivateKeySECP256K1R{preFundedKeys[0]},
		preFundedKeys[0].PublicKey().Address(),
	)
	if err != nil {
		panic(err)
	}

	// store it
	versionedState := state.NewDiff(
		tState,
		tState.CurrentStakers(),
		tState.PendingStakers(),
	)

	executor := executor.StandardTxExecutor{
		Backend: &txExecBackend,
		State:   versionedState,
		Tx:      testSubnet1,
	}
	err = testSubnet1.Unsigned.Visit(&executor)
	if err != nil {
		panic(err)
	}
	versionedState.AddTx(testSubnet1, status.Committed)
	versionedState.Apply(tState)
}

func defaultState(
	cfg *config.Config,
	ctx *snow.Context,
	baseDB *versiondb.Database,
	rewardsCalc reward.Calculator,
) state.State {
	dummyLocalStake := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "uts",
		Name:      "local_staked",
		Help:      "Total amount of AVAX on this node staked",
	})
	dummyTotalStake := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "uts",
		Name:      "total_staked",
		Help:      "Total amount of AVAX staked",
	})

	genesisBytes := buildGenesisTest(ctx)
	tState, err := state.New(
		baseDB,
		prometheus.NewRegistry(),
		cfg,
		ctx,
		dummyLocalStake,
		dummyTotalStake,
		rewardsCalc,
		genesisBytes,
	)
	if err != nil {
		panic(err)
	}
	return tState
}

func defaultCtx(baseDB *versiondb.Database) *snow.Context {
	ctx := snow.DefaultContextTest()
	ctx.NetworkID = 10
	ctx.XChainID = xChainID
	ctx.AVAXAssetID = avaxAssetID

	atomicDB := prefixdb.New([]byte{1}, baseDB)
	m := &atomic.Memory{}
	err := m.Initialize(logging.NoLog{}, atomicDB)
	if err != nil {
		panic(err)
	}

	ctx.SharedMemory = m.NewSharedMemory(ctx.ChainID)

	ctx.SNLookup = &snLookup{
		chainsToSubnet: map[ids.ID]ids.ID{
			constants.PlatformChainID: constants.PrimaryNetworkID,
			xChainID:                  constants.PrimaryNetworkID,
			cChainID:                  constants.PrimaryNetworkID,
		},
	}

	return ctx
}

func defaultCfg() *config.Config {
	return &config.Config{
		Chains:                 chains.MockManager{},
		UptimeLockedCalculator: uptime.NewLockedCalculator(),
		Validators:             validators.NewManager(),
		TxFee:                  defaultTxFee,
		CreateSubnetTxFee:      100 * defaultTxFee,
		CreateBlockchainTxFee:  100 * defaultTxFee,
		MinValidatorStake:      5 * units.MilliAvax,
		MaxValidatorStake:      500 * units.MilliAvax,
		MinDelegatorStake:      1 * units.MilliAvax,
		MinStakeDuration:       defaultMinStakingDuration,
		MaxStakeDuration:       defaultMaxStakingDuration,
		RewardConfig: reward.Config{
			MaxConsumptionRate: .12 * reward.PercentDenominator,
			MinConsumptionRate: .10 * reward.PercentDenominator,
			MintingPeriod:      365 * 24 * time.Hour,
			SupplyCap:          720 * units.MegaAvax,
		},
		ApricotPhase3Time: defaultValidateEndTime,
		ApricotPhase4Time: defaultValidateEndTime,
		ApricotPhase5Time: defaultValidateEndTime,
		ApricotPhase6Time: mockable.MaxTime,
	}
}

func defaultClock() *mockable.Clock {
	clk := mockable.Clock{}
	clk.Set(defaultGenesisTime)
	return &clk
}

type fxVMInt struct {
	registry codec.Registry
	clk      *mockable.Clock
	log      logging.Logger
}

func (fvi *fxVMInt) CodecRegistry() codec.Registry { return fvi.registry }
func (fvi *fxVMInt) Clock() *mockable.Clock        { return fvi.clk }
func (fvi *fxVMInt) Logger() logging.Logger        { return fvi.log }

func defaultFx(clk *mockable.Clock, log logging.Logger, isBootstrapped bool) fx.Fx {
	fxVMInt := &fxVMInt{
		registry: linearcodec.NewDefault(),
		clk:      clk,
		log:      log,
	}
	res := &secp256k1fx.Fx{}
	if err := res.Initialize(fxVMInt); err != nil {
		panic(err)
	}
	if isBootstrapped {
		if err := res.Bootstrapped(); err != nil {
			panic(err)
		}
	}
	return res
}

func defaultKeys() []*crypto.PrivateKeySECP256K1R {
	dummyCtx := snow.DefaultContextTest()
	res := make([]*crypto.PrivateKeySECP256K1R, 0)
	factory := crypto.FactorySECP256K1R{}
	for _, key := range []string{
		"24jUJ9vZexUM6expyMcT48LBx27k1m7xpraoV62oSQAHdziao5",
		"2MMvUMsxx6zsHSNXJdFD8yc5XkancvwyKPwpw4xUK3TCGDuNBY",
		"cxb7KpGWhDMALTjNNSJ7UQkkomPesyWAPUaWRGdyeBNzR6f35",
		"ewoqjP7PxY4yr3iLTpLisriqt94hdyDFNgchSxGGztUrTXtNN",
		"2RWLv6YVEXDiWLpaCbXhhqxtLbnFaKQsWPSSMSPhpWo47uJAeV",
	} {
		privKeyBytes, err := formatting.Decode(formatting.CB58, key)
		dummyCtx.Log.AssertNoError(err)
		pk, err := factory.ToPrivateKey(privKeyBytes)
		dummyCtx.Log.AssertNoError(err)
		res = append(res, pk.(*crypto.PrivateKeySECP256K1R))
	}
	return res
}

func buildGenesisTest(ctx *snow.Context) []byte {
	genesisUTXOs := make([]api.UTXO, len(preFundedKeys))
	hrp := constants.NetworkIDToHRP[testNetworkID]
	for i, key := range preFundedKeys {
		id := key.PublicKey().Address()
		addr, err := address.FormatBech32(hrp, id.Bytes())
		if err != nil {
			panic(err)
		}
		genesisUTXOs[i] = api.UTXO{
			Amount:  json.Uint64(defaultBalance),
			Address: addr,
		}
	}

	genesisValidators := make([]api.PrimaryValidator, len(preFundedKeys))
	for i, key := range preFundedKeys {
		nodeID := ids.NodeID(key.PublicKey().Address())
		addr, err := address.FormatBech32(hrp, nodeID.Bytes())
		if err != nil {
			panic(err)
		}
		genesisValidators[i] = api.PrimaryValidator{
			Staker: api.Staker{
				StartTime: json.Uint64(defaultValidateStartTime.Unix()),
				EndTime:   json.Uint64(defaultValidateEndTime.Unix()),
				NodeID:    nodeID,
			},
			RewardOwner: &api.Owner{
				Threshold: 1,
				Addresses: []string{addr},
			},
			Staked: []api.UTXO{{
				Amount:  json.Uint64(defaultWeight),
				Address: addr,
			}},
			DelegationFee: reward.PercentDenominator,
		}
	}

	buildGenesisArgs := api.BuildGenesisArgs{
		NetworkID:     json.Uint32(testNetworkID),
		AvaxAssetID:   ctx.AVAXAssetID,
		UTXOs:         genesisUTXOs,
		Validators:    genesisValidators,
		Chains:        nil,
		Time:          json.Uint64(defaultGenesisTime.Unix()),
		InitialSupply: json.Uint64(360 * units.MegaAvax),
		Encoding:      formatting.CB58,
	}

	buildGenesisResponse := api.BuildGenesisReply{}
	platformvmSS := api.StaticService{}
	if err := platformvmSS.BuildGenesis(nil, &buildGenesisArgs, &buildGenesisResponse); err != nil {
		panic(fmt.Errorf("problem while building platform chain's genesis state: %v", err))
	}

	genesisBytes, err := formatting.Decode(buildGenesisResponse.Encoding, buildGenesisResponse.Bytes)
	if err != nil {
		panic(err)
	}

	return genesisBytes
}

func internalStateShutdown(t *testHelpersCollection) error {
	if t.mockedFullState != nil {
		// state is mocked, nothing to do here
		return nil
	}

	if t.isBootstrapped.GetValue() {
		primaryValidatorSet, exist := t.cfg.Validators.GetValidators(constants.PrimaryNetworkID)
		if !exist {
			return errors.New("no default subnet validators")
		}
		primaryValidators := primaryValidatorSet.List()

		validatorIDs := make([]ids.NodeID, len(primaryValidators))
		for i, vdr := range primaryValidators {
			validatorIDs[i] = vdr.ID()
		}

		if err := t.uptimeMan.Shutdown(validatorIDs); err != nil {
			return err
		}
		if err := t.fullState.Commit(); err != nil {
			return err
		}
	}

	errs := wrappers.Errs{}
	if t.fullState != nil {
		errs.Add(t.fullState.Close())
	}
	errs.Add(t.baseDB.Close())
	return errs.Err
}
