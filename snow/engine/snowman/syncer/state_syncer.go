// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package syncer

import (
	"fmt"
	"time"

	stdmath "math"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/version"
)

const (
	// maxOutstandingStateSyncRequests is the maximum number of
	// messages sent but not responded to/failed for each relevant message type
	maxOutstandingStateSyncRequests = 50
)

var _ common.StateSyncer = &stateSyncer{}

// summary content as received from network, along with accumulated weight.
type weightedSummary struct {
	summary block.Summary
	weight  uint64
}

type stateSyncer struct {
	Config

	// list of NoOpsHandler for messages dropped by state syncer
	common.AcceptedFrontierHandler
	common.AcceptedHandler
	common.AncestorsHandler
	common.PutHandler
	common.QueryHandler
	common.ChitsHandler
	common.AppHandler

	started bool

	// Tracks the last requestID that was used in a request
	requestID uint32

	stateSyncVM        block.StateSyncableVM
	onDoneStateSyncing func(lastReqID uint32) error

	// we track the (possibly nil) local summary to help engine
	// choosing among multiple validated summaries
	locallyAvailableSummary block.Summary

	// Holds the beacons that were sampled for the accepted frontier
	frontierSeeders validators.Set
	// IDs of validators we should request state summary frontier from
	targetSeeders ids.NodeIDSet
	// IDs of validators we requested a state summary frontier from
	// but haven't received a reply yet. ID is cleared if/when reply arrives.
	contactedSeeders ids.NodeIDSet
	// IDs of validators that failed to respond with their state summary frontier
	failedSeeders ids.NodeIDSet

	// IDs of validators we should request filtering the accepted state summaries from
	targetVoters ids.NodeIDSet
	// IDs of validators we requested filtering the accepted state summaries from
	// but haven't received a reply yet. ID is cleared if/when reply arrives.
	contactedVoters ids.NodeIDSet
	// IDs of validators that failed to respond with their filtered accepted state summaries
	failedVoters ids.NodeIDSet

	// summaryID --> (summary, weight)
	weightedSummaries map[ids.ID]weightedSummary

	// number of times the state sync has been attempted
	attempts int
}

func New(
	cfg Config,
	onDoneStateSyncing func(lastReqID uint32) error,
) common.StateSyncer {
	ssVM, _ := cfg.VM.(block.StateSyncableVM)
	return &stateSyncer{
		Config:                  cfg,
		AcceptedFrontierHandler: common.NewNoOpAcceptedFrontierHandler(cfg.Ctx.Log),
		AcceptedHandler:         common.NewNoOpAcceptedHandler(cfg.Ctx.Log),
		AncestorsHandler:        common.NewNoOpAncestorsHandler(cfg.Ctx.Log),
		PutHandler:              common.NewNoOpPutHandler(cfg.Ctx.Log),
		QueryHandler:            common.NewNoOpQueryHandler(cfg.Ctx.Log),
		ChitsHandler:            common.NewNoOpChitsHandler(cfg.Ctx.Log),
		AppHandler:              common.NewNoOpAppHandler(cfg.Ctx.Log),
		stateSyncVM:             ssVM,
		onDoneStateSyncing:      onDoneStateSyncing,
	}
}

