// Copyright (C) 2019-2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package fee

import (
	"errors"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/components/fee"
	"github.com/ava-labs/avalanchego/vms/components/verify"
	"github.com/ava-labs/avalanchego/vms/platformvm/fx"
	"github.com/ava-labs/avalanchego/vms/platformvm/signer"
	"github.com/ava-labs/avalanchego/vms/platformvm/stakeable"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
)

const (
	intrinsicValidatorBandwidth = ids.NodeIDLen + // nodeID
		wrappers.LongLen + // start
		wrappers.LongLen + // end
		wrappers.LongLen // weight

	intrinsicSubnetValidatorBandwidth = intrinsicValidatorBandwidth + // validator
		ids.IDLen // subnetID

	intrinsicOutputBandwidth = ids.IDLen + // assetID
		wrappers.IntLen // output typeID

	intrinsicStakeableLockedOutputBandwidth = wrappers.LongLen + // locktime
		wrappers.IntLen // output typeID

	intrinsicSECP256k1FxOutputOwnersBandwidth = wrappers.LongLen + // locktime
		wrappers.IntLen + // threshold
		wrappers.IntLen // num addresses

	intrinsicSECP256k1FxOutputBandwidth = wrappers.LongLen + // amount
		intrinsicSECP256k1FxOutputOwnersBandwidth

	intrinsicInputBandwidth = ids.IDLen + // txID
		wrappers.IntLen + // output index
		ids.IDLen + // assetID
		wrappers.IntLen + // input typeID
		wrappers.IntLen // credential typeID

	intrinsicStakeableLockedInputBandwidth = wrappers.LongLen + // locktime
		wrappers.IntLen // input typeID

	intrinsicSECP256k1FxInputBandwidth = wrappers.IntLen + // num indices
		wrappers.IntLen // num signatures

	intrinsicSECP256k1FxTransferableInputBandwidth = wrappers.LongLen + // amount
		intrinsicSECP256k1FxInputBandwidth

	intrinsicSECP256k1FxSignatureBandwidth = wrappers.IntLen + // signature index
		secp256k1.SignatureLen // signature length

	intrinsicPoPBandwidth = bls.PublicKeyLen + // public key
		bls.SignatureLen // signature
)

var (
	_ txs.Visitor = (*complexityCalculator)(nil)

	IntrinsicAddPermissionlessValidatorTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			intrinsicValidatorBandwidth + // validator
			ids.IDLen + // subnetID
			wrappers.IntLen + // signer typeID
			wrappers.IntLen + // num stake outs
			wrappers.IntLen + // validator rewards typeID
			wrappers.IntLen + // delegator rewards typeID
			wrappers.IntLen, // delegation shares
		fee.DBRead:  1,
		fee.DBWrite: 1,
		fee.Compute: 0,
	}
	IntrinsicAddPermissionlessDelegatorTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			intrinsicValidatorBandwidth + // validator
			ids.IDLen + // subnetID
			wrappers.IntLen + // num stake outs
			wrappers.IntLen, // delegator rewards typeID
		fee.DBRead:  1,
		fee.DBWrite: 0,
		fee.Compute: 0,
	}
	IntrinsicAddSubnetValidatorTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			intrinsicSubnetValidatorBandwidth + // subnetValidator
			wrappers.IntLen + // subnetAuth typeID
			wrappers.IntLen, // subnetAuthCredential typeID
		fee.DBRead:  2,
		fee.DBWrite: 1,
		fee.Compute: 0,
	}
	IntrinsicBaseTxComplexities = fee.Dimensions{
		fee.Bandwidth: wrappers.ShortLen + // codecID
			wrappers.IntLen + // typeID
			wrappers.IntLen + // networkID
			ids.IDLen + // blockchainID
			wrappers.IntLen + // number of outputs
			wrappers.IntLen + // number of inputs
			wrappers.IntLen + // length of memo
			wrappers.IntLen, // number of credentials
		fee.DBRead:  0,
		fee.DBWrite: 0,
		fee.Compute: 0,
	}
	IntrinsicCreateChainTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			ids.IDLen + // subnetID
			wrappers.ShortLen + // chainName length
			ids.IDLen + // vmID
			wrappers.IntLen + // num fxIDs
			wrappers.IntLen + // genesis length
			wrappers.IntLen + // subnetAuth typeID
			wrappers.IntLen, // subnetAuthCredential typeID
		fee.DBRead:  1,
		fee.DBWrite: 1,
		fee.Compute: 0,
	}
	IntrinsicCreateSubnetTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			wrappers.IntLen, // owner typeID
		fee.DBRead:  0,
		fee.DBWrite: 1,
		fee.Compute: 0,
	}
	IntrinsicExportTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			ids.IDLen + // destination chainID
			wrappers.IntLen, // num exported outputs
		fee.DBRead:  0,
		fee.DBWrite: 0,
		fee.Compute: 0,
	}
	IntrinsicImportTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			ids.IDLen + // source chainID
			wrappers.IntLen, // num importing inputs
		fee.DBRead:  0,
		fee.DBWrite: 0,
		fee.Compute: 0,
	}
	IntrinsicRemoveSubnetValidatorTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			ids.NodeIDLen + // nodeID
			ids.IDLen + // subnetID
			wrappers.IntLen + // subnetAuth typeID
			wrappers.IntLen, // subnetAuthCredential typeID
		fee.DBRead:  2,
		fee.DBWrite: 1,
		fee.Compute: 0,
	}
	IntrinsicTransferSubnetOwnershipTxComplexities = fee.Dimensions{
		fee.Bandwidth: IntrinsicBaseTxComplexities[fee.Bandwidth] +
			ids.IDLen + // subnetID
			wrappers.IntLen + // subnetAuth typeID
			wrappers.IntLen + // owner typeID
			wrappers.IntLen, // subnetAuthCredential typeID
		fee.DBRead:  1,
		fee.DBWrite: 1,
		fee.Compute: 0,
	}

	errUnsupportedTx     = errors.New("unsupported tx type")
	errUnsupportedOutput = errors.New("unsupported output type")
	errUnsupportedInput  = errors.New("unsupported input type")
	errUnsupportedOwner  = errors.New("unsupported owner type")
	errUnsupportedAuth   = errors.New("unsupported auth type")
	errUnsupportedSigner = errors.New("unsupported signer type")
)

