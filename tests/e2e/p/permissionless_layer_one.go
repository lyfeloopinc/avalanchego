// Copyright (C) 2019-2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package p

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/ava-labs/avalanchego/api/info"
	"github.com/ava-labs/avalanchego/config"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/network/peer"
	"github.com/ava-labs/avalanchego/proto/pb/sdk"
	"github.com/ava-labs/avalanchego/snow/networking/router"
	"github.com/ava-labs/avalanchego/tests/fixture/e2e"
	"github.com/ava-labs/avalanchego/tests/fixture/tmpnet"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/buffer"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/example/xsvm/genesis"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"

	p2pmessage "github.com/ava-labs/avalanchego/message"
	p2psdk "github.com/ava-labs/avalanchego/network/p2p"
	p2ppb "github.com/ava-labs/avalanchego/proto/pb/p2p"
	snowvalidators "github.com/ava-labs/avalanchego/snow/validators"
	platformvmsdk "github.com/ava-labs/avalanchego/vms/platformvm"
	platformvmvalidators "github.com/ava-labs/avalanchego/vms/platformvm/validators"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
)

const (
	genesisWeight   = units.Schmeckle
	genesisBalance  = units.Avax
	registerWeight  = genesisWeight / 10
	updatedWeight   = 2 * registerWeight
	registerBalance = 0
)