func (ss *stateSyncer) StateSummaryFrontier(validatorID ids.NodeID, requestID uint32, summaryBytes []byte) error {
	// ignores any late responses
	if requestID != ss.requestID {
		ss.Ctx.Log.Debug("Received an Out-of-Sync StateSummaryFrontier - validator: %v - expectedRequestID: %v, requestID: %v",
			validatorID, ss.requestID, requestID)
		return nil
	}

	if !ss.contactedSeeders.Contains(validatorID) {
		ss.Ctx.Log.Debug("Received a StateSummaryFrontier message from %s unexpectedly", validatorID)
		return nil
	}

	// Mark that we received a response from [validatorID]
	ss.contactedSeeders.Remove(validatorID)

	// retrieve summary ID and register frontier;
	// make sure next beacons are reached out
	// even in case invalid summaries are received
	if summary, err := ss.stateSyncVM.ParseStateSummary(summaryBytes); err == nil {
		if _, exists := ss.weightedSummaries[summary.ID()]; !exists {
			ss.weightedSummaries[summary.ID()] = weightedSummary{
				summary: summary,
			}
		}
	} else {
		ss.Ctx.Log.Debug("Could not parse summary from bytes%s: %v", summaryBytes, err)
	}

	ss.sendGetStateSummaryFrontiers()

	// still waiting on requests
	if ss.contactedSeeders.Len() != 0 {
		return nil
	}

	// We've received the accepted frontier from every state syncer
	// Ask each state syncer to filter the list of containers that we were
	// told are on the accepted frontier such that the list only contains containers
	// they think are accepted

	// Create a newAlpha taking using the sampled beacon
	// Keep the proportion of b.Alpha in the newAlpha
	// newAlpha := totalSampledWeight * b.Alpha / totalWeight
	newAlpha := float64(ss.frontierSeeders.Weight()*ss.Alpha) / float64(ss.StateSyncBeacons.Weight())

	failedBeaconWeight, err := ss.StateSyncBeacons.SubsetWeight(ss.failedSeeders)
	if err != nil {
		return err
	}

	// fail the fast sync if the weight is not enough to fast sync
	if float64(ss.frontierSeeders.Weight())-newAlpha < float64(failedBeaconWeight) {
		if ss.Config.RetryBootstrap {
			ss.Ctx.Log.Debug("Not enough frontiers received, restarting state sync... - Beacons: %d - Failed Bootstrappers: %d "+
				"- state sync attempt: %d", ss.StateSyncBeacons.Len(), ss.failedSeeders.Len(), ss.attempts)
			return ss.restart()
		}

		ss.Ctx.Log.Debug("Didn't receive enough frontiers - failed validators: %d, "+
			"state sync attempt: %d", ss.failedSeeders.Len(), ss.attempts)
	}

	ss.requestID++
	return ss.sendGetAccepted()
}

func (ss *stateSyncer) GetStateSummaryFrontierFailed(validatorID ids.NodeID, requestID uint32) error {
	// ignores any late responses
	if requestID != ss.requestID {
		ss.Ctx.Log.Debug("Received an Out-of-Sync GetStateSummaryFrontierFailed - validator: %v - expectedRequestID: %v, requestID: %v",
			validatorID, ss.requestID, requestID)
		return nil
	}

	// If we can't get a response from [validatorID], act as though they said their state summary frontier is empty
	// and we add the validator to the failed list
	ss.failedSeeders.Add(validatorID)
	return ss.StateSummaryFrontier(validatorID, requestID, []byte{})
}