func TxComplexity(tx txs.UnsignedTx) (fee.Dimensions, error) {
	c := complexityCalculator{}
	err := tx.Visit(&c)
	return c.output, err
}

func OutputComplexity(out *avax.TransferableOutput) (fee.Dimensions, error) {
	complexity := fee.Dimensions{
		fee.Bandwidth: intrinsicOutputBandwidth + intrinsicSECP256k1FxOutputBandwidth,
		fee.DBRead:    0,
		fee.DBWrite:   1,
		fee.Compute:   0,
	}

	outIntf := out.Out
	if stakeableOut, ok := outIntf.(*stakeable.LockOut); ok {
		complexity[fee.Bandwidth] += intrinsicStakeableLockedOutputBandwidth
		outIntf = stakeableOut.TransferableOut
	}

	secp256k1Out, ok := outIntf.(*secp256k1fx.TransferOutput)
	if !ok {
		return fee.Dimensions{}, errUnsupportedOutput
	}

	numAddresses := uint64(len(secp256k1Out.Addrs))
	addressBandwidth, err := math.Mul(numAddresses, ids.ShortIDLen)
	if err != nil {
		return fee.Dimensions{}, err
	}
	complexity[fee.Bandwidth], err = math.Add(complexity[fee.Bandwidth], addressBandwidth)
	return complexity, err
}

// InputComplexity returns the complexity an input adds to a transaction.
// It includes the complexity that the corresponding credential will add.
func InputComplexity(in *avax.TransferableInput) (fee.Dimensions, error) {
	complexity := fee.Dimensions{
		fee.Bandwidth: intrinsicInputBandwidth + intrinsicSECP256k1FxTransferableInputBandwidth,
		fee.DBRead:    1,
		fee.DBWrite:   1,
		fee.Compute:   0, // TODO
	}

	inIntf := in.In
	if stakeableIn, ok := inIntf.(*stakeable.LockIn); ok {
		complexity[fee.Bandwidth] += intrinsicStakeableLockedInputBandwidth
		inIntf = stakeableIn.TransferableIn
	}

	secp256k1In, ok := inIntf.(*secp256k1fx.TransferInput)
	if !ok {
		return fee.Dimensions{}, errUnsupportedInput
	}

	numSignatures := uint64(len(secp256k1In.SigIndices))
	signatureBandwidth, err := math.Mul(numSignatures, intrinsicSECP256k1FxSignatureBandwidth)
	if err != nil {
		return fee.Dimensions{}, err
	}
	complexity[fee.Bandwidth], err = math.Add(complexity[fee.Bandwidth], signatureBandwidth)
	return complexity, err
}

