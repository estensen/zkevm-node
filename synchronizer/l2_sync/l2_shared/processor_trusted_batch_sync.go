package l2_shared

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/types"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	syncCommon "github.com/0xPolygonHermez/zkevm-node/synchronizer/common"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v4"
)

// BatchProcessMode is the mode for process a batch (full, incremental, reprocess, nothing)
type BatchProcessMode string

const (
	// FullProcessMode This batch is not on database, so is the first time we process it
	FullProcessMode BatchProcessMode = "full"
	// IncrementalProcessMode We have processed this batch before, and we have the intermediate state root, so is going to be process only new Tx
	IncrementalProcessMode BatchProcessMode = "incremental"
	// ReprocessProcessMode We have processed this batch before, but we don't have the intermediate state root, so we need to reprocess it
	ReprocessProcessMode BatchProcessMode = "reprocess"
	// NothingProcessMode The batch is already synchronized, so we don't need to process it
	NothingProcessMode BatchProcessMode = "nothing"
)

// ProcessData contains the data required to process a batch
type ProcessData struct {
	BatchNumber       uint64
	Mode              BatchProcessMode
	OldStateRoot      common.Hash
	OldAccInputHash   common.Hash
	BatchMustBeClosed bool
	// The batch in trusted node, it NEVER will be nil
	TrustedBatch *types.Batch
	// Current batch in state DB, it could be nil
	StateBatch  *state.Batch
	Now         time.Time
	Description string
	// DebugPrefix is used to log, must prefix all logs entries
	DebugPrefix string
}

// ProcessResponse contains the response of the process of a batch
type ProcessResponse struct {
	// ProcessBatchResponse have the NewStateRoot
	ProcessBatchResponse *state.ProcessBatchResponse
	// ClearCache force to clear cache for next execution
	ClearCache bool
	// UpdateBatch  update the batch for next execution
	UpdateBatch *state.Batch
	// UpdateBatchWithProcessBatchResponse update the batch (if not nil) with the data in ProcessBatchResponse
	UpdateBatchWithProcessBatchResponse bool
}

// SyncTrustedBatchExecutor is the interface that known how to process a batch
type SyncTrustedBatchExecutor interface {
	// FullProcess process a batch that is not on database, so is the first time we process it
	FullProcess(ctx context.Context, data *ProcessData, dbTx pgx.Tx) (*ProcessResponse, error)
	// IncrementalProcess process a batch that we have processed before, and we have the intermediate state root, so is going to be process only new Tx
	IncrementalProcess(ctx context.Context, data *ProcessData, dbTx pgx.Tx) (*ProcessResponse, error)
	// ReProcess process a batch that we have processed before, but we don't have the intermediate state root, so we need to reprocess it
	ReProcess(ctx context.Context, data *ProcessData, dbTx pgx.Tx) (*ProcessResponse, error)
	// NothingProcess process a batch that is already synchronized, so we don't need to process it
	NothingProcess(ctx context.Context, data *ProcessData, dbTx pgx.Tx) (*ProcessResponse, error)
	// CloseBatch close a batch
	//CloseBatch(ctx context.Context, trustedBatch *types.Batch, dbTx pgx.Tx) error
}

// ProcessorTrustedBatchSync is a template to sync trusted state. It classify what kind of update is needed and call to SyncTrustedStateBatchExecutorSteps
//
//	  that is the one that execute the sync process
//
//		the real implementation of the steps is in the SyncTrustedStateBatchExecutorSteps interface that known how to process a batch
type ProcessorTrustedBatchSync struct {
	Steps        SyncTrustedBatchExecutor
	timeProvider syncCommon.TimeProvider
}

// NewProcessorTrustedBatchSync creates a new SyncTrustedStateBatchExecutorTemplate
func NewProcessorTrustedBatchSync(steps SyncTrustedBatchExecutor,
	timeProvider syncCommon.TimeProvider) *ProcessorTrustedBatchSync {
	return &ProcessorTrustedBatchSync{
		Steps:        steps,
		timeProvider: timeProvider,
	}
}

