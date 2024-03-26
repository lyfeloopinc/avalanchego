// Copyright (C) 2019-2024, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package executor

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/avm/fxs"
	"github.com/ava-labs/avalanchego/vms/avm/state"
	"github.com/ava-labs/avalanchego/vms/avm/txs"
	"github.com/ava-labs/avalanchego/vms/avm/txs/fees"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/components/verify"

	commonfees "github.com/ava-labs/avalanchego/vms/components/fees"
)

var (
	_ txs.Visitor = (*SemanticVerifier)(nil)

	errAssetIDMismatch = errors.New("asset IDs in the input don't match the utxo")
	errNotAnAsset      = errors.New("not an asset")
	errIncompatibleFx  = errors.New("incompatible feature extension")
	errUnknownFx       = errors.New("unknown feature extension")
)

type SemanticVerifier struct {
	// inputs
	*Backend
	BlkFeeManager      *commonfees.Manager
	BlockMaxComplexity commonfees.Dimensions
	State              state.ReadOnlyChain
	Tx                 *txs.Tx

	// outputs
	TipPercentage commonfees.TipPercentage
}

func (v *SemanticVerifier) BaseTx(tx *txs.BaseTx) error {
	return v.verifyBaseTx(tx, nil, nil, v.Tx.Creds)
}

func (v *SemanticVerifier) CreateAssetTx(tx *txs.CreateAssetTx) error {
	return v.verifyBaseTx(&tx.BaseTx, nil, nil, v.Tx.Creds)
}

func (v *SemanticVerifier) OperationTx(tx *txs.OperationTx) error {
	if err := v.verifyBaseTx(&tx.BaseTx, nil, nil, v.Tx.Creds); err != nil {
		return err
	}

	if !v.Bootstrapped || v.Tx.ID().String() == "MkvpJS13eCnEYeYi9B5zuWrU9goG9RBj7nr83U7BjrFV22a12" {
		return nil
	}

	offset := len(tx.Ins)
	for i, op := range tx.Ops {
		// Note: Verification of the length of [t.tx.Creds] happens during
		// syntactic verification, which happens before semantic verification.
		cred := v.Tx.Creds[i+offset].Credential
		if err := v.verifyOperation(tx, op, cred); err != nil {
			return err
		}
	}
	return nil
}

func (v *SemanticVerifier) ImportTx(tx *txs.ImportTx) error {
	if err := v.verifyBaseTx(&tx.BaseTx, tx.ImportedIns, nil, v.Tx.Creds); err != nil {
		return err
	}

	if !v.Bootstrapped {
		return nil
	}

	if err := verify.SameSubnet(context.TODO(), v.Ctx, tx.SourceChain); err != nil {
		return err
	}

	utxoIDs := make([][]byte, len(tx.ImportedIns))
	for i, in := range tx.ImportedIns {
		inputID := in.UTXOID.InputID()
		utxoIDs[i] = inputID[:]
	}

	allUTXOBytes, err := v.Ctx.SharedMemory.Get(tx.SourceChain, utxoIDs)
	if err != nil {
		return err
	}

	offset := len(tx.Ins)
	for i, in := range tx.ImportedIns {
		utxo := avax.UTXO{}
		if _, err := v.Codec.Unmarshal(allUTXOBytes[i], &utxo); err != nil {
			return err
		}

		// Note: Verification of the length of [t.tx.Creds] happens during
		// syntactic verification, which happens before semantic verification.
		cred := v.Tx.Creds[i+offset].Credential
		if err := v.verifyTransferOfUTXO(tx, in, cred, &utxo); err != nil {
			return err
		}
	}
	return nil
}

func (v *SemanticVerifier) ExportTx(tx *txs.ExportTx) error {
	if err := v.verifyBaseTx(&tx.BaseTx, nil, tx.ExportedOuts, v.Tx.Creds); err != nil {
		return err
	}

	if v.Bootstrapped {
		if err := verify.SameSubnet(context.TODO(), v.Ctx, tx.DestinationChain); err != nil {
			return err
		}
	}

	for _, out := range tx.ExportedOuts {
		fxIndex, err := v.getFx(out.Out)
		if err != nil {
			return err
		}

		assetID := out.AssetID()
		if err := v.verifyFxUsage(fxIndex, assetID); err != nil {
			return err
		}
	}
	return nil
}