// OwnerComplexity returns the complexity an owner adds to a transaction.
// It does not include the typeID of the owner.
func OwnerComplexity(ownerIntf fx.Owner) (fee.Dimensions, error) {
	owner, ok := ownerIntf.(*secp256k1fx.OutputOwners)
	if !ok {
		return fee.Dimensions{}, errUnsupportedOwner
	}

	numAddresses := uint64(len(owner.Addrs))
	addressBandwidth, err := math.Mul(numAddresses, ids.ShortIDLen)
	if err != nil {
		return fee.Dimensions{}, err
	}

	bandwidth, err := math.Add(addressBandwidth, intrinsicSECP256k1FxOutputOwnersBandwidth)
	if err != nil {
		return fee.Dimensions{}, err
	}

	return fee.Dimensions{
		fee.Bandwidth: bandwidth,
		fee.DBRead:    0,
		fee.DBWrite:   0,
		fee.Compute:   0,
	}, nil
}

// AuthComplexity returns the complexity an authorization adds to a transaction.
// It does not include the typeID of the authorization.
// It does includes the complexity that the corresponding credential will add.
// It does not include the typeID of the credential.
func AuthComplexity(authIntf verify.Verifiable) (fee.Dimensions, error) {
	auth, ok := authIntf.(*secp256k1fx.Input)
	if !ok {
		return fee.Dimensions{}, errUnsupportedAuth
	}

	numSignatures := uint64(len(auth.SigIndices))
	signatureBandwidth, err := math.Mul(numSignatures, intrinsicSECP256k1FxSignatureBandwidth)
	if err != nil {
		return fee.Dimensions{}, err
	}

	bandwidth, err := math.Add(signatureBandwidth, intrinsicSECP256k1FxInputBandwidth)
	if err != nil {
		return fee.Dimensions{}, err
	}

	return fee.Dimensions{
		fee.Bandwidth: bandwidth,
		fee.DBRead:    0,
		fee.DBWrite:   0,
		fee.Compute:   0,
	}, nil
}

// SignerComplexity returns the complexity a signer adds to a transaction.
// It does not include the typeID of the signer.
func SignerComplexity(s signer.Signer) (fee.Dimensions, error) {
	switch s.(type) {
	case *signer.Empty:
		return fee.Dimensions{}, nil
	case *signer.ProofOfPossession:
		return fee.Dimensions{
			fee.Bandwidth: intrinsicPoPBandwidth,
			fee.DBRead:    0,
			fee.DBWrite:   0,
			fee.Compute:   0,
		}, nil
	default:
		return fee.Dimensions{}, errUnsupportedSigner
	}
}

type complexityCalculator struct {
	output fee.Dimensions
}

func (*complexityCalculator) AddDelegatorTx(*txs.AddDelegatorTx) error {
	return errUnsupportedTx
}

func (*complexityCalculator) AddValidatorTx(*txs.AddValidatorTx) error {
	return errUnsupportedTx
}

func (*complexityCalculator) AdvanceTimeTx(*txs.AdvanceTimeTx) error {
	return errUnsupportedTx
}

func (*complexityCalculator) RewardValidatorTx(*txs.RewardValidatorTx) error {
	return errUnsupportedTx
}

func (*complexityCalculator) TransformSubnetTx(*txs.TransformSubnetTx) error {
	return errUnsupportedTx
}

func (c *complexityCalculator) AddPermissionlessValidatorTx(tx *txs.AddPermissionlessValidatorTx) error {
	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	signerComplexity, err := SignerComplexity(tx.Signer)
	if err != nil {
		return err
	}
	outputsComplexity, err := outputsComplexity(tx.StakeOuts)
	if err != nil {
		return err
	}
	validatorOwnerComplexity, err := OwnerComplexity(tx.ValidatorRewardsOwner)
	if err != nil {
		return err
	}
	delegatorOwnerComplexity, err := OwnerComplexity(tx.DelegatorRewardsOwner)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicAddPermissionlessValidatorTxComplexities.Add(
		baseTxComplexity,
		signerComplexity,
		outputsComplexity,
		validatorOwnerComplexity,
		delegatorOwnerComplexity,
	)
	return err
}

func (c *complexityCalculator) AddPermissionlessDelegatorTx(tx *txs.AddPermissionlessDelegatorTx) error {
	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	ownerComplexity, err := OwnerComplexity(tx.DelegationRewardsOwner)
	if err != nil {
		return err
	}
	outputsComplexity, err := outputsComplexity(tx.StakeOuts)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicAddPermissionlessDelegatorTxComplexities.Add(
		baseTxComplexity,
		ownerComplexity,
		outputsComplexity,
	)
	return err
}

func (c *complexityCalculator) AddSubnetValidatorTx(tx *txs.AddSubnetValidatorTx) error {
	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	authComplexity, err := AuthComplexity(tx.SubnetAuth)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicAddSubnetValidatorTxComplexities.Add(
		baseTxComplexity,
		authComplexity,
	)
	return err
}

