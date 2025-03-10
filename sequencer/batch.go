package sequencer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/event"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/sequencer/metrics"
	"github.com/0xPolygonHermez/zkevm-node/state"
	stateMetrics "github.com/0xPolygonHermez/zkevm-node/state/metrics"
	"github.com/ethereum/go-ethereum/common"
)

// Batch represents a wip or processed batch.
type Batch struct {
	batchNumber        uint64
	coinbase           common.Address
	timestamp          time.Time
	initialStateRoot   common.Hash // initial stateRoot of the batch
	imStateRoot        common.Hash // intermediate stateRoot that is updated each time a single tx is processed
	finalStateRoot     common.Hash // final stateroot of the batch when a L2 block is processed
	localExitRoot      common.Hash
	countOfTxs         int
	remainingResources state.BatchResources
	closingReason      state.ClosingReason
}

func (w *Batch) isEmpty() bool {
	return w.countOfTxs == 0
}

// getLastStateRoot gets the state root from the latest batch
func (f *finalizer) getLastStateRoot(ctx context.Context) (common.Hash, error) {
	var oldStateRoot common.Hash

	batches, err := f.state.GetLastNBatches(ctx, 2, nil) //nolint:gomnd
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to get last %d batches, err: %w", 2, err) //nolint:gomnd
	}

	if len(batches) == 1 { //nolint:gomnd
		oldStateRoot = batches[0].StateRoot
	} else if len(batches) == 2 { //nolint:gomnd
		oldStateRoot = batches[1].StateRoot
	}

	return oldStateRoot, nil
}

// GetWIPBatch returns ready WIP batch
func (f *finalizer) setWIPBatch(ctx context.Context, wipStateBatch *state.Batch) (*Batch, error) {
	dbTx, err := f.state.BeginStateTransaction(ctx)
	if err != nil {
		return nil, err
	}

	// Retrieve prevStateBatch to init the initialStateRoot of the wip batch
	prevStateBatch, err := f.state.GetBatchByNumber(ctx, wipStateBatch.BatchNumber-1, dbTx)
	if err != nil {
		return nil, err
	}

	wipStateBatchBlocks, err := state.DecodeBatchV2(wipStateBatch.BatchL2Data)
	if err != nil {
		return nil, err
	}

	// Count the number of txs in the wip state batch
	wipStateBatchCountOfTxs := 0
	for _, rawBlock := range wipStateBatchBlocks.Blocks {
		wipStateBatchCountOfTxs = wipStateBatchCountOfTxs + len(rawBlock.Transactions)
	}

	remainingResources := getMaxRemainingResources(f.batchConstraints)
	err = remainingResources.Sub(wipStateBatch.Resources)
	if err != nil {
		return nil, err
	}

	wipBatch := &Batch{
		batchNumber:        wipStateBatch.BatchNumber,
		coinbase:           wipStateBatch.Coinbase,
		imStateRoot:        wipStateBatch.StateRoot,
		initialStateRoot:   prevStateBatch.StateRoot,
		finalStateRoot:     wipStateBatch.StateRoot,
		localExitRoot:      wipStateBatch.LocalExitRoot,
		timestamp:          wipStateBatch.Timestamp,
		countOfTxs:         wipStateBatchCountOfTxs,
		remainingResources: remainingResources,
	}

	return wipBatch, nil
}