// ProcessTrustedBatch processes a trusted batch and return the new state
func (s *ProcessorTrustedBatchSync) ProcessTrustedBatch(ctx context.Context, trustedBatch *types.Batch, status TrustedState, dbTx pgx.Tx, debugPrefix string) (*TrustedState, error) {
	log.Debugf("%s Processing trusted batch: %v", debugPrefix, trustedBatch.Number)
	// Duplicate batches to avoid interferences with cache
	var stateCurrentBatch *state.Batch = nil
	var statePreviousBatch *state.Batch = nil
	if status.LastTrustedBatches[0] != nil {
		tmpBatch := *status.LastTrustedBatches[0]
		stateCurrentBatch = &tmpBatch
	}
	if status.LastTrustedBatches[1] != nil {
		tmpBatch := *status.LastTrustedBatches[1]
		statePreviousBatch = &tmpBatch
	}
	processMode, err := s.getModeForProcessBatch(trustedBatch, stateCurrentBatch, statePreviousBatch)
	processMode.DebugPrefix = fmt.Sprintf("%s mode %s:", debugPrefix, processMode.Mode)
	if err != nil {
		log.Error("%s error getting processMode. Error: ", debugPrefix, trustedBatch.Number, err)
		return nil, err
	}
	log.Infof("%s  Processing trusted batch: mode=%s desc=%s", processMode.DebugPrefix, processMode.Mode, processMode.Description)
	var processBatchResp *ProcessResponse = nil
	switch processMode.Mode {
	case NothingProcessMode:
		log.Debugf("%s  is already synchronized", processMode.DebugPrefix, trustedBatch.Number)
		err = nil
	case FullProcessMode:
		log.Debugf("%s is not on database, so is the first time we process it", debugPrefix)
		processBatchResp, err = s.Steps.FullProcess(ctx, &processMode, dbTx)
	case IncrementalProcessMode:
		log.Debugf("%s is partially synchronized", processMode.DebugPrefix)
		processBatchResp, err = s.Steps.IncrementalProcess(ctx, &processMode, dbTx)
	case ReprocessProcessMode:
		log.Debugf("%s is partially synchronized but we don't have intermediate stateRoot so it needs to be fully reprocessed", processMode.DebugPrefix)
		processBatchResp, err = s.Steps.ReProcess(ctx, &processMode, dbTx)
	}
	if err != nil {
		log.Errorf("%s error processing trusted batch. Error: %s", processMode.DebugPrefix, err)
		return nil, err
	}

	if processMode.BatchMustBeClosed {
		err = checkProcessBatchResultMatchExpected(&processMode, processBatchResp.ProcessBatchResponse)
		if err != nil {
			log.Error("%s error verifying batch result!  Error: ", debugPrefix, err)
			return nil, err
		}
	}

	if processBatchResp != nil && !processBatchResp.ClearCache {
		newStatus := updateCache(status, processBatchResp, processMode.BatchMustBeClosed)
		log.Debugf("%s Batch %v synchronized, updated cache for next run", debugPrefix, trustedBatch.Number)
		return &newStatus, nil
	} else {
		log.Debugf("%s Batch %v synchronized -> clear cache", debugPrefix, trustedBatch.Number)
		return nil, nil
	}
}

func updateCache(status TrustedState, response *ProcessResponse, closedBatch bool) TrustedState {
	res := TrustedState{
		LastTrustedBatches: []*state.Batch{nil, nil},
	}
	if response == nil || response.ClearCache {
		return res
	}
	if response.UpdateBatch != nil {
		res.LastTrustedBatches[0] = response.UpdateBatch
	}
	if response.ProcessBatchResponse != nil && response.UpdateBatchWithProcessBatchResponse && res.LastTrustedBatches[0] != nil {
		//if res.LastTrustedBatches[0].BatchNumber != uint64(response.ProcessBatchResponse.NewBatchNumber) {
		//	panic(fmt.Sprintf("BatchNumber mismatch. Expected %v, got %v", res.LastTrustedBatches[0].BatchNumber, response.ProcessBatchResponse.NewBatchNumber))
		//}
		res.LastTrustedBatches[0].StateRoot = response.ProcessBatchResponse.NewStateRoot
		res.LastTrustedBatches[0].LocalExitRoot = response.ProcessBatchResponse.NewLocalExitRoot
		res.LastTrustedBatches[0].AccInputHash = response.ProcessBatchResponse.NewAccInputHash
		res.LastTrustedBatches[0].WIP = !closedBatch
	}
	if closedBatch {
		res.LastTrustedBatches[1] = res.LastTrustedBatches[0]
		res.LastTrustedBatches[0] = nil
	}
	return res
}

func (s *ProcessorTrustedBatchSync) getModeForProcessBatch(trustedNodeBatch *types.Batch, stateBatch *state.Batch, statePreviousBatch *state.Batch) (ProcessData, error) {
	// Check parameters
	if trustedNodeBatch == nil || statePreviousBatch == nil {
		return ProcessData{}, fmt.Errorf("trustedNodeBatch and statePreviousBatch can't be nil")
	}

	var result ProcessData
	if stateBatch == nil {
		result = ProcessData{
			Mode:         FullProcessMode,
			OldStateRoot: statePreviousBatch.StateRoot,
			Description:  "Batch is not on database, so is the first time we process it",
		}
	} else {
		batchSynced, strSync := checkIfSynced(stateBatch, trustedNodeBatch)
		if batchSynced {
			// "The batch from Node, and the one in database are the same, already synchronized",
			result = ProcessData{
				Mode:         NothingProcessMode,
				OldStateRoot: common.Hash{},
				Description:  "no new data on batch",
			}
		} else {
			// We have a previous batch, but in node something change
			// We have processed this batch before, and we have the intermediate state root, so is going to be process only new Tx.
			if stateBatch.StateRoot != state.ZeroHash {
				result = ProcessData{
					Mode:         IncrementalProcessMode,
					OldStateRoot: stateBatch.StateRoot,
					Description:  "batch exists + intermediateStateRoot " + strSync,
				}
			} else {
				// We have processed this batch before, but we don't have the intermediate state root, so we need to reprocess all txs.
				result = ProcessData{
					Mode:         ReprocessProcessMode,
					OldStateRoot: statePreviousBatch.StateRoot,
					Description:  "batch exists + StateRoot==Zero" + strSync,
				}
			}
		}
	}
	if result.Mode == "" {
		return result, fmt.Errorf("failed to get mode for process batch %v", trustedNodeBatch.Number)
	}
	result.BatchNumber = uint64(trustedNodeBatch.Number)
	result.BatchMustBeClosed = result.Mode != NothingProcessMode && isTrustedBatchClosed(trustedNodeBatch)
	result.StateBatch = stateBatch
	result.TrustedBatch = trustedNodeBatch
	result.OldAccInputHash = statePreviousBatch.AccInputHash
	result.Now = s.timeProvider.Now()
	return result, nil
}