func (c *complexityCalculator) BaseTx(tx *txs.BaseTx) error {
	baseTxComplexity, err := baseTx(tx)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicBaseTxComplexities.Add(baseTxComplexity)
	return err
}

func (c *complexityCalculator) CreateChainTx(tx *txs.CreateChainTx) error {
	bandwidth, err := math.Mul(uint64(len(tx.FxIDs)), ids.IDLen)
	if err != nil {
		return err
	}
	bandwidth, err = math.Add(bandwidth, uint64(len(tx.ChainName)))
	if err != nil {
		return err
	}
	bandwidth, err = math.Add(bandwidth, uint64(len(tx.GenesisData)))
	if err != nil {
		return err
	}
	dynamicComplexity := fee.Dimensions{
		fee.Bandwidth: bandwidth,
		fee.DBRead:    0,
		fee.DBWrite:   0,
		fee.Compute:   0,
	}

	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	authComplexity, err := AuthComplexity(tx.SubnetAuth)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicCreateChainTxComplexities.Add(
		dynamicComplexity,
		baseTxComplexity,
		authComplexity,
	)
	return err
}

func (c *complexityCalculator) CreateSubnetTx(tx *txs.CreateSubnetTx) error {
	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	ownerComplexity, err := OwnerComplexity(tx.Owner)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicCreateSubnetTxComplexities.Add(
		baseTxComplexity,
		ownerComplexity,
	)
	return err
}

func (c *complexityCalculator) ExportTx(tx *txs.ExportTx) error {
	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	outputsComplexity, err := outputsComplexity(tx.ExportedOutputs)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicExportTxComplexities.Add(
		baseTxComplexity,
		outputsComplexity,
	)
	return err
}

func (c *complexityCalculator) ImportTx(tx *txs.ImportTx) error {
	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	inputsComplexity, err := inputsComplexity(tx.ImportedInputs)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicImportTxComplexities.Add(
		baseTxComplexity,
		inputsComplexity,
	)
	return err
}

func (c *complexityCalculator) RemoveSubnetValidatorTx(tx *txs.RemoveSubnetValidatorTx) error {
	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	authComplexity, err := AuthComplexity(tx.SubnetAuth)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicRemoveSubnetValidatorTxComplexities.Add(
		baseTxComplexity,
		authComplexity,
	)
	return err
}

func (c *complexityCalculator) TransferSubnetOwnershipTx(tx *txs.TransferSubnetOwnershipTx) error {
	baseTxComplexity, err := baseTx(&tx.BaseTx)
	if err != nil {
		return err
	}
	authComplexity, err := AuthComplexity(tx.SubnetAuth)
	if err != nil {
		return err
	}
	ownerComplexity, err := OwnerComplexity(tx.Owner)
	if err != nil {
		return err
	}
	c.output, err = IntrinsicTransferSubnetOwnershipTxComplexities.Add(
		baseTxComplexity,
		authComplexity,
		ownerComplexity,
	)
	return err
}

func baseTx(tx *txs.BaseTx) (fee.Dimensions, error) {
	outputsComplexity, err := outputsComplexity(tx.Outs)
	if err != nil {
		return fee.Dimensions{}, err
	}
	inputsComplexity, err := inputsComplexity(tx.Ins)
	if err != nil {
		return fee.Dimensions{}, err
	}
	complexity, err := outputsComplexity.Add(inputsComplexity)
	if err != nil {
		return fee.Dimensions{}, err
	}
	complexity[fee.Bandwidth], err = math.Add(
		complexity[fee.Bandwidth],
		uint64(len(tx.Memo)),
	)
	return complexity, err
}

func outputsComplexity(outs []*avax.TransferableOutput) (fee.Dimensions, error) {
	var complexity fee.Dimensions
	for _, out := range outs {
		outputComplexity, err := OutputComplexity(out)
		if err != nil {
			return fee.Dimensions{}, err
		}

		complexity, err = complexity.Add(outputComplexity)
		if err != nil {
			return fee.Dimensions{}, err
		}
	}
	return complexity, nil
}

func inputsComplexity(ins []*avax.TransferableInput) (fee.Dimensions, error) {
	var complexity fee.Dimensions
	for _, in := range ins {
		inputComplexity, err := InputComplexity(in)
		if err != nil {
			return fee.Dimensions{}, err
		}

		complexity, err = complexity.Add(inputComplexity)
		if err != nil {
			return fee.Dimensions{}, err
		}
	}
	return complexity, nil
}