// initWIPBatch inits the wip batch
func (f *finalizer) initWIPBatch(ctx context.Context) {
	for !f.isSynced(ctx) {
		log.Info("wait for synchronizer to sync last batch")
		time.Sleep(time.Second)
	}

	lastBatchNum, err := f.state.GetLastBatchNumber(ctx, nil)
	if err != nil {
		log.Fatalf("failed to get last batch number. Error: %s", err)
	}

	// Get the last batch in trusted state
	lastStateBatch, err := f.state.GetBatchByNumber(ctx, lastBatchNum, nil)
	if err != nil {
		log.Fatalf("failed to get last batch. Error: %s", err)
	}

	isClosed := !lastStateBatch.WIP

	log.Infof("batch %d isClosed: %v", lastBatchNum, isClosed)

	if isClosed { //if the last batch is close then open a new wip batch
		// Get las GlobalExitRoot
		f.lastL1InfoTreeMux.Lock()
		lastGER := f.lastL1InfoTree.GlobalExitRoot.GlobalExitRoot
		f.lastL1InfoTreeMux.Unlock()

		f.wipBatch, err = f.openNewWIPBatch(ctx, lastStateBatch.BatchNumber+1, lastGER, lastStateBatch.StateRoot, lastStateBatch.LocalExitRoot)
		if err != nil {
			log.Fatalf("failed to open new wip batch. Error: %s", err)
		}
	} else { /// if it's not closed, it is the wip state batch, set it as wip batch in the finalizer
		f.wipBatch, err = f.setWIPBatch(ctx, lastStateBatch)
		if err != nil {
			log.Fatalf("failed to set wip batch. Error: %s", err)
		}
	}

	log.Infof("initial batch: %d, initialStateRoot: %s, stateRoot: %s, coinbase: %s, LER: %s",
		f.wipBatch.batchNumber, f.wipBatch.initialStateRoot, f.wipBatch.finalStateRoot, f.wipBatch.coinbase, f.wipBatch.localExitRoot)
}

// finalizeBatch retries until successful closes the current batch and opens a new one, potentially processing forced batches between the batch is closed and the resulting new empty batch
func (f *finalizer) finalizeBatch(ctx context.Context) {
	start := time.Now()
	defer func() {
		metrics.ProcessingTime(time.Since(start))
	}()

	var err error
	f.wipBatch, err = f.closeAndOpenNewWIPBatch(ctx)
	if err != nil {
		f.Halt(ctx, fmt.Errorf("failed to create new WIP batch. Error: %s", err))
	}

	log.Infof("new WIP batch %d", f.wipBatch.batchNumber)
}

// closeAndOpenNewWIPBatch closes the current batch and opens a new one, potentially processing forced batches between the batch is closed and the resulting new empty batch
func (f *finalizer) closeAndOpenNewWIPBatch(ctx context.Context) (*Batch, error) {
	// Finalize the wip L2 block if it has transactions, if not we keep it open to store it in the new wip batch
	if !f.wipL2Block.isEmpty() {
		f.finalizeL2Block(ctx)
	}

	// Wait until all L2 blocks are processed
	startWait := time.Now()
	f.pendingL2BlocksToProcessWG.Wait()
	endWait := time.Now()
	log.Debugf("waiting for pending L2 blocks to be processed took: %s", endWait.Sub(startWait).String())

	// Wait until all L2 blocks are store
	startWait = time.Now()
	f.pendingL2BlocksToStoreWG.Wait()
	endWait = time.Now()
	log.Debugf("waiting for pending L2 blocks to be stored took: %s", endWait.Sub(startWait).String())

	var err error

	// We need to process the batch to update the state root before closing the batch
	if f.wipBatch.initialStateRoot == f.wipBatch.finalStateRoot {
		log.Info("reprocessing batch because the state root has not changed...")
		_, err = f.processTransaction(ctx, nil, true)
		if err != nil {
			return nil, err
		}
	}

	// Reprocess full batch as sanity check
	if f.cfg.SequentialReprocessFullBatch {
		// Do the full batch reprocess now
		_, err := f.reprocessFullBatch(ctx, f.wipBatch.batchNumber, f.wipBatch.initialStateRoot, f.wipBatch.finalStateRoot)
		if err != nil {
			// There is an error reprocessing the batch. We halt the execution of the Sequencer at this point
			return nil, fmt.Errorf("halting Sequencer because of error reprocessing full batch %d (sanity check). Error: %s ", f.wipBatch.batchNumber, err)
		}
	} else {
		// Do the full batch reprocess in parallel
		go func() {
			_, _ = f.reprocessFullBatch(ctx, f.wipBatch.batchNumber, f.wipBatch.initialStateRoot, f.wipBatch.finalStateRoot)
		}()
	}

	// Close the wip batch
	err = f.closeWIPBatch(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to close batch, err: %w", err)
	}

	log.Infof("batch %d closed", f.wipBatch.batchNumber)

	//TODO: Call DSUpdateGER function
	// Check if the batch is empty and sending a GER Update to the stream is needed
	//TODO: is this UpdateGER still needed?
	/*if f.streamServer != nil && f.wipBatch.isEmpty() && f.currentGERHash != f.previousGERHash {
		updateGer := state.DSUpdateGER{
			BatchNumber:    f.wipBatch.batchNumber,
			Timestamp:      f.wipBatch.timestamp.Unix(),
			GlobalExitRoot: f.wipBatch.globalExitRoot,
			Coinbase:       f.sequencerAddress,
			ForkID:         uint16(f.state.GetForkIDByBatchNumber(f.wipBatch.batchNumber)),
			StateRoot:      f.wipBatch.finalStateRoot,
		}

		err = f.streamServer.StartAtomicOp()
		if err != nil {
			log.Errorf("failed to start atomic op for Update GER on batch %v: %v", f.wipBatch.batchNumber, err)
		}

		_, err = f.streamServer.AddStreamEntry(state.EntryTypeUpdateGER, updateGer.Encode())
		if err != nil {
			log.Errorf("failed to add stream entry for Update GER on batch %v: %v", f.wipBatch.batchNumber, err)
		}

		err = f.streamServer.CommitAtomicOp()
		if err != nil {
			log.Errorf("failed to commit atomic op for Update GER on batch  %v: %v", f.wipBatch.batchNumber, err)
		}
	}*/

	// Metadata for the next batch
	stateRoot := f.wipBatch.finalStateRoot
	lastBatchNumber := f.wipBatch.batchNumber

	// Process forced batches
	if len(f.nextForcedBatches) > 0 {
		lastBatchNumber, stateRoot = f.processForcedBatches(ctx, lastBatchNumber, stateRoot)
		// We must init/reset the wip L2 block from the state since processForcedBatches has created new L2 blocks
		f.initWIPL2Block(ctx)
	}

	currentGER := f.wipL2Block.l1InfoTreeExitRoot.GlobalExitRoot.GlobalExitRoot

	batch, err := f.openNewWIPBatch(ctx, lastBatchNumber+1, currentGER, stateRoot, f.wipBatch.localExitRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to open new wip batch. Error: %s", err)
	}

	// Subtract the L2 block used resources to wip batch
	err = f.wipBatch.remainingResources.Sub(l2BlockUsedResources)
	if err != nil {
		return nil, fmt.Errorf("failed to subtract L2 block used resources to wip batch %d. Error: %s", f.wipBatch.batchNumber, err)
	}

	return batch, nil
}