var _ = e2e.DescribePChain("[Permissionless L1]", func() {
	tc := e2e.NewTestContext()
	require := require.New(tc)

	ginkgo.It("creates and updates Permissionless L1", func() {
		env := e2e.GetEnv(tc)
		nodeURI := env.GetRandomNodeURI()

		tc.By("verifying Etna is activated", func() {
			infoClient := info.NewClient(nodeURI.URI)
			upgrades, err := infoClient.Upgrades(tc.DefaultContext())
			require.NoError(err)

			now := time.Now()
			if !upgrades.IsEtnaActivated(now) {
				ginkgo.Skip("Etna is not activated. Permissionless L1s are enabled post-Etna, skipping test.")
			}
		})

		tc.By("loading the wallet")
		var (
			keychain   = env.NewKeychain()
			baseWallet = e2e.NewWallet(tc, keychain, nodeURI)
			pWallet    = baseWallet.P()
			pClient    = platformvmsdk.NewClient(nodeURI.URI)
			owner      = &secp256k1fx.OutputOwners{
				Threshold: 1,
				Addrs: []ids.ShortID{
					keychain.Keys[0].Address(),
				},
			}
		)

		tc.By("creating the chain genesis")
		genesisKey, err := secp256k1.NewPrivateKey()
		require.NoError(err)

		genesisBytes, err := genesis.Codec.Marshal(genesis.CodecVersion, &genesis.Genesis{
			Timestamp: time.Now().Unix(),
			Allocations: []genesis.Allocation{
				{
					Address: genesisKey.Address(),
					Balance: math.MaxUint64,
				},
			},
		})
		require.NoError(err)

		var subnetID ids.ID
		tc.By("issuing a CreateSubnetTx", func() {
			subnetTx, err := pWallet.IssueCreateSubnetTx(
				owner,
				tc.WithDefaultContext(),
			)
			require.NoError(err)

			subnetID = subnetTx.ID()
		})

		tc.By("verifying a Permissioned Subnet was successfully created", func() {
			require.NotEqual(constants.PrimaryNetworkID, subnetID)

			subnet, err := pClient.GetSubnet(tc.DefaultContext(), subnetID)
			require.NoError(err)
			require.Equal(
				platformvmsdk.GetSubnetClientResponse{
					IsPermissioned: true,
					ControlKeys: []ids.ShortID{
						keychain.Keys[0].Address(),
					},
					Threshold: 1,
				},
				subnet,
			)
		})

		var chainID ids.ID
		tc.By("issuing a CreateChainTx", func() {
			chainTx, err := pWallet.IssueCreateChainTx(
				subnetID,
				genesisBytes,
				constants.XSVMID,
				nil,
				"No Permissions",
				tc.WithDefaultContext(),
			)
			require.NoError(err)

			chainID = chainTx.ID()
		})

		tc.By("creating the genesis validator")
		subnetGenesisNode := e2e.AddEphemeralNode(tc, env.GetNetwork(), tmpnet.FlagsMap{
			config.TrackSubnetsKey: subnetID.String(),
		})

		genesisNodePoP, err := subnetGenesisNode.GetProofOfPossession()
		require.NoError(err)

		genesisNodePK, err := bls.PublicKeyFromCompressedBytes(genesisNodePoP.PublicKey[:])
		require.NoError(err)

		tc.By("connecting to the genesis validator")
		var (
			networkID           = env.GetNetwork().GetNetworkID()
			genesisPeerMessages = buffer.NewUnboundedBlockingDeque[p2pmessage.InboundMessage](1)
		)
		genesisPeer, err := peer.StartTestPeer(
			tc.DefaultContext(),
			subnetGenesisNode.StakingAddress,
			networkID,
			router.InboundHandlerFunc(func(_ context.Context, m p2pmessage.InboundMessage) {
				tc.Outf("received %s %s from %s\n", m.Op(), m.Message(), m.NodeID())
				genesisPeerMessages.PushRight(m)
			}),
		)
		require.NoError(err)
		defer func() {
			genesisPeerMessages.Close()
			genesisPeer.StartClose()
			require.NoError(genesisPeer.AwaitClosed(tc.DefaultContext()))
		}()

		address := []byte{}
		tc.By("issuing a ConvertSubnetTx", func() {
			_, err := pWallet.IssueConvertSubnetTx(
				subnetID,
				chainID,
				address,
				[]*txs.ConvertSubnetValidator{
					{
						NodeID:  subnetGenesisNode.NodeID.Bytes(),
						Weight:  genesisWeight,
						Balance: genesisBalance,
						Signer:  *genesisNodePoP,
					},
				},
				tc.WithDefaultContext(),
			)
			require.NoError(err)
		})

		verifyValidatorSet := func(expectedValidators map[ids.NodeID]*snowvalidators.GetValidatorOutput) {
			height, err := pClient.GetHeight(tc.DefaultContext())
			require.NoError(err)

			subnetValidators, err := pClient.GetValidatorsAt(tc.DefaultContext(), subnetID, height)
			require.NoError(err)
			require.Equal(expectedValidators, subnetValidators)
		}
		tc.By("verifying the Permissioned Subnet was converted to a Permissionless L1", func() {
			expectedConversionID, err := warpmessage.SubnetConversionID(warpmessage.SubnetConversionData{
				SubnetID:       subnetID,
				ManagerChainID: chainID,
				ManagerAddress: address,
				Validators: []warpmessage.SubnetConversionValidatorData{
					{
						NodeID:       subnetGenesisNode.NodeID.Bytes(),
						BLSPublicKey: genesisNodePoP.PublicKey,
						Weight:       genesisWeight,
					},
				},
			})
			require.NoError(err)

			tc.By("verifying the subnet reports as being converted", func() {
				subnet, err := pClient.GetSubnet(tc.DefaultContext(), subnetID)
				require.NoError(err)
				require.Equal(
					platformvmsdk.GetSubnetClientResponse{
						IsPermissioned: false,
						ControlKeys: []ids.ShortID{
							keychain.Keys[0].Address(),
						},
						Threshold:      1,
						ConversionID:   expectedConversionID,
						ManagerChainID: chainID,
						ManagerAddress: address,
					},
					subnet,
				)
			})

			tc.By("verifying the validator set was updated", func() {
				verifyValidatorSet(map[ids.NodeID]*snowvalidators.GetValidatorOutput{
					subnetGenesisNode.NodeID: {
						NodeID:    subnetGenesisNode.NodeID,
						PublicKey: genesisNodePK,
						Weight:    genesisWeight,
					},
				})
			})
		})

		advanceProposerVMPChainHeight := func() {
			// We first must wait at least [RecentlyAcceptedWindowTTL] to ensure
			// the next block will evict the prior block from the windower.
			time.Sleep((5 * platformvmvalidators.RecentlyAcceptedWindowTTL) / 4)

			// Now we must:
			// 1. issue a block which should include the old P-chain height.
			// 2. issue a block which should include the new P-chain height.
			for range 2 {
				_, err = pWallet.IssueBaseTx(nil, tc.WithDefaultContext())
				require.NoError(err)
			}
			// Now that a block has been issued with the new P-chain height, the
			// next block will use that height for warp message verification.
		}
		tc.By("advancing the proposervm P-chain height", advanceProposerVMPChainHeight)

		tc.By("creating the validator to register")
		subnetRegisterNode := e2e.AddEphemeralNode(tc, env.GetNetwork(), tmpnet.FlagsMap{
			config.TrackSubnetsKey: subnetID.String(),
		})

		registerNodePoP, err := subnetRegisterNode.GetProofOfPossession()
		require.NoError(err)

		tc.By("ensuring the subnet nodes are healthy", func() {
			e2e.WaitForHealthy(tc, subnetGenesisNode)
			e2e.WaitForHealthy(tc, subnetRegisterNode)
		})

		tc.By("creating the RegisterSubnetValidatorMessage")
		registerSubnetValidatorMessage, err := warpmessage.NewRegisterSubnetValidator(
			subnetID,
			subnetRegisterNode.NodeID,
			registerNodePoP.PublicKey,
			uint64(time.Now().Add(5*time.Minute).Unix()),
			warpmessage.PChainOwner{},
			warpmessage.PChainOwner{},
			registerWeight,
		)
		require.NoError(err)

		tc.By("registering the validator", func() {
			tc.By("creating the unsigned warp message")
			unsignedRegisterSubnetValidator := must[*warp.UnsignedMessage](tc)(warp.NewUnsignedMessage(
				networkID,
				chainID,
				must[*payload.AddressedCall](tc)(payload.NewAddressedCall(
					address,
					registerSubnetValidatorMessage.Bytes(),
				)).Bytes(),
			))

			tc.By("sending the request to sign the warp message", func() {
				registerSubnetValidatorRequest, err := wrapWarpSignatureRequest(
					unsignedRegisterSubnetValidator,
					nil,
				)
				require.NoError(err)

				require.True(genesisPeer.Send(tc.DefaultContext(), registerSubnetValidatorRequest))
			})

			tc.By("getting the signature response")
			registerSubnetValidatorSignature, ok, err := findMessage(genesisPeerMessages, unwrapWarpSignature)
			require.NoError(err)
			require.True(ok)

			tc.By("creating the signed warp message to register the validator")
			signers := set.NewBits()
			signers.Add(0) // [signers] has weight from the genesis peer

			var sigBytes [bls.SignatureLen]byte
			copy(sigBytes[:], bls.SignatureToBytes(registerSubnetValidatorSignature))
			registerSubnetValidator, err := warp.NewMessage(
				unsignedRegisterSubnetValidator,
				&warp.BitSetSignature{
					Signers:   signers.Bytes(),
					Signature: sigBytes,
				},
			)
			require.NoError(err)

			tc.By("issuing a RegisterSubnetValidatorTx", func() {
				_, err := pWallet.IssueRegisterSubnetValidatorTx(
					registerBalance,
					registerNodePoP.ProofOfPossession,
					registerSubnetValidator.Bytes(),
				)
				require.NoError(err)
			})
		})

		tc.By("verifying the validator was registered", func() {
			tc.By("verifying the validator set was updated", func() {
				verifyValidatorSet(map[ids.NodeID]*snowvalidators.GetValidatorOutput{
					subnetGenesisNode.NodeID: {
						NodeID:    subnetGenesisNode.NodeID,
						PublicKey: genesisNodePK,
						Weight:    genesisWeight,
					},
					ids.EmptyNodeID: { // The validator is not active
						NodeID: ids.EmptyNodeID,
						Weight: registerWeight,
					},
				})
			})
		})
	})
})