func (ss *stateSyncer) AcceptedStateSummary(validatorID ids.NodeID, requestID uint32, summaryIDs []ids.ID) error {
	// ignores any late responses
	if requestID != ss.requestID {
		ss.Ctx.Log.Debug("Received an Out-of-Sync Accepted - validator: %v - expectedRequestID: %v, requestID: %v",
			validatorID, ss.requestID, requestID)
		return nil
	}

	if !ss.contactedVoters.Contains(validatorID) {
		ss.Ctx.Log.Debug("Received an AcceptedStateSummary message from %s unexpectedly", validatorID)
		return nil
	}
	// Mark that we received a response from [validatorID]
	ss.contactedVoters.Remove(validatorID)

	weight := uint64(0)
	if w, ok := ss.StateSyncBeacons.GetWeight(validatorID); ok {
		weight = w
	}

	for _, summaryID := range summaryIDs {
		ws, ok := ss.weightedSummaries[summaryID]
		if !ok {
			ss.Ctx.Log.Debug("Received a vote from %s for unknown summary %s. Skipped.", validatorID, summaryID)
			continue
		}
		previousWeight := ws.weight
		newWeight, err := math.Add64(weight, previousWeight)
		if err != nil {
			ss.Ctx.Log.Error("Error calculating the Accepted votes - weight: %v, previousWeight: %v", weight, previousWeight)
			newWeight = stdmath.MaxUint64
		}
		ws.weight = newWeight
		ss.weightedSummaries[summaryID] = ws
	}

	if err := ss.sendGetAccepted(); err != nil {
		return err
	}

	// wait on pending responses
	if ss.contactedVoters.Len() != 0 {
		return nil
	}

	// We've received the filtered accepted frontier from every state sync validator
	// Drop all summaries without a sufficient weight behind them
	for key, ws := range ss.weightedSummaries {
		if ws.weight < ss.Alpha {
			delete(ss.weightedSummaries, key)
		}
	}

	// if we don't have enough weight for the state summary to be accepted then retry or fail the state sync
	size := len(ss.weightedSummaries)
	if size == 0 {
		// retry the state sync if the weight is not enough to state sync
		failedBeaconWeight, err := ss.StateSyncBeacons.SubsetWeight(ss.failedVoters)
		if err != nil {
			return err
		}

		// if we had too many timeouts when asking for validator votes, we should restart
		// state sync hoping for the network problems to go away; otherwise, we received
		// enough (>= ss.Alpha) responses, but no state summary was supported by a majority
		// of validators (i.e. votes are split between minorities supporting different state
		// summaries), so there is no point in retrying state sync; we should move ahead to bootstrapping
		votingStakes := ss.StateSyncBeacons.Weight() - failedBeaconWeight
		if ss.Config.RetryBootstrap && votingStakes < ss.Alpha {
			ss.Ctx.Log.Debug("Not enough votes received, restarting state sync... - Beacons: %d - Failed syncer: %d "+
				"- state sync attempt: %d", ss.StateSyncBeacons.Len(), ss.failedVoters.Len(), ss.attempts)
			return ss.restart()
		}

		// if we do not restart state sync, move on to bootstrapping.
		return ss.onDoneStateSyncing(ss.requestID)
	}

	preferredStateSummary := ss.selectSyncableStateSummary()
	ss.Ctx.Log.Info("Selected summary %s out of %d to start state sync",
		preferredStateSummary.ID(), size,
	)

	accepted, err := preferredStateSummary.Accept()
	if err != nil {
		return err
	}
	if accepted {
		// summary was accepted and VM is state syncing.
		// Engine will wait for notification of state sync done.
		return nil
	}

	// VM did not accept the summary, move on to bootstrapping.
	return ss.onDoneStateSyncing(ss.requestID)
}

// selectSyncableStateSummary chooses a state summary from all
// the network validated summaries.
func (ss *stateSyncer) selectSyncableStateSummary() block.Summary {
	maxSummaryHeight := uint64(0)
	preferredStateSummaryID := ids.Empty

	// by default pick highest summary, unless locallyAvailableSummary is still valid.
	// In such case we pick locallyAvailableSummary to allow VM resuming state syncing.
	for id, ws := range ss.weightedSummaries {
		if ss.locallyAvailableSummary != nil && id == ss.locallyAvailableSummary.ID() {
			preferredStateSummaryID = id
			break
		}

		if maxSummaryHeight < ws.summary.Height() {
			maxSummaryHeight = ws.summary.Height()
			preferredStateSummaryID = id
		}
	}
	return ss.weightedSummaries[preferredStateSummaryID].summary
}

func (ss *stateSyncer) GetAcceptedStateSummaryFailed(validatorID ids.NodeID, requestID uint32) error {
	// ignores any late responses
	if requestID != ss.requestID {
		ss.Ctx.Log.Debug("Received an Out-of-Sync GetAcceptedStateSummaryFailed - validator: %v - expectedRequestID: %v, requestID: %v",
			validatorID, ss.requestID, requestID)
		return nil
	}

	// If we can't get a response from [validatorID], act as though they said
	// that they think none of the containers we sent them in GetAccepted are accepted
	ss.failedVoters.Add(validatorID)

	return ss.AcceptedStateSummary(validatorID, requestID, []ids.ID{})
}