// openNewWIPBatch opens a new batch in the state and returns it as WipBatch
func (f *finalizer) openNewWIPBatch(ctx context.Context, batchNumber uint64, ger, stateRoot, LER common.Hash) (*Batch, error) {
	// open next batch
	newStateBatch := state.Batch{
		BatchNumber:    batchNumber,
		Coinbase:       f.sequencerAddress,
		Timestamp:      now(),
		GlobalExitRoot: ger,
		StateRoot:      stateRoot,
		LocalExitRoot:  LER,
	}

	dbTx, err := f.state.BeginStateTransaction(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin state transaction to open batch, err: %w", err)
	}

	// OpenBatch opens a new wip batch in the state
	err = f.state.OpenWIPBatch(ctx, newStateBatch, dbTx)
	if err != nil {
		if rollbackErr := dbTx.Rollback(ctx); rollbackErr != nil {
			return nil, fmt.Errorf("failed to rollback dbTx: %s. Error: %w", rollbackErr.Error(), err)
		}
		return nil, fmt.Errorf("failed to open new wip batch. Error: %w", err)
	}

	if err := dbTx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit database transaction for opening a wip batch. Error: %w", err)
	}

	// Check if synchronizer is up-to-date
	for !f.isSynced(ctx) {
		log.Info("wait for synchronizer to sync last batch")
		time.Sleep(time.Second)
	}

	return &Batch{
		batchNumber:        newStateBatch.BatchNumber,
		coinbase:           newStateBatch.Coinbase,
		initialStateRoot:   newStateBatch.StateRoot,
		imStateRoot:        newStateBatch.StateRoot,
		finalStateRoot:     newStateBatch.StateRoot,
		timestamp:          newStateBatch.Timestamp,
		localExitRoot:      newStateBatch.LocalExitRoot,
		remainingResources: getMaxRemainingResources(f.batchConstraints),
		closingReason:      state.EmptyClosingReason,
	}, err
}

