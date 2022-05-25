// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package stateful

import (
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/crypto"
	"github.com/ava-labs/avalanchego/utils/hashing"
	"github.com/ava-labs/avalanchego/vms/platformvm/reward"
	"github.com/ava-labs/avalanchego/vms/platformvm/status"
	"github.com/ava-labs/avalanchego/vms/platformvm/transactions/signed"
	"github.com/ava-labs/avalanchego/vms/platformvm/transactions/unsigned"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
)

func TestAddSubnetValidatorTxSyntacticVerify(t *testing.T) {
	h := newTestHelpersCollection()
	defer func() {
		if err := internalStateShutdown(h); err != nil {
			t.Fatal(err)
		}
	}()

	nodeID := ids.NodeID(preFundedKeys[0].PublicKey().Address())

	// Case: tx is nil
	var unsignedTx *unsigned.AddSubnetValidatorTx
	stx := signed.Tx{
		Unsigned: unsignedTx,
	}
	if err := stx.SyntacticVerify(h.ctx); err == nil {
		t.Fatal("should have errored because tx is nil")
	}

	// Case: Wrong network ID
	tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(defaultValidateStartTime.Unix()),
		uint64(defaultValidateEndTime.Unix()),
		nodeID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}
	tx.Unsigned.(*unsigned.AddSubnetValidatorTx).NetworkID++
	// This tx was syntactically verified when it was created...pretend it wasn't so we don't use cache
	tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SyntacticallyVerified = false
	if err := tx.SyntacticVerify(h.ctx); err == nil {
		t.Fatal("should have errored because the wrong network ID was used")
	}

	// Case: Missing Subnet ID
	tx, err = h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(defaultValidateStartTime.Unix()),
		uint64(defaultValidateEndTime.Unix()),
		nodeID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}
	tx.Unsigned.(*unsigned.AddSubnetValidatorTx).Validator.Subnet = ids.ID{}
	// This tx was syntactically verified when it was created...pretend it wasn't so we don't use cache
	tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SyntacticallyVerified = false
	if err := tx.SyntacticVerify(h.ctx); err == nil {
		t.Fatal("should have errored because Subnet ID is empty")
	}

	// Case: No weight
	tx, err = h.txBuilder.NewAddSubnetValidatorTx(
		1,
		uint64(defaultValidateStartTime.Unix()),
		uint64(defaultValidateEndTime.Unix()),
		nodeID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}
	tx.Unsigned.(*unsigned.AddSubnetValidatorTx).Validator.Wght = 0
	// This tx was syntactically verified when it was created...pretend it wasn't so we don't use cache
	tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SyntacticallyVerified = false
	if err := tx.SyntacticVerify(h.ctx); err == nil {
		t.Fatal("should have errored because of no weight")
	}

	// Case: Subnet auth indices not unique
	tx, err = h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(defaultValidateStartTime.Unix()),
		uint64(defaultValidateEndTime.Unix())-1,
		nodeID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}
	tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SubnetAuth.(*secp256k1fx.Input).SigIndices[0] =
		tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SubnetAuth.(*secp256k1fx.Input).SigIndices[1]
	// This tx was syntactically verified when it was created...pretend it wasn't so we don't use cache
	tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SyntacticallyVerified = false
	if err = tx.SyntacticVerify(h.ctx); err == nil {
		t.Fatal("should have errored because sig indices weren't unique")
	}

	// Case: Valid
	if tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(defaultValidateStartTime.Unix()),
		uint64(defaultValidateEndTime.Unix()),
		nodeID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	); err != nil {
		t.Fatal(err)
	} else if err := tx.SyntacticVerify(h.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAddSubnetValidatorTxExecute(t *testing.T) {
	h := newTestHelpersCollection()
	defer func() {
		if err := internalStateShutdown(h); err != nil {
			t.Fatal(err)
		}
	}()

	nodeID := preFundedKeys[0].PublicKey().Address()

	// Case: Proposed validator currently validating primary network
	// but stops validating subnet after stops validating primary network
	// (note that preFundedKeys[0] is a genesis validator)
	if tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(defaultValidateStartTime.Unix()),
		uint64(defaultValidateEndTime.Unix())+1,
		ids.NodeID(nodeID),
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	); err != nil {
		t.Fatal(err)
	} else {
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed because validator stops validating primary network earlier than subnet")
		}
	}

	// Case: Proposed validator currently validating primary network
	// and proposed subnet validation period is subset of
	// primary network validation period
	// (note that preFundedKeys[0] is a genesis validator)
	if tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(defaultValidateStartTime.Unix()+1),
		uint64(defaultValidateEndTime.Unix()),
		ids.NodeID(nodeID),
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	); err != nil {
		t.Fatal(err)
	} else {
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err = vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err != nil {
			t.Fatal(err)
		}
	}

	// Add a validator to pending validator set of primary network
	factory := crypto.FactorySECP256K1R{}
	key, err := factory.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pendingDSValidatorID := ids.NodeID(key.PublicKey().Address())

	// starts validating primary network 10 seconds after genesis
	DSStartTime := defaultGenesisTime.Add(10 * time.Second)
	DSEndTime := DSStartTime.Add(5 * defaultMinStakingDuration)

	addDSTx, err := h.txBuilder.NewAddValidatorTx(
		h.cfg.MinValidatorStake,    // stake amount
		uint64(DSStartTime.Unix()), // start time
		uint64(DSEndTime.Unix()),   // end time
		pendingDSValidatorID,       // node ID
		nodeID,                     // reward address
		reward.PercentDenominator,  // shares
		[]*crypto.PrivateKeySECP256K1R{preFundedKeys[0]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Case: Proposed validator isn't in pending or current validator sets
	if tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(DSStartTime.Unix()), // start validating subnet before primary network
		uint64(DSEndTime.Unix()),
		pendingDSValidatorID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	); err != nil {
		t.Fatal(err)
	} else {
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err = vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed because validator not in the current or pending validator sets of the primary network")
		}
	}

	h.tState.AddCurrentStaker(addDSTx, 0)
	h.tState.AddTx(addDSTx, status.Committed)
	if err := h.tState.Write(); err != nil {
		t.Fatal(err)
	}
	if err := h.tState.Load(); err != nil {
		t.Fatal(err)
	}

	// Node with ID key.PublicKey().Address() now a pending validator for primary network

	// Case: Proposed validator is pending validator of primary network
	// but starts validating subnet before primary network
	if tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(DSStartTime.Unix())-1, // start validating subnet before primary network
		uint64(DSEndTime.Unix()),
		pendingDSValidatorID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	); err != nil {
		t.Fatal(err)
	} else {
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed because validator starts validating primary " +
				"network before starting to validate primary network")
		}
	}

	// Case: Proposed validator is pending validator of primary network
	// but stops validating subnet after primary network
	if tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(DSStartTime.Unix()),
		uint64(DSEndTime.Unix())+1, // stop validating subnet after stopping validating primary network
		pendingDSValidatorID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	); err != nil {
		t.Fatal(err)
	} else {
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err = vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed because validator stops validating primary " +
				"network after stops validating primary network")
		}
	}

	// Case: Proposed validator is pending validator of primary network
	// and period validating subnet is subset of time validating primary network
	if tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(DSStartTime.Unix()), // same start time as for primary network
		uint64(DSEndTime.Unix()),   // same end time as for primary network
		pendingDSValidatorID,
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	); err != nil {
		t.Fatal(err)
	} else {
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err != nil {
			t.Fatalf("should have passed verification")
		}
	}

	// Case: Proposed validator start validating at/before current timestamp
	// First, advance the timestamp
	newTimestamp := defaultGenesisTime.Add(2 * time.Second)
	h.tState.SetTimestamp(newTimestamp)

	if tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,               // weight
		uint64(newTimestamp.Unix()), // start time
		uint64(newTimestamp.Add(defaultMinStakingDuration).Unix()), // end time
		ids.NodeID(nodeID), // node ID
		testSubnet1.ID(),   // subnet ID
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	); err != nil {
		t.Fatal(err)
	} else {
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed verification because starts validating at current timestamp")
		}
	}

	// reset the timestamp
	h.tState.SetTimestamp(defaultGenesisTime)

	// Case: Proposed validator already validating the subnet
	// First, add validator as validator of subnet
	subnetTx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,                           // weight
		uint64(defaultValidateStartTime.Unix()), // start time
		uint64(defaultValidateEndTime.Unix()),   // end time
		ids.NodeID(nodeID),                      // node ID
		testSubnet1.ID(),                        // subnet ID
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}

	h.tState.AddCurrentStaker(subnetTx, 0)
	h.tState.AddTx(subnetTx, status.Committed)
	if err := h.tState.Write(); err != nil {
		t.Fatal(err)
	}
	if err := h.tState.Load(); err != nil {
		t.Fatal(err)
	}

	// Node with ID nodeIDKey.PublicKey().Address() now validating subnet with ID testSubnet1.ID
	duplicateSubnetTx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,                           // weight
		uint64(defaultValidateStartTime.Unix()), // start time
		uint64(defaultValidateEndTime.Unix()),   // end time
		ids.NodeID(nodeID),                      // node ID
		testSubnet1.ID(),                        // subnet ID
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}

	{ // test execute
		verifiableTx, err := MakeStatefulTx(duplicateSubnetTx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, duplicateSubnetTx.Creds); err == nil {
			t.Fatal("should have failed verification because validator already validating the specified subnet")
		}
	}

	h.tState.DeleteCurrentStaker(subnetTx)
	if err := h.tState.Write(); err != nil {
		t.Fatal(err)
	}
	if err := h.tState.Load(); err != nil {
		t.Fatal(err)
	}

	// Case: Too many signatures
	tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,                     // weight
		uint64(defaultGenesisTime.Unix()), // start time
		uint64(defaultGenesisTime.Add(defaultMinStakingDuration).Unix())+1, // end time
		ids.NodeID(nodeID), // node ID
		testSubnet1.ID(),   // subnet ID
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1], testSubnet1ControlKeys[2]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}
	{ // test execute
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed verification because validator already validating the specified subnet")
		}
	}

	// Case: Too few signatures
	tx, err = h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,                     // weight
		uint64(defaultGenesisTime.Unix()), // start time
		uint64(defaultGenesisTime.Add(defaultMinStakingDuration).Unix()), // end time
		ids.NodeID(nodeID), // node ID
		testSubnet1.ID(),   // subnet ID
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[2]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}
	{ // test execute
		// Remove a signature
		tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SubnetAuth.(*secp256k1fx.Input).SigIndices =
			tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SubnetAuth.(*secp256k1fx.Input).SigIndices[1:]

		// This tx was syntactically verified when it was created...pretend it wasn't so we don't use cache
		tx.Unsigned.(*unsigned.AddSubnetValidatorTx).SyntacticallyVerified = false

		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed verification because not enough control sigs")
		}
	}

	// Case: Control Signature from invalid key (preFundedKeys[3] is not a control key)
	tx, err = h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,                     // weight
		uint64(defaultGenesisTime.Unix()), // start time
		uint64(defaultGenesisTime.Add(defaultMinStakingDuration).Unix()), // end time
		ids.NodeID(nodeID), // node ID
		testSubnet1.ID(),   // subnet ID
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], preFundedKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}
	{ // test execute
		// Replace a valid signature with one from preFundedKeys[3]
		sig, err := preFundedKeys[3].SignHash(hashing.ComputeHash256(tx.Unsigned.UnsignedBytes()))
		if err != nil {
			t.Fatal(err)
		}
		copy(tx.Creds[0].(*secp256k1fx.Credential).Sigs[0][:], sig)

		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed verification because a control sig is invalid")
		}
	}

	// Case: Proposed validator in pending validator set for subnet
	// First, add validator to pending validator set of subnet
	tx, err = h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,                       // weight
		uint64(defaultGenesisTime.Unix())+1, // start time
		uint64(defaultGenesisTime.Add(defaultMinStakingDuration).Unix())+1, // end time
		ids.NodeID(nodeID), // node ID
		testSubnet1.ID(),   // subnet ID
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}

	h.tState.AddCurrentStaker(tx, 0)
	h.tState.AddTx(tx, status.Committed)
	if err := h.tState.Write(); err != nil {
		t.Fatal(err)
	}
	if err := h.tState.Load(); err != nil {
		t.Fatal(err)
	}

	{ // test execute
		verifiableTx, err := MakeStatefulTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		vProposalTx, ok := verifiableTx.(ProposalTx)
		if !ok {
			t.Fatal("unexpected tx type")
		}
		if _, _, err := vProposalTx.Execute(h.txVerifier, h.tState, tx.Creds); err == nil {
			t.Fatal("should have failed verification because validator already in pending validator set of the specified subnet")
		}
	}
}