func (ss *stateSyncer) Start(startReqID uint32) error {
	ss.requestID = startReqID
	ss.Ctx.SetState(snow.StateSyncing)

	if !ss.WeightTracker.EnoughConnectedWeight() {
		return nil
	}

	ss.started = true
	return ss.startup()
}

func (ss *stateSyncer) startup() error {
	ss.Config.Ctx.Log.Info("starting state sync")
	if err := ss.VM.SetState(snow.StateSyncing); err != nil {
		return fmt.Errorf("failed to notify VM that state syncing has started: %w", err)
	}

	// clear up messages trackers
	ss.weightedSummaries = make(map[ids.ID]weightedSummary)

	ss.targetSeeders.Clear()
	ss.contactedSeeders.Clear()
	ss.failedSeeders.Clear()
	ss.targetVoters.Clear()
	ss.contactedVoters.Clear()
	ss.failedVoters.Clear()

	// sample K beacons to retrieve frontier from
	beacons, err := ss.StateSyncBeacons.Sample(ss.Config.SampleK)
	if err != nil {
		return err
	}

	ss.frontierSeeders = validators.NewSet()
	if err = ss.frontierSeeders.Set(beacons); err != nil {
		return err
	}

	for _, vdr := range beacons {
		vdrID := vdr.ID()
		ss.targetSeeders.Add(vdrID)
	}

	// list all beacons, to reach them for voting on frontier
	for _, vdr := range ss.StateSyncBeacons.List() {
		vdrID := vdr.ID()
		ss.targetVoters.Add(vdrID)
	}

	// check if there is an ongoing state sync; if so add its state summary
	// to the frontier to request votes on
	// Note: database.ErrNotFound means there is no ongoing summary
	localSummary, err := ss.stateSyncVM.GetOngoingSyncStateSummary()
	switch err {
	case database.ErrNotFound:
		// no action needed
	case nil:
		ss.locallyAvailableSummary = localSummary
		ss.weightedSummaries[localSummary.ID()] = weightedSummary{
			summary: localSummary,
		}
	default:
		return err
	}

	// initiate messages exchange
	ss.attempts++
	if ss.targetSeeders.Len() == 0 {
		ss.Ctx.Log.Info("State syncing skipped due to no provided syncers")
		return ss.onDoneStateSyncing(ss.requestID)
	}

	ss.requestID++
	ss.sendGetStateSummaryFrontiers()
	return nil
}

func (ss *stateSyncer) restart() error {
	if ss.attempts > 0 && ss.attempts%ss.RetryBootstrapWarnFrequency == 0 {
		ss.Ctx.Log.Info("continuing to attempt to state sync after %d failed attempts. Is this node connected to the internet?",
			ss.attempts)
	}

	return ss.startup()
}

// Ask up to [maxOutstandingStateSyncRequests] state sync validators at times
// to send their accepted state summary. It is called again until there are
// no more seeders to be reached in the pending set
func (ss *stateSyncer) sendGetStateSummaryFrontiers() {
	vdrs := ids.NewNodeIDSet(1)
	for ss.targetSeeders.Len() > 0 && vdrs.Len() < maxOutstandingStateSyncRequests {
		vdr, _ := ss.targetSeeders.Pop()
		vdrs.Add(vdr)
	}

	if vdrs.Len() > 0 {
		ss.Sender.SendGetStateSummaryFrontier(vdrs, ss.requestID)
		ss.contactedSeeders.Add(vdrs.List()...)
	}
}