// closeWIPBatch closes the current batch in the state
func (f *finalizer) closeWIPBatch(ctx context.Context) error {
	/*transactions, effectivePercentages, err := f.dbManager.GetTransactionsByBatchNumber(ctx, f.wipBatch.batchNumber)
	if err != nil {
		return fmt.Errorf("failed to get transactions from transactions, err: %w", err)
	}
	for i, tx := range transactions {
		log.Debugf("[closeWIPBatch] BatchNum: %d, Tx position: %d, txHash: %s", f.wipBatch.batchNumber, i, tx.Hash().String())
	}*/
	usedResources := getUsedBatchResources(f.batchConstraints, f.wipBatch.remainingResources)
	receipt := state.ProcessingReceipt{
		BatchNumber:    f.wipBatch.batchNumber,
		BatchResources: usedResources,
		ClosingReason:  f.wipBatch.closingReason,
	}

	dbTx, err := f.state.BeginStateTransaction(ctx)
	if err != nil {
		return err
	}

	err = f.state.CloseWIPBatch(ctx, receipt, dbTx)
	if err != nil {
		err2 := dbTx.Rollback(ctx)
		if err2 != nil {
			log.Errorf("[CloseWIPBatch] error rolling back: %v", err2)
		}
		return err
	} else {
		err := dbTx.Commit(ctx)
		if err != nil {
			log.Errorf("[CloseWIPBatch] error committing: %v", err)
			return err
		}
	}

	return nil
}

// maxTxsPerBatchReached checks if the batch has reached the maximum number of txs per batch
func (f *finalizer) maxTxsPerBatchReached() bool {
	if f.wipBatch.countOfTxs >= int(f.batchConstraints.MaxTxsPerBatch) {
		log.Infof("closing batch: %d, because it reached the maximum number of txs.", f.wipBatch.batchNumber)
		f.wipBatch.closingReason = state.BatchFullClosingReason
		return true
	}
	return false
}

// reprocessFullBatch reprocesses a batch used as sanity check
func (f *finalizer) reprocessFullBatch(ctx context.Context, batchNum uint64, initialStateRoot common.Hash, expectedNewStateRoot common.Hash) (*state.ProcessBatchResponse, error) {
	reprocessError := func(batch *state.Batch) {
		if batch == nil {
			return
		}

		rawL2Blocks, err := state.DecodeBatchV2(batch.BatchL2Data)
		if err != nil {
			log.Errorf("[reprocessFullBatch] error decoding BatchL2Data for batch %d. Error: %s", batch.BatchNumber, err)
			return
		}

		// Log batch detailed info
		log.Infof("[reprocessFullBatch] BatchNumber: %d, InitialStateRoot: %s, ExpectedNewStateRoot: %s", batch.BatchNumber, initialStateRoot, expectedNewStateRoot)
		for i, rawL2block := range rawL2Blocks.Blocks {
			for j, rawTx := range rawL2block.Transactions {
				log.Infof("[reprocessFullBatch] BatchNumber: %d, block position: % d, tx position %d, tx hash: %s", batch.BatchNumber, i, j, rawTx.Tx.Hash())
			}
		}

		f.Halt(ctx, fmt.Errorf("error reprocessing full batch (sanity check). Check previous errors in logs to know which was the cause"))
	}

	log.Debugf("[reprocessFullBatch] reprocessing batch: %d, InitialStateRoot: %s, ExpectedNewStateRoot: %s", batchNum, initialStateRoot, expectedNewStateRoot)

	batch, err := f.state.GetBatchByNumber(ctx, batchNum, nil)
	if err != nil {
		log.Errorf("[reprocessFullBatch] failed to get batch %d. Error: %w", batchNum, err)
		reprocessError(nil)
		return nil, ErrGetBatchByNumber
	}

	caller := stateMetrics.DiscardCallerLabel
	if f.cfg.SequentialReprocessFullBatch {
		caller = stateMetrics.SequencerCallerLabel
	}

	executorBatchRequest := state.ProcessRequest{
		BatchNumber:             batch.BatchNumber,
		L1InfoRoot_V2:           mockL1InfoRoot,
		OldStateRoot:            initialStateRoot,
		Transactions:            batch.BatchL2Data,
		Coinbase:                batch.Coinbase,
		TimestampLimit_V2:       uint64(time.Now().Unix()),
		ForkID:                  f.state.GetForkIDByBatchNumber(batch.BatchNumber),
		SkipVerifyL1InfoRoot_V2: true,
		Caller:                  caller,
	}
	executorBatchRequest.L1InfoTreeData_V2, _, err = f.state.GetL1InfoTreeDataFromBatchL2Data(ctx, batch.BatchL2Data, nil)
	if err != nil {
		log.Errorf("[reprocessFullBatch] failed to get L1InfoTreeData for batch %d. Error: %w", batch.BatchNumber, err)
		reprocessError(nil)
		return nil, ErrGetBatchByNumber
	}

	var result *state.ProcessBatchResponse

	result, err = f.state.ProcessBatchV2(ctx, executorBatchRequest, false)
	if err != nil {
		log.Errorf("[reprocessFullBatch] failed to process batch %d. Error: %s", batch.BatchNumber, err)
		reprocessError(batch)
		return nil, ErrProcessBatch
	}

	if result.ExecutorError != nil {
		log.Errorf("[reprocessFullBatch] executor error when reprocessing batch %d, error: %s", batch.BatchNumber, result.ExecutorError)
		reprocessError(batch)
		return nil, ErrExecutorError
	}

	if result.IsRomOOCError {
		log.Errorf("[reprocessFullBatch] failed to process batch %d because OutOfCounters", batch.BatchNumber)
		reprocessError(batch)

		payload, err := json.Marshal(executorBatchRequest)
		if err != nil {
			log.Errorf("[reprocessFullBatch] error marshaling payload: %s", err)
		} else {
			event := &event.Event{
				ReceivedAt:  time.Now(),
				Source:      event.Source_Node,
				Component:   event.Component_Sequencer,
				Level:       event.Level_Critical,
				EventID:     event.EventID_ReprocessFullBatchOOC,
				Description: string(payload),
				Json:        executorBatchRequest,
			}
			err = f.eventLog.LogEvent(ctx, event)
			if err != nil {
				log.Errorf("[reprocessFullBatch] error storing payload: %s", err)
			}
		}

		return nil, ErrProcessBatchOOC
	}

	if result.NewStateRoot != expectedNewStateRoot {
		log.Errorf("[reprocessFullBatch] new state root mismatch for batch %d, expected: %s, got: %s", batch.BatchNumber, expectedNewStateRoot.String(), result.NewStateRoot.String())
		reprocessError(batch)
		return nil, ErrStateRootNoMatch
	}

	log.Infof("[reprocessFullBatch]: reprocess successfully done for batch %d", batch.BatchNumber)
	return result, nil
}