func (v *SemanticVerifier) verifyBaseTx(
	tx *txs.BaseTx,
	importedIns []*avax.TransferableInput,
	exportedOuts []*avax.TransferableOutput,
	creds []*fxs.FxCredential,
) error {
	isEActive := v.Config.IsEActivated(v.State.GetTimestamp())
	feeCalculator := fees.Calculator{
		IsEActive:          isEActive,
		Config:             v.Config,
		FeeManager:         v.BlkFeeManager,
		BlockMaxComplexity: v.BlockMaxComplexity,
		Codec:              v.Codec,
		Credentials:        creds,
	}
	if err := tx.Visit(&feeCalculator); err != nil {
		return err
	}

	feesPaid, err := avax.VerifyTx(
		feeCalculator.Fee,
		v.FeeAssetID,
		[][]*avax.TransferableInput{tx.Ins, importedIns},
		[][]*avax.TransferableOutput{tx.Outs, exportedOuts},
		v.Codec,
	)
	if err != nil {
		return err
	}

	if isEActive {
		if err := feeCalculator.CalculateTipPercentage(feesPaid); err != nil {
			return fmt.Errorf("failed estimating fee tip percentage: %w", err)
		}
		v.TipPercentage = feeCalculator.TipPercentage
	}

	for i, in := range tx.Ins {
		// Note: Verification of the length of [t.tx.Creds] happens during
		// syntactic verification, which happens before semantic verification.
		cred := v.Tx.Creds[i].Credential
		if err := v.verifyTransfer(tx, in, cred); err != nil {
			return err
		}
	}

	for _, out := range tx.Outs {
		fxIndex, err := v.getFx(out.Out)
		if err != nil {
			return err
		}

		assetID := out.AssetID()
		if err := v.verifyFxUsage(fxIndex, assetID); err != nil {
			return err
		}
	}

	return nil
}

func (v *SemanticVerifier) verifyTransfer(
	tx txs.UnsignedTx,
	in *avax.TransferableInput,
	cred verify.Verifiable,
) error {
	utxo, err := v.State.GetUTXO(in.UTXOID.InputID())
	if err != nil {
		return err
	}
	return v.verifyTransferOfUTXO(tx, in, cred, utxo)
}

func (v *SemanticVerifier) verifyTransferOfUTXO(
	tx txs.UnsignedTx,
	in *avax.TransferableInput,
	cred verify.Verifiable,
	utxo *avax.UTXO,
) error {
	utxoAssetID := utxo.AssetID()
	inAssetID := in.AssetID()
	if utxoAssetID != inAssetID {
		return errAssetIDMismatch
	}

	fxIndex, err := v.getFx(cred)
	if err != nil {
		return err
	}

	if err := v.verifyFxUsage(fxIndex, inAssetID); err != nil {
		return err
	}

	fx := v.Fxs[fxIndex].Fx
	return fx.VerifyTransfer(tx, in.In, cred, utxo.Out)
}

func (v *SemanticVerifier) verifyOperation(
	tx *txs.OperationTx,
	op *txs.Operation,
	cred verify.Verifiable,
) error {
	var (
		opAssetID = op.AssetID()
		numUTXOs  = len(op.UTXOIDs)
		utxos     = make([]interface{}, numUTXOs)
	)
	for i, utxoID := range op.UTXOIDs {
		utxo, err := v.State.GetUTXO(utxoID.InputID())
		if err != nil {
			return err
		}

		utxoAssetID := utxo.AssetID()
		if utxoAssetID != opAssetID {
			return errAssetIDMismatch
		}
		utxos[i] = utxo.Out
	}

	fxIndex, err := v.getFx(op.Op)
	if err != nil {
		return err
	}

	if err := v.verifyFxUsage(fxIndex, opAssetID); err != nil {
		return err
	}

	fx := v.Fxs[fxIndex].Fx
	return fx.VerifyOperation(tx, op.Op, cred, utxos)
}

func (v *SemanticVerifier) verifyFxUsage(
	fxID int,
	assetID ids.ID,
) error {
	tx, err := v.State.GetTx(assetID)
	if err != nil {
		return err
	}

	createAssetTx, ok := tx.Unsigned.(*txs.CreateAssetTx)
	if !ok {
		return errNotAnAsset
	}

	for _, state := range createAssetTx.States {
		if state.FxIndex == uint32(fxID) {
			return nil
		}
	}

	return errIncompatibleFx
}

func (v *SemanticVerifier) getFx(val interface{}) (int, error) {
	valType := reflect.TypeOf(val)
	fx, exists := v.TypeToFxIndex[valType]
	if !exists {
		return 0, errUnknownFx
	}
	return fx, nil
}