// Ask up to [maxOutstandingStateSyncRequests] syncers validators to send
// their filtered accepted frontier. It is called again until there are
// no more voters to be reached in the pending set.
func (ss *stateSyncer) sendGetAccepted() error {
	// pick voters to contact
	vdrs := ids.NewNodeIDSet(1)
	for ss.targetVoters.Len() > 0 && vdrs.Len() < maxOutstandingStateSyncRequests {
		vdr, _ := ss.targetVoters.Pop()
		vdrs.Add(vdr)
	}

	if len(vdrs) == 0 {
		return nil
	}

	acceptedSummaryHeights := make([]uint64, 0, len(ss.weightedSummaries))
	seenHeights := make(map[uint64]struct{}, len(ss.weightedSummaries))
	for _, ws := range ss.weightedSummaries {
		height := ws.summary.Height()
		if _, seen := seenHeights[height]; seen {
			continue // avoid passing duplicate heights to SendGetAcceptedStateSummary
		}
		seenHeights[height] = struct{}{}
		acceptedSummaryHeights = append(acceptedSummaryHeights, height)
	}
	ss.Sender.SendGetAcceptedStateSummary(vdrs, ss.requestID, acceptedSummaryHeights)
	ss.contactedVoters.Add(vdrs.List()...)
	ss.Ctx.Log.Debug("sent %d more GetAcceptedStateSummary messages with %d more to send",
		vdrs.Len(), ss.targetVoters.Len())
	return nil
}

func (ss *stateSyncer) AppRequest(nodeID ids.NodeID, requestID uint32, deadline time.Time, request []byte) error {
	return ss.VM.AppRequest(nodeID, requestID, deadline, request)
}

func (ss *stateSyncer) AppResponse(nodeID ids.NodeID, requestID uint32, response []byte) error {
	return ss.VM.AppResponse(nodeID, requestID, response)
}

func (ss *stateSyncer) AppRequestFailed(nodeID ids.NodeID, requestID uint32) error {
	return ss.VM.AppRequestFailed(nodeID, requestID)
}

func (ss *stateSyncer) Notify(msg common.Message) error {
	// if state sync and bootstrap is done, we shouldn't receive StateSyncDone from the VM
	ss.Ctx.Log.Verbo("snowman engine notified of %s from the vm", msg)
	switch msg {
	case common.PendingTxs:
		ss.Ctx.Log.Warn("Message %s received in state sync. Dropped.", msg.String())

	case common.StateSyncDone:
		return ss.onDoneStateSyncing(ss.requestID)

	default:
		ss.Ctx.Log.Warn("unexpected message from the VM: %s", msg)
	}
	return nil
}

func (ss *stateSyncer) Connected(nodeID ids.NodeID, nodeVersion version.Application) error {
	if err := ss.VM.Connected(nodeID, nodeVersion); err != nil {
		return err
	}

	if err := ss.WeightTracker.AddWeightForNode(nodeID); err != nil {
		return err
	}

	if ss.WeightTracker.EnoughConnectedWeight() && !ss.started {
		ss.started = true
		return ss.startup()
	}

	return nil
}

func (ss *stateSyncer) Disconnected(nodeID ids.NodeID) error {
	if err := ss.VM.Disconnected(nodeID); err != nil {
		return err
	}

	return ss.WeightTracker.RemoveWeightForNode(nodeID)
}

func (ss *stateSyncer) Gossip() error { return nil }

func (ss *stateSyncer) Shutdown() error {
	ss.Config.Ctx.Log.Info("shutting down state syncer")
	return ss.VM.Shutdown()
}

func (ss *stateSyncer) Context() *snow.ConsensusContext { return ss.Config.Ctx }

func (ss *stateSyncer) IsBootstrapped() bool { return ss.Ctx.GetState() == snow.NormalOp }

// Halt implements the InternalHandler interface
func (ss *stateSyncer) Halt() {}

// Timeout implements the InternalHandler interface
func (ss *stateSyncer) Timeout() error { return nil }

func (ss *stateSyncer) HealthCheck() (interface{}, error) {
	vmIntf, vmErr := ss.VM.HealthCheck()
	intf := map[string]interface{}{
		"consensus": struct{}{},
		"vm":        vmIntf,
	}
	return intf, vmErr
}

func (ss *stateSyncer) GetVM() common.VM { return ss.VM }

func (ss *stateSyncer) IsEnabled() (bool, error) {
	if ss.stateSyncVM == nil {
		// state sync is not implemented
		return false, nil
	}

	return ss.stateSyncVM.StateSyncEnabled()
}