// checkRemainingResources checks if the transaction uses less resources than the remaining ones in the batch.
func (f *finalizer) checkRemainingResources(result *state.ProcessBatchResponse, tx *TxTracker) error {
	usedResources := state.BatchResources{
		ZKCounters: result.UsedZkCounters,
		Bytes:      uint64(len(tx.RawTx)),
	}

	err := f.wipBatch.remainingResources.Sub(usedResources)
	if err != nil {
		log.Infof("current transaction exceeds the remaining batch resources, updating metadata for tx in worker and continuing")
		start := time.Now()
		f.worker.UpdateTxZKCounters(result.BlockResponses[0].TransactionResponses[0].TxHash, tx.From, usedResources.ZKCounters)
		metrics.WorkerProcessingTime(time.Since(start))
		return err
	}

	return nil
}

// isBatchResourcesExhausted checks if one of resources of the wip batch has reached the max value
func (f *finalizer) isBatchResourcesExhausted() bool {
	resources := f.wipBatch.remainingResources
	zkCounters := resources.ZKCounters
	result := false
	resourceDesc := ""
	if resources.Bytes <= f.getConstraintThresholdUint64(f.batchConstraints.MaxBatchBytesSize) {
		resourceDesc = "MaxBatchBytesSize"
		result = true
	} else if zkCounters.UsedSteps <= f.getConstraintThresholdUint32(f.batchConstraints.MaxSteps) {
		resourceDesc = "MaxSteps"
		result = true
	} else if zkCounters.UsedPoseidonPaddings <= f.getConstraintThresholdUint32(f.batchConstraints.MaxPoseidonPaddings) {
		resourceDesc = "MaxPoseidonPaddings"
		result = true
	} else if zkCounters.UsedBinaries <= f.getConstraintThresholdUint32(f.batchConstraints.MaxBinaries) {
		resourceDesc = "MaxBinaries"
		result = true
	} else if zkCounters.UsedKeccakHashes <= f.getConstraintThresholdUint32(f.batchConstraints.MaxKeccakHashes) {
		resourceDesc = "MaxKeccakHashes"
		result = true
	} else if zkCounters.UsedArithmetics <= f.getConstraintThresholdUint32(f.batchConstraints.MaxArithmetics) {
		resourceDesc = "MaxArithmetics"
		result = true
	} else if zkCounters.UsedMemAligns <= f.getConstraintThresholdUint32(f.batchConstraints.MaxMemAligns) {
		resourceDesc = "MaxMemAligns"
		result = true
	} else if zkCounters.GasUsed <= f.getConstraintThresholdUint64(f.batchConstraints.MaxCumulativeGasUsed) {
		resourceDesc = "MaxCumulativeGasUsed"
		result = true
	} else if zkCounters.UsedSha256Hashes_V2 <= f.getConstraintThresholdUint32(f.batchConstraints.MaxSHA256Hashes) {
		resourceDesc = "MaxSHA256Hashes"
		result = true
	}

	if result {
		log.Infof("closing batch %d, because it reached %s limit", f.wipBatch.batchNumber, resourceDesc)
		f.wipBatch.closingReason = state.BatchAlmostFullClosingReason
	}

	return result
}