// Test that marshalling/unmarshalling works
func TestAddSubnetValidatorMarshal(t *testing.T) {
	h := newTestHelpersCollection()
	defer func() {
		if err := internalStateShutdown(h); err != nil {
			t.Fatal(err)
		}
	}()

	var unmarshaledTx signed.Tx

	// valid tx
	tx, err := h.txBuilder.NewAddSubnetValidatorTx(
		defaultWeight,
		uint64(defaultValidateStartTime.Unix()),
		uint64(defaultValidateEndTime.Unix()),
		ids.NodeID(preFundedKeys[0].PublicKey().Address()),
		testSubnet1.ID(),
		[]*crypto.PrivateKeySECP256K1R{testSubnet1ControlKeys[0], testSubnet1ControlKeys[1]},
		ids.ShortEmpty,
	)
	if err != nil {
		t.Fatal(err)
	}
	txBytes, err := unsigned.Codec.Marshal(unsigned.Version, tx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := unsigned.Codec.Unmarshal(txBytes, &unmarshaledTx); err != nil {
		t.Fatal(err)
	}

	if err := unmarshaledTx.Sign(unsigned.Codec, nil); err != nil {
		t.Fatal(err)
	}

	if err := unmarshaledTx.SyntacticVerify(h.ctx); err != nil {
		t.Fatal(err)
	}
}