func isTrustedBatchClosed(batch *types.Batch) bool {
	return batch.Closed
}
func checkIfSynced(stateBatch *state.Batch, trustedBatch *types.Batch) (bool, string) {
	ok, str := checkIfSyncedWhitoutWIP(stateBatch, trustedBatch)
	if stateBatch.WIP != !trustedBatch.Closed {
		str += "matchWIP: false, "
		ok = false
	}
	return ok, str
}

// Retruns true|false and a debug string
func checkIfSyncedWhitoutWIP(stateBatch *state.Batch, trustedBatch *types.Batch) (bool, string) {
	if stateBatch == nil || trustedBatch == nil {
		log.Infof("checkIfSynced stateBatch or trustedBatch is nil, so is not synced")
		return false, "nil pointers"
	}
	matchNumber := stateBatch.BatchNumber == uint64(trustedBatch.Number)
	matchGER := stateBatch.GlobalExitRoot.String() == trustedBatch.GlobalExitRoot.String()
	matchLER := stateBatch.LocalExitRoot.String() == trustedBatch.LocalExitRoot.String()
	matchSR := stateBatch.StateRoot.String() == trustedBatch.StateRoot.String()
	matchCoinbase := stateBatch.Coinbase.String() == trustedBatch.Coinbase.String()
	// TODO: Check uint64(trustedBatch.Timestamp) <= uint64(stateBatch.Timestamp.Unix()) but pending issue #2953
	matchTimestamp := true
	matchL2Data := hex.EncodeToString(stateBatch.BatchL2Data) == hex.EncodeToString(trustedBatch.BatchL2Data)

	if matchNumber && matchGER && matchLER && matchSR &&
		matchCoinbase && matchTimestamp && matchL2Data {
		return true, fmt.Sprintf("Equal batch: %v", stateBatch.BatchNumber)
	}
	log.Debug("matchNumber: ", matchNumber)
	log.Debug("matchGER: ", matchGER)
	log.Debug("matchLER: ", matchLER)
	log.Debug("matchSR: ", matchSR)
	log.Debug("matchCoinbase: ", matchCoinbase)
	log.Debug("matchTimestamp: ", matchTimestamp)
	log.Debug("matchL2Data: ", matchL2Data)
	debugStrResult := ""
	values := []bool{matchNumber, matchGER, matchLER, matchSR, matchCoinbase, matchTimestamp, matchL2Data}
	names := []string{"matchNumber", "matchGER", "matchLER", "matchSR", "matchCoinbase", "matchTimestamp", "matchL2Data"}
	for i, v := range values {
		if !v {
			debugStrResult += fmt.Sprintf("%s: %v, ", names[i], v)
		}
	}
	return false, debugStrResult
}

func checkStateRootAndLER(batchNumber uint64, expectedStateRoot common.Hash, expectedLER common.Hash, calculatedStateRoot common.Hash, calculatedLER common.Hash) error {
	if calculatedStateRoot != expectedStateRoot {
		return fmt.Errorf("batch %v: stareRoot calculated [%s] is different from the one in the batch [%s]", batchNumber, calculatedStateRoot, expectedStateRoot)
	}
	if calculatedLER != expectedLER {
		return fmt.Errorf("batch %v: LocalExitRoot calculated [%s] is different from the one in the batch [%s]", batchNumber, calculatedLER, expectedLER)
	}
	return nil
}

func checkProcessBatchResultMatchExpected(data *ProcessData, processBatchResp *state.ProcessBatchResponse) error {
	var err error = nil
	var trustedBatch = data.TrustedBatch
	if trustedBatch == nil {
		panic("trustedBatch is nil")
	}
	if processBatchResp == nil {
		log.Warnf("Batch %v: Can't check  processBatchResp because is nil, then check store batch in DB", trustedBatch.Number)
		err = checkStateRootAndLER(uint64(trustedBatch.Number), trustedBatch.StateRoot, trustedBatch.LocalExitRoot, data.StateBatch.StateRoot, data.StateBatch.LocalExitRoot)
	} else {
		err = checkStateRootAndLER(uint64(trustedBatch.Number), trustedBatch.StateRoot, trustedBatch.LocalExitRoot, processBatchResp.NewStateRoot, processBatchResp.NewLocalExitRoot)
	}
	if err != nil {
		log.Error(err.Error())
	}
	return err
}