// getConstraintThresholdUint64 returns the threshold for the given input
func (f *finalizer) getConstraintThresholdUint64(input uint64) uint64 {
	return input * uint64(f.cfg.ResourcePercentageToCloseBatch) / 100 //nolint:gomnd
}

// getConstraintThresholdUint32 returns the threshold for the given input
func (f *finalizer) getConstraintThresholdUint32(input uint32) uint32 {
	return input * f.cfg.ResourcePercentageToCloseBatch / 100 //nolint:gomnd
}

// getUsedBatchResources returns the max resources that can be used in a batch
func getUsedBatchResources(constraints state.BatchConstraintsCfg, remainingResources state.BatchResources) state.BatchResources {
	return state.BatchResources{
		ZKCounters: state.ZKCounters{
			GasUsed:              constraints.MaxCumulativeGasUsed - remainingResources.ZKCounters.GasUsed,
			UsedKeccakHashes:     constraints.MaxKeccakHashes - remainingResources.ZKCounters.UsedKeccakHashes,
			UsedPoseidonHashes:   constraints.MaxPoseidonHashes - remainingResources.ZKCounters.UsedPoseidonHashes,
			UsedPoseidonPaddings: constraints.MaxPoseidonPaddings - remainingResources.ZKCounters.UsedPoseidonPaddings,
			UsedMemAligns:        constraints.MaxMemAligns - remainingResources.ZKCounters.UsedMemAligns,
			UsedArithmetics:      constraints.MaxArithmetics - remainingResources.ZKCounters.UsedArithmetics,
			UsedBinaries:         constraints.MaxBinaries - remainingResources.ZKCounters.UsedBinaries,
			UsedSteps:            constraints.MaxSteps - remainingResources.ZKCounters.UsedSteps,
			UsedSha256Hashes_V2:  constraints.MaxSHA256Hashes - remainingResources.ZKCounters.UsedSha256Hashes_V2,
		},
		Bytes: constraints.MaxBatchBytesSize - remainingResources.Bytes,
	}
}

// getMaxRemainingResources returns the max zkcounters that can be used in a batch
func getMaxRemainingResources(constraints state.BatchConstraintsCfg) state.BatchResources {
	return state.BatchResources{
		ZKCounters: state.ZKCounters{
			GasUsed:              constraints.MaxCumulativeGasUsed,
			UsedKeccakHashes:     constraints.MaxKeccakHashes,
			UsedPoseidonHashes:   constraints.MaxPoseidonHashes,
			UsedPoseidonPaddings: constraints.MaxPoseidonPaddings,
			UsedMemAligns:        constraints.MaxMemAligns,
			UsedArithmetics:      constraints.MaxArithmetics,
			UsedBinaries:         constraints.MaxBinaries,
			UsedSteps:            constraints.MaxSteps,
			UsedSha256Hashes_V2:  constraints.MaxSHA256Hashes,
		},
		Bytes: constraints.MaxBatchBytesSize,
	}
}