func wrapWarpSignatureRequest(
	msg *warp.UnsignedMessage,
	justification []byte,
) (p2pmessage.OutboundMessage, error) {
	p2pMessageFactory, err := p2pmessage.NewCreator(
		logging.NoLog{},
		prometheus.NewRegistry(),
		constants.DefaultNetworkCompressionType,
		10*time.Second,
	)
	if err != nil {
		return nil, err
	}

	request := sdk.SignatureRequest{
		Message:       msg.Bytes(),
		Justification: justification,
	}
	requestBytes, err := proto.Marshal(&request)
	if err != nil {
		return nil, err
	}

	return p2pMessageFactory.AppRequest(
		msg.SourceChainID,
		0,
		time.Hour,
		p2psdk.PrefixMessage(
			p2psdk.ProtocolPrefix(p2psdk.SignatureRequestHandlerID),
			requestBytes,
		),
	)
}

func findMessage[T any](
	q buffer.BlockingDeque[p2pmessage.InboundMessage],
	parser func(p2pmessage.InboundMessage) (T, bool, error),
) (T, bool, error) {
	var messagesToReprocess []p2pmessage.InboundMessage
	defer func() {
		slices.Reverse(messagesToReprocess)
		for _, msg := range messagesToReprocess {
			q.PushLeft(msg)
		}
	}()

	for {
		msg, ok := q.PopLeft()
		if !ok {
			return utils.Zero[T](), false, nil
		}

		parsed, ok, err := parser(msg)
		if err != nil {
			return utils.Zero[T](), false, err
		}
		if ok {
			return parsed, true, nil
		}

		messagesToReprocess = append(messagesToReprocess, msg)
	}
}

// unwrapWarpSignature assumes the only type of AppResponses that will be
// received are ACP-118 compliant responses.
func unwrapWarpSignature(msg p2pmessage.InboundMessage) (*bls.Signature, bool, error) {
	var appResponse *p2ppb.AppResponse
	switch msg := msg.Message().(type) {
	case *p2ppb.AppResponse:
		appResponse = msg
	case *p2ppb.AppError:
		return nil, false, errors.New(msg.ErrorMessage)
	default:
		return nil, false, nil
	}

	var response sdk.SignatureResponse
	if err := proto.Unmarshal(appResponse.AppBytes, &response); err != nil {
		return nil, false, err
	}

	warpSignature, err := bls.SignatureFromBytes(response.Signature)
	return warpSignature, true, err
}

func must[T any](t require.TestingT) func(T, error) T {
	return func(val T, err error) T {
		require.NoError(t, err)
		return val
	}
}
