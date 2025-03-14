package state

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/event"
	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/merkletree"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime/executor"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetL2Hash computes the l2 hash of a transaction for testing purposes
func TestGetL2Hash(tx types.Transaction, sender common.Address) (common.Hash, error) {
	return getL2Hash(tx, sender)
}

// GetL2Hash computes the l2 hash of a transaction
func GetL2Hash(tx types.Transaction) (common.Hash, error) {
	sender, err := GetSender(tx)
	if err != nil {
		log.Debugf("error getting sender: %v", err)
	}

	return getL2Hash(tx, sender)
}

func getL2Hash(tx types.Transaction, sender common.Address) (common.Hash, error) {
	var input string
	input += formatL2TxHashParam(fmt.Sprintf("%x", tx.Nonce()))
	input += formatL2TxHashParam(fmt.Sprintf("%x", tx.GasPrice()))
	input += formatL2TxHashParam(fmt.Sprintf("%x", tx.Gas()))
	input += pad20Bytes(formatL2TxHashParam(fmt.Sprintf("%x", tx.To())))
	input += formatL2TxHashParam(fmt.Sprintf("%x", tx.Value()))
	if len(tx.Data()) > 0 {
		input += formatL2TxHashParam(fmt.Sprintf("%x", tx.Data()))
	}
	if sender != ZeroAddress {
		input += pad20Bytes(formatL2TxHashParam(fmt.Sprintf("%x", sender)))
	}

	h4Hash, err := merkletree.HashContractBytecode(common.Hex2Bytes(input))
	if err != nil {
		return common.Hash{}, err
	}

	return common.HexToHash(merkletree.H4ToString(h4Hash)), nil
}

// pad20Bytes pads the given address with 0s to make it 20 bytes long
func pad20Bytes(address string) string {
	const addressLength = 40

	if len(address) < addressLength {
		address = strings.Repeat("0", addressLength-len(address)) + address
	}
	return address
}

func formatL2TxHashParam(param string) string {
	param = strings.TrimLeft(param, "0x")

	if param == "00" || param == "" {
		return "00"
	}

	if len(param)%2 != 0 {
		param = "0" + param
	}

	return param
}

// GetSender gets the sender from the transaction's signature
func GetSender(tx types.Transaction) (common.Address, error) {
	signer := types.NewEIP155Signer(tx.ChainId())
	sender, err := signer.Sender(&tx)
	if err != nil {
		return common.Address{}, err
	}
	return sender, nil
}

// RlpFieldsToLegacyTx parses the rlp fields slice into a type.LegacyTx
// in this specific order:
//
// required fields:
// [0] Nonce    uint64
// [1] GasPrice *big.Int
// [2] Gas      uint64
// [3] To       *common.Address
// [4] Value    *big.Int
// [5] Data     []byte
//
// optional fields:
// [6] V        *big.Int
// [7] R        *big.Int
// [8] S        *big.Int
func RlpFieldsToLegacyTx(fields [][]byte, v, r, s []byte) (tx *types.LegacyTx, err error) {
	const (
		fieldsSizeWithoutChainID = 6
		fieldsSizeWithChainID    = 7
	)

	if len(fields) < fieldsSizeWithoutChainID {
		return nil, types.ErrTxTypeNotSupported
	}

	nonce := big.NewInt(0).SetBytes(fields[0]).Uint64()
	gasPrice := big.NewInt(0).SetBytes(fields[1])
	gas := big.NewInt(0).SetBytes(fields[2]).Uint64()
	var to *common.Address

	if fields[3] != nil && len(fields[3]) != 0 {
		tmp := common.BytesToAddress(fields[3])
		to = &tmp
	}
	value := big.NewInt(0).SetBytes(fields[4])
	data := fields[5]

	txV := big.NewInt(0).SetBytes(v)
	if len(fields) >= fieldsSizeWithChainID {
		chainID := big.NewInt(0).SetBytes(fields[6])

		// a = chainId * 2
		// b = v - 27
		// c = a + 35
		// v = b + c
		//
		// same as:
		// v = v-27+chainId*2+35
		a := new(big.Int).Mul(chainID, big.NewInt(double))
		b := new(big.Int).Sub(new(big.Int).SetBytes(v), big.NewInt(ether155V))
		c := new(big.Int).Add(a, big.NewInt(etherPre155V))
		txV = new(big.Int).Add(b, c)
	}

	txR := big.NewInt(0).SetBytes(r)
	txS := big.NewInt(0).SetBytes(s)

	return &types.LegacyTx{
		Nonce:    nonce,
		GasPrice: gasPrice,
		Gas:      gas,
		To:       to,
		Value:    value,
		Data:     data,
		V:        txV,
		R:        txR,
		S:        txS,
	}, nil
}

// StoreTransactions is used by the sequencer to add processed transactions into
// an open batch. If the batch already has txs, the processedTxs must be a super
// set of the existing ones, preserving order.
func (s *State) StoreTransactions(ctx context.Context, batchNumber uint64, processedBlocks []*ProcessBlockResponse, txsEGPLog []*EffectiveGasPriceLog, dbTx pgx.Tx) error {
	if dbTx == nil {
		return ErrDBTxNil
	}

	// Check if last batch is closed. Note that it's assumed that only the latest batch can be open
	isBatchClosed, err := s.IsBatchClosed(ctx, batchNumber, dbTx)
	if err != nil {
		return err
	}
	if isBatchClosed {
		return ErrBatchAlreadyClosed
	}

	for _, processedBlock := range processedBlocks {
		processedTxs := processedBlock.TransactionResponses
		// check existing txs vs parameter txs
		/*
			existingTxs, err := s.GetTxsHashesByBatchNumber(ctx, batchNumber, dbTx)
			if err != nil {
				return err
			}*/

		// TODO: Refactor
		/*
			if err := CheckSupersetBatchTransactions(existingTxs, processedTxs); err != nil {
				return err
			}
		*/

		processingContext, err := s.GetProcessingContext(ctx, batchNumber, dbTx)
		if err != nil {
			return err
		}

		// firstTxToInsert := len(existingTxs)

		firstTxToInsert := 0

		for i := firstTxToInsert; i < len(processedTxs); i++ {
			processedTx := processedTxs[i]
			// if the transaction has an intrinsic invalid tx error it means
			// the transaction has not changed the state, so we don't store it
			// and just move to the next
			if executor.IsIntrinsicError(executor.RomErrorCode(processedTx.RomError)) || errors.Is(processedTx.RomError, executor.RomErr(executor.RomError_ROM_ERROR_INVALID_RLP)) {
				continue
			}

			lastL2Block, err := s.GetLastL2Block(ctx, dbTx)
			if err != nil {
				return err
			}

			header := NewL2Header(&types.Header{
				Number:     new(big.Int).SetUint64(lastL2Block.Number().Uint64() + 1),
				ParentHash: lastL2Block.Hash(),
				Coinbase:   processingContext.Coinbase,
				Root:       processedTx.StateRoot,
				GasUsed:    processedTx.GasUsed,
				GasLimit:   s.cfg.MaxCumulativeGasUsed,
				Time:       uint64(processingContext.Timestamp.Unix()),
			})
			transactions := []*types.Transaction{&processedTx.Tx}

			receipt := GenerateReceipt(header.Number, processedTx)
			if !CheckLogOrder(receipt.Logs) {
				return fmt.Errorf("error: logs received from executor are not in order")
			}
			receipts := []*types.Receipt{receipt}

			// Create l2Block to be able to calculate its hash
			l2Block := NewL2Block(header, transactions, []*L2Header{}, receipts, &trie.StackTrie{})
			l2Block.ReceivedAt = processingContext.Timestamp

			receipt.BlockHash = l2Block.Hash()

			storeTxsEGPData := []StoreTxEGPData{{EGPLog: nil, EffectivePercentage: uint8(processedTx.EffectivePercentage)}}
			if txsEGPLog != nil {
				storeTxsEGPData[0].EGPLog = txsEGPLog[i]
			}

			// Store L2 block and its transaction
			if err := s.AddL2Block(ctx, batchNumber, l2Block, receipts, storeTxsEGPData, dbTx); err != nil {
				return err
			}
		}
	}
	return nil
}

// StoreL2Block stores a l2 block into the state
func (s *State) StoreL2Block(ctx context.Context, batchNumber uint64, l2Block *ProcessBlockResponse, txsEGPLog []*EffectiveGasPriceLog, dbTx pgx.Tx) error {
	if dbTx == nil {
		return ErrDBTxNil
	}

	log.Debugf("storing l2 block %d, txs %d, hash %s", l2Block.BlockNumber, len(l2Block.TransactionResponses), l2Block.BlockHash.String())
	start := time.Now()

	header := &types.Header{
		Number:     new(big.Int).SetUint64(l2Block.BlockNumber),
		ParentHash: l2Block.ParentHash,
		Coinbase:   l2Block.Coinbase,
		Root:       l2Block.BlockHash, //BlockHash is the StateRoot in Etrog
		GasUsed:    l2Block.GasUsed,
		GasLimit:   s.cfg.MaxCumulativeGasUsed,
		Time:       l2Block.Timestamp,
	}

	l2Header := NewL2Header(header)

	l2Header.GlobalExitRoot = l2Block.GlobalExitRoot
	l2Header.BlockInfoRoot = l2Block.BlockInfoRoot

	transactions := []*types.Transaction{}
	storeTxsEGPData := []StoreTxEGPData{}
	receipts := []*types.Receipt{}

	for i, txResponse := range l2Block.TransactionResponses {
		// if the transaction has an intrinsic invalid tx error it means
		// the transaction has not changed the state, so we don't store it
		if executor.IsIntrinsicError(executor.RomErrorCode(txResponse.RomError)) {
			continue
		}
		if executor.IsInvalidL2Block(executor.RomErrorCode(txResponse.RomError)) {
			continue
		}

		txResp := *txResponse
		transactions = append(transactions, &txResp.Tx)

		storeTxsEGPData = append(storeTxsEGPData, StoreTxEGPData{EGPLog: nil, EffectivePercentage: uint8(txResponse.EffectivePercentage)})
		if txsEGPLog != nil {
			storeTxsEGPData[i].EGPLog = txsEGPLog[i]
		}

		receipt := GenerateReceipt(header.Number, txResponse)
		receipts = append(receipts, receipt)
	}

	// Create block to be able to calculate its hash
	block := NewL2Block(l2Header, transactions, []*L2Header{}, receipts, &trie.StackTrie{})
	block.ReceivedAt = time.Unix(int64(l2Block.Timestamp), 0)

	for _, receipt := range receipts {
		receipt.BlockHash = block.Hash()
	}

	// Store L2 block and its transactions
	if err := s.AddL2Block(ctx, batchNumber, block, receipts, storeTxsEGPData, dbTx); err != nil {
		return err
	}

	log.Debugf("stored L2 block %d for batch %d, storing time %v", header.Number, batchNumber, time.Since(start))

	return nil
}

// PreProcessTransaction processes the transaction in order to calculate its zkCounters before adding it to the pool
func (s *State) PreProcessTransaction(ctx context.Context, tx *types.Transaction, dbTx pgx.Tx) (*ProcessBatchResponse, error) {
	sender, err := GetSender(*tx)
	if err != nil {
		return nil, err
	}

	response, err := s.internalProcessUnsignedTransaction(ctx, tx, sender, nil, false, dbTx)
	if err != nil {
		return response, err
	}

	return response, nil
}

// ProcessUnsignedTransaction processes the given unsigned transaction.
func (s *State) ProcessUnsignedTransaction(ctx context.Context, tx *types.Transaction, senderAddress common.Address, l2BlockNumber *uint64, noZKEVMCounters bool, dbTx pgx.Tx) (*runtime.ExecutionResult, error) {
	result := new(runtime.ExecutionResult)
	response, err := s.internalProcessUnsignedTransaction(ctx, tx, senderAddress, l2BlockNumber, noZKEVMCounters, dbTx)
	if err != nil {
		return nil, err
	}

	r := response.BlockResponses[0].TransactionResponses[0]
	result.ReturnValue = r.ReturnValue
	result.GasLeft = r.GasLeft
	result.GasUsed = r.GasUsed
	result.CreateAddress = r.CreateAddress
	result.StateRoot = r.StateRoot.Bytes()

	if errors.Is(r.RomError, runtime.ErrExecutionReverted) {
		result.Err = constructErrorFromRevert(r.RomError, r.ReturnValue)
	} else {
		result.Err = r.RomError
	}

	return result, nil
}

// internalProcessUnsignedTransaction processes the given unsigned transaction.
func (s *State) internalProcessUnsignedTransaction(ctx context.Context, tx *types.Transaction, senderAddress common.Address, l2BlockNumber *uint64, noZKEVMCounters bool, dbTx pgx.Tx) (*ProcessBatchResponse, error) {
	var l2Block *L2Block
	var err error
	if l2BlockNumber == nil {
		l2Block, err = s.GetLastL2Block(ctx, dbTx)
	} else {
		l2Block, err = s.GetL2BlockByNumber(ctx, *l2BlockNumber, dbTx)
	}
	if err != nil {
		return nil, err
	}

	batch, err := s.GetBatchByL2BlockNumber(ctx, l2Block.NumberU64(), dbTx)
	if err != nil {
		return nil, err
	}

	forkID := s.GetForkIDByBatchNumber(batch.BatchNumber)
	if forkID < FORKID_ETROG {
		return s.internalProcessUnsignedTransactionV1(ctx, tx, senderAddress, *batch, *l2Block, forkID, noZKEVMCounters, dbTx)
	} else {
		return s.internalProcessUnsignedTransactionV2(ctx, tx, senderAddress, *batch, *l2Block, forkID, noZKEVMCounters, dbTx)
	}
}

// internalProcessUnsignedTransactionV1 processes the given unsigned transaction.
// pre ETROG
func (s *State) internalProcessUnsignedTransactionV1(ctx context.Context, tx *types.Transaction, senderAddress common.Address, batch Batch, l2Block L2Block, forkID uint64, noZKEVMCounters bool, dbTx pgx.Tx) (*ProcessBatchResponse, error) {
	var attempts = 1

	if s.executorClient == nil {
		return nil, ErrExecutorNil
	}
	if s.tree == nil {
		return nil, ErrStateTreeNil
	}

	latestL2BlockNumber, err := s.GetLastL2BlockNumber(ctx, dbTx)
	if err != nil {
		return nil, err
	}

	timestamp := l2Block.Time()
	if l2Block.NumberU64() == latestL2BlockNumber {
		timestamp = uint64(time.Now().Unix())
	}

	loadedNonce, err := s.tree.GetNonce(ctx, senderAddress, batch.StateRoot.Bytes())
	if err != nil {
		return nil, err
	}
	nonce := loadedNonce.Uint64()

	batchL2Data, err := EncodeUnsignedTransaction(*tx, s.cfg.ChainID, &nonce, forkID)
	if err != nil {
		log.Errorf("error encoding unsigned transaction ", err)
		return nil, err
	}

	// Create Batch V1
	processBatchRequestV1 := &executor.ProcessBatchRequest{
		From:             senderAddress.String(),
		OldBatchNum:      batch.BatchNumber,
		OldStateRoot:     l2Block.Root().Bytes(),
		OldAccInputHash:  batch.AccInputHash.Bytes(),
		ForkId:           forkID,
		Coinbase:         batch.Coinbase.String(),
		BatchL2Data:      batchL2Data,
		ChainId:          s.cfg.ChainID,
		UpdateMerkleTree: cFalse,
		ContextId:        uuid.NewString(),

		// v1 fields
		GlobalExitRoot: batch.GlobalExitRoot.Bytes(),
		EthTimestamp:   timestamp,
	}
	if noZKEVMCounters {
		processBatchRequestV1.NoCounters = cTrue
	}
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.From]: %v", processBatchRequestV1.From)
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.OldBatchNum]: %v", processBatchRequestV1.OldBatchNum)
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.OldStateRoot]: %v", hex.EncodeToHex(processBatchRequestV1.OldStateRoot))
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.OldAccInputHash]: %v", hex.EncodeToHex(processBatchRequestV1.OldAccInputHash))
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.ForkId]: %v", processBatchRequestV1.ForkId)
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.Coinbase]: %v", processBatchRequestV1.Coinbase)
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.ChainId]: %v", processBatchRequestV1.ChainId)
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.UpdateMerkleTree]: %v", processBatchRequestV1.UpdateMerkleTree)
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.ContextId]: %v", processBatchRequestV1.ContextId)
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.GlobalExitRoot]: %v", hex.EncodeToHex(processBatchRequestV1.GlobalExitRoot))
	log.Debugf("internalProcessUnsignedTransactionV1[processBatchRequestV1.EthTimestamp]: %v", processBatchRequestV1.EthTimestamp)

	// Send Batch to the Executor
	processBatchResponse, err := s.executorClient.ProcessBatch(ctx, processBatchRequestV1)
	if err != nil {
		if status.Code(err) == codes.ResourceExhausted || (processBatchResponse != nil && processBatchResponse.Error == executor.ExecutorError_EXECUTOR_ERROR_DB_ERROR) {
			log.Errorf("error processing unsigned transaction ", err)
			for attempts < s.cfg.MaxResourceExhaustedAttempts {
				time.Sleep(s.cfg.WaitOnResourceExhaustion.Duration)
				log.Errorf("retrying to process unsigned transaction")
				processBatchResponse, err = s.executorClient.ProcessBatch(ctx, processBatchRequestV1)
				if status.Code(err) == codes.ResourceExhausted || (processBatchResponse != nil && processBatchResponse.Error == executor.ExecutorError_EXECUTOR_ERROR_DB_ERROR) {
					log.Errorf("error processing unsigned transaction ", err)
					attempts++
					continue
				}
				break
			}
		}

		if err != nil {
			if status.Code(err) == codes.ResourceExhausted || (processBatchResponse != nil && processBatchResponse.Error == executor.ExecutorError_EXECUTOR_ERROR_DB_ERROR) {
				log.Error("reporting error as time out")
				return nil, runtime.ErrGRPCResourceExhaustedAsTimeout
			}
			// Log the error
			event := &event.Event{
				ReceivedAt:  time.Now(),
				Source:      event.Source_Node,
				Level:       event.Level_Error,
				EventID:     event.EventID_ExecutorError,
				Description: fmt.Sprintf("error processing unsigned transaction %s: %v", tx.Hash(), err),
			}

			err2 := s.eventLog.LogEvent(context.Background(), event)
			if err2 != nil {
				log.Errorf("error logging event %v", err2)
			}
			log.Errorf("error processing unsigned transaction ", err)
			return nil, err
		}
	}

	if err == nil && processBatchResponse.Error != executor.ExecutorError_EXECUTOR_ERROR_NO_ERROR {
		err = executor.ExecutorErr(processBatchResponse.Error)
		s.eventLog.LogExecutorError(ctx, processBatchResponse.Error, processBatchRequestV1)
		return nil, err
	}

	response, err := s.convertToProcessBatchResponse(processBatchResponse)
	if err != nil {
		return nil, err
	}

	if processBatchResponse.Responses[0].Error != executor.RomError_ROM_ERROR_NO_ERROR {
		err := executor.RomErr(processBatchResponse.Responses[0].Error)
		if !isEVMRevertError(err) {
			return response, err
		}
	}

	return response, nil
}

// internalProcessUnsignedTransactionV2 processes the given unsigned transaction.
// post ETROG
func (s *State) internalProcessUnsignedTransactionV2(ctx context.Context, tx *types.Transaction, senderAddress common.Address, batch Batch, l2Block L2Block, forkID uint64, noZKEVMCounters bool, dbTx pgx.Tx) (*ProcessBatchResponse, error) {
	var attempts = 1

	if s.executorClient == nil {
		return nil, ErrExecutorNil
	}
	if s.tree == nil {
		return nil, ErrStateTreeNil
	}

	loadedNonce, err := s.tree.GetNonce(ctx, senderAddress, l2Block.Root().Bytes())
	if err != nil {
		return nil, err
	}
	nonce := loadedNonce.Uint64()

	deltaTimestamp := uint32(uint64(time.Now().Unix()) - l2Block.Time())
	transactions := s.BuildChangeL2Block(deltaTimestamp, uint32(0))

	batchL2Data, err := EncodeUnsignedTransaction(*tx, s.cfg.ChainID, &nonce, forkID)
	if err != nil {
		log.Errorf("error encoding unsigned transaction ", err)
		return nil, err
	}

	transactions = append(transactions, batchL2Data...)

	// Create a batch to be sent to the executor
	processBatchRequestV2 := &executor.ProcessBatchRequestV2{
		From:             senderAddress.String(),
		OldBatchNum:      batch.BatchNumber,
		OldStateRoot:     l2Block.Root().Bytes(),
		OldAccInputHash:  batch.AccInputHash.Bytes(),
		Coinbase:         batch.Coinbase.String(),
		ForkId:           forkID,
		BatchL2Data:      transactions,
		ChainId:          s.cfg.ChainID,
		UpdateMerkleTree: cFalse,
		ContextId:        uuid.NewString(),

		// v2 fields
		L1InfoRoot:             l2Block.BlockInfoRoot().Bytes(),
		TimestampLimit:         uint64(time.Now().Unix()),
		SkipFirstChangeL2Block: cFalse,
		SkipWriteBlockInfoRoot: cTrue,
	}
	if noZKEVMCounters {
		processBatchRequestV2.NoCounters = cTrue
	}

	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.From]: %v", processBatchRequestV2.From)
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.OldBatchNum]: %v", processBatchRequestV2.OldBatchNum)
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.OldStateRoot]: %v", hex.EncodeToHex(processBatchRequestV2.OldStateRoot))
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.OldAccInputHash]: %v", hex.EncodeToHex(processBatchRequestV2.OldAccInputHash))
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.Coinbase]: %v", processBatchRequestV2.Coinbase)
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.ForkId]: %v", processBatchRequestV2.ForkId)
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.ChainId]: %v", processBatchRequestV2.ChainId)
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.UpdateMerkleTree]: %v", processBatchRequestV2.UpdateMerkleTree)
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.ContextId]: %v", processBatchRequestV2.ContextId)

	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.L1InfoRoot]: %v", hex.EncodeToHex(processBatchRequestV2.L1InfoRoot))
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.TimestampLimit]: %v", processBatchRequestV2.TimestampLimit)
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.SkipFirstChangeL2Block]: %v", processBatchRequestV2.SkipFirstChangeL2Block)
	log.Debugf("internalProcessUnsignedTransactionV2[processBatchRequestV2.SkipWriteBlockInfoRoot]: %v", processBatchRequestV2.SkipWriteBlockInfoRoot)

	// Send Batch to the Executor
	processBatchResponseV2, err := s.executorClient.ProcessBatchV2(ctx, processBatchRequestV2)
	if err != nil {
		if status.Code(err) == codes.ResourceExhausted || (processBatchResponseV2 != nil && processBatchResponseV2.Error == executor.ExecutorError_EXECUTOR_ERROR_DB_ERROR) {
			log.Errorf("error processing unsigned transaction ", err)
			for attempts < s.cfg.MaxResourceExhaustedAttempts {
				time.Sleep(s.cfg.WaitOnResourceExhaustion.Duration)
				log.Errorf("retrying to process unsigned transaction")
				processBatchResponseV2, err = s.executorClient.ProcessBatchV2(ctx, processBatchRequestV2)
				if status.Code(err) == codes.ResourceExhausted || (processBatchResponseV2 != nil && processBatchResponseV2.Error == executor.ExecutorError_EXECUTOR_ERROR_DB_ERROR) {
					log.Errorf("error processing unsigned transaction ", err)
					attempts++
					continue
				}
				break
			}
		}

		if err != nil {
			if status.Code(err) == codes.ResourceExhausted || (processBatchResponseV2 != nil && processBatchResponseV2.Error == executor.ExecutorError_EXECUTOR_ERROR_DB_ERROR) {
				log.Error("reporting error as time out")
				return nil, runtime.ErrGRPCResourceExhaustedAsTimeout
			}
			// Log the error
			event := &event.Event{
				ReceivedAt:  time.Now(),
				Source:      event.Source_Node,
				Level:       event.Level_Error,
				EventID:     event.EventID_ExecutorError,
				Description: fmt.Sprintf("error processing unsigned transaction %s: %v", tx.Hash(), err),
			}

			err2 := s.eventLog.LogEvent(context.Background(), event)
			if err2 != nil {
				log.Errorf("error logging event %v", err2)
			}
			log.Errorf("error processing unsigned transaction ", err)
			return nil, err
		}
	}

	if err == nil && processBatchResponseV2.Error != executor.ExecutorError_EXECUTOR_ERROR_NO_ERROR {
		err = executor.ExecutorErr(processBatchResponseV2.Error)
		s.eventLog.LogExecutorErrorV2(ctx, processBatchResponseV2.Error, processBatchRequestV2)
		return nil, err
	}

	response, err := s.convertToProcessBatchResponseV2(processBatchResponseV2)
	if err != nil {
		return nil, err
	}

	if processBatchResponseV2.BlockResponses[0].Responses[0].Error != executor.RomError_ROM_ERROR_NO_ERROR {
		err := executor.RomErr(processBatchResponseV2.BlockResponses[0].Responses[0].Error)
		if !isEVMRevertError(err) {
			return response, err
		}
	}

	return response, nil
}

// isContractCreation checks if the tx is a contract creation
func (s *State) isContractCreation(tx *types.Transaction) bool {
	return tx.To() == nil && len(tx.Data()) > 0
}

// StoreTransaction is used by the sequencer and trusted state synchronizer to add process a transaction.
func (s *State) StoreTransaction(ctx context.Context, batchNumber uint64, processedTx *ProcessTransactionResponse, coinbase common.Address, timestamp uint64, egpLog *EffectiveGasPriceLog, dbTx pgx.Tx) (*L2Header, error) {
	if dbTx == nil {
		return nil, ErrDBTxNil
	}

	// if the transaction has an intrinsic invalid tx error it means
	// the transaction has not changed the state, so we don't store it
	if executor.IsIntrinsicError(executor.RomErrorCode(processedTx.RomError)) {
		return nil, nil
	}

	lastL2Block, err := s.GetLastL2Block(ctx, dbTx)
	if err != nil {
		return nil, err
	}

	header := NewL2Header(&types.Header{
		Number:     new(big.Int).SetUint64(lastL2Block.Number().Uint64() + 1),
		ParentHash: lastL2Block.Hash(),
		Coinbase:   coinbase,
		Root:       processedTx.StateRoot,
		GasUsed:    processedTx.GasUsed,
		GasLimit:   s.cfg.MaxCumulativeGasUsed,
		Time:       timestamp,
	})
	transactions := []*types.Transaction{&processedTx.Tx}

	receipt := GenerateReceipt(header.Number, processedTx)
	receipts := []*types.Receipt{receipt}

	// Create l2Block to be able to calculate its hash
	l2Block := NewL2Block(header, transactions, []*L2Header{}, receipts, &trie.StackTrie{})
	l2Block.ReceivedAt = time.Unix(int64(timestamp), 0)

	receipt.BlockHash = l2Block.Hash()

	storeTxsEGPData := []StoreTxEGPData{{EGPLog: egpLog, EffectivePercentage: uint8(processedTx.EffectivePercentage)}}

	// Store L2 block and its transaction
	if err := s.AddL2Block(ctx, batchNumber, l2Block, receipts, storeTxsEGPData, dbTx); err != nil {
		return nil, err
	}

	return l2Block.Header(), nil
}

// CheckSupersetBatchTransactions verifies that processedTransactions is a
// superset of existingTxs and that the existing txs have the same order,
// returns a non-nil error if that is not the case.
func CheckSupersetBatchTransactions(existingTxHashes []common.Hash, processedTxs []*ProcessTransactionResponse) error {
	if len(existingTxHashes) > len(processedTxs) {
		return ErrExistingTxGreaterThanProcessedTx
	}
	for i, existingTxHash := range existingTxHashes {
		if existingTxHash != processedTxs[i].TxHash {
			return ErrOutOfOrderProcessedTx
		}
	}
	return nil
}

// EstimateGas for a transaction
func (s *State) EstimateGas(transaction *types.Transaction, senderAddress common.Address, l2BlockNumber *uint64, dbTx pgx.Tx) (uint64, []byte, error) {
	const ethTransferGas = 21000

	ctx := context.Background()

	var l2Block *L2Block
	var err error
	if l2BlockNumber == nil {
		l2Block, err = s.GetLastL2Block(ctx, dbTx)
	} else {
		l2Block, err = s.GetL2BlockByNumber(ctx, *l2BlockNumber, dbTx)
	}
	if err != nil {
		return 0, nil, err
	}

	batch, err := s.GetBatchByL2BlockNumber(ctx, l2Block.NumberU64(), dbTx)
	if err != nil {
		return 0, nil, err
	}

	forkID := s.GetForkIDByBatchNumber(batch.BatchNumber)
	latestL2BlockNumber, err := s.GetLastL2BlockNumber(ctx, dbTx)
	if err != nil {
		return 0, nil, err
	}

	loadedNonce, err := s.tree.GetNonce(ctx, senderAddress, l2Block.Root().Bytes())
	if err != nil {
		return 0, nil, err
	}
	nonce := loadedNonce.Uint64()

	highEnd := s.cfg.MaxCumulativeGasUsed

	// if gas price is set, set the highEnd to the max amount
	// of the account afford
	isGasPriceSet := transaction.GasPrice().BitLen() != 0
	if isGasPriceSet {
		senderBalance, err := s.tree.GetBalance(ctx, senderAddress, l2Block.Root().Bytes())
		if errors.Is(err, ErrNotFound) {
			senderBalance = big.NewInt(0)
		} else if err != nil {
			return 0, nil, err
		}

		availableBalance := new(big.Int).Set(senderBalance)
		// check if the account has funds to pay the transfer value
		if transaction.Value() != nil {
			if transaction.Value().Cmp(availableBalance) > 0 {
				return 0, nil, ErrInsufficientFundsForTransfer
			}

			// deduct the value from the available balance
			availableBalance.Sub(availableBalance, transaction.Value())
		}

		// Check the gas allowance for this account, make sure high end is capped to it
		gasAllowance := new(big.Int).Div(availableBalance, transaction.GasPrice())
		if gasAllowance.IsUint64() && highEnd > gasAllowance.Uint64() {
			log.Debugf("Gas estimation high-end capped by allowance [%d]", gasAllowance.Uint64())
			highEnd = gasAllowance.Uint64()
		}
	}

	// if the tx gas is set and it is smaller than the highEnd,
	// limit the highEnd to the maximum allowed by the tx gas
	if transaction.Gas() != 0 && transaction.Gas() < highEnd {
		highEnd = transaction.Gas()
	}

	// set start values for lowEnd and highEnd:
	lowEnd, err := core.IntrinsicGas(transaction.Data(), transaction.AccessList(), s.isContractCreation(transaction), true, false, false)
	if err != nil {
		return 0, nil, err
	}

	// if the intrinsic gas is the same as the constant value for eth transfer
	// and the transaction has a receiver address
	if lowEnd == ethTransferGas && transaction.To() != nil {
		receiver := *transaction.To()
		// check if the receiver address is not a smart contract
		code, err := s.tree.GetCode(ctx, receiver, l2Block.Root().Bytes())
		if err != nil {
			log.Warnf("error while getting code for address %v: %v", receiver.String(), err)
		} else if len(code) == 0 {
			// in case it is just an account, we can avoid the execution and return
			// the transfer constant amount
			return lowEnd, nil, nil
		}
	}

	// testTransaction runs the transaction with the specified gas value.
	// it returns a status indicating if the transaction has failed, if it
	// was reverted and the accompanying error
	txExecutions := []time.Duration{}
	var totalExecutionTime time.Duration

	// Check if the highEnd is a good value to make the transaction pass, if it fails we
	// can return immediately.
	log.Debugf("Estimate gas. Trying to execute TX with %v gas", highEnd)
	var failed, reverted bool
	var gasUsed uint64
	var returnValue []byte
	if forkID < FORKID_ETROG {
		failed, reverted, gasUsed, returnValue, err = s.internalTestGasEstimationTransactionV1(ctx, batch, l2Block, latestL2BlockNumber, transaction, forkID, senderAddress, highEnd, nonce, false)
	} else {
		failed, reverted, gasUsed, returnValue, err = s.internalTestGasEstimationTransactionV2(ctx, batch, l2Block, latestL2BlockNumber, transaction, forkID, senderAddress, highEnd, nonce, false)
	}

	if failed {
		if reverted {
			return 0, returnValue, err
		}

		// The transaction shouldn't fail, for whatever reason, at highEnd
		return 0, nil, fmt.Errorf(
			"gas required exceeds allowance (%d)",
			highEnd,
		)
	}

	// sets
	if lowEnd < gasUsed {
		lowEnd = gasUsed
	}

	// Start the binary search for the lowest possible gas price
	for (lowEnd < highEnd) && (highEnd-lowEnd) > 4096 {
		txExecutionStart := time.Now()
		mid := (lowEnd + highEnd) / 2 // nolint:gomnd
		if mid > lowEnd*2 {
			// Most txs don't need much higher gas limit than their gas used, and most txs don't
			// require near the full block limit of gas, so the selection of where to bisect the
			// range here is skewed to favor the low side.
			mid = lowEnd * 2 // nolint:gomnd
		}

		log.Debugf("Estimate gas. Trying to execute TX with %v gas", mid)
		if forkID < FORKID_ETROG {
			failed, reverted, _, _, err = s.internalTestGasEstimationTransactionV1(ctx, batch, l2Block, latestL2BlockNumber, transaction, forkID, senderAddress, mid, nonce, true)
		} else {
			failed, reverted, _, _, err = s.internalTestGasEstimationTransactionV2(ctx, batch, l2Block, latestL2BlockNumber, transaction, forkID, senderAddress, mid, nonce, true)
		}
		executionTime := time.Since(txExecutionStart)
		totalExecutionTime += executionTime
		txExecutions = append(txExecutions, executionTime)
		if err != nil && !reverted {
			// Reverts are ignored in the binary search, but are checked later on
			// during the execution for the optimal gas limit found
			return 0, nil, err
		}

		if failed {
			// If the transaction failed => increase the gas
			lowEnd = mid + 1
		} else {
			// If the transaction didn't fail => make this ok value the high end
			highEnd = mid
		}
	}

	executions := int64(len(txExecutions))
	if executions > 0 {
		log.Debugf("EstimateGas executed TX %v %d times in %d milliseconds", transaction.Hash(), executions, totalExecutionTime.Milliseconds())
	} else {
		log.Debug("Estimate gas. Tx not executed")
	}
	return highEnd, nil, nil
}

// internalTestGasEstimationTransactionV1 is used by the EstimateGas to test the tx execution
// during the binary search process to define the gas estimation of a given tx for l2 blocks
// before ETROG
func (s *State) internalTestGasEstimationTransactionV1(ctx context.Context, batch *Batch, l2Block *L2Block, latestL2BlockNumber uint64,
	transaction *types.Transaction, forkID uint64, senderAddress common.Address,
	gas uint64, nonce uint64, shouldOmitErr bool) (failed, reverted bool, gasUsed uint64, returnValue []byte, err error) {
	timestamp := l2Block.Time()
	if l2Block.NumberU64() == latestL2BlockNumber {
		timestamp = uint64(time.Now().Unix())
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       transaction.To(),
		Value:    transaction.Value(),
		Gas:      gas,
		GasPrice: transaction.GasPrice(),
		Data:     transaction.Data(),
	})

	batchL2Data, err := EncodeUnsignedTransaction(*tx, s.cfg.ChainID, &nonce, forkID)
	if err != nil {
		log.Errorf("error encoding unsigned transaction ", err)
		return false, false, gasUsed, nil, err
	}

	// Create a batch to be sent to the executor
	processBatchRequestV1 := &executor.ProcessBatchRequest{
		From:             senderAddress.String(),
		OldBatchNum:      batch.BatchNumber,
		OldStateRoot:     l2Block.Root().Bytes(),
		OldAccInputHash:  batch.AccInputHash.Bytes(),
		ForkId:           forkID,
		Coinbase:         batch.Coinbase.String(),
		BatchL2Data:      batchL2Data,
		ChainId:          s.cfg.ChainID,
		UpdateMerkleTree: cFalse,
		ContextId:        uuid.NewString(),

		// v1 fields
		GlobalExitRoot: batch.GlobalExitRoot.Bytes(),
		EthTimestamp:   timestamp,
	}

	log.Debugf("EstimateGas[processBatchRequestV1.From]: %v", processBatchRequestV1.From)
	log.Debugf("EstimateGas[processBatchRequestV1.From]: %v", processBatchRequestV1.From)
	log.Debugf("EstimateGas[processBatchRequestV1.OldBatchNum]: %v", processBatchRequestV1.OldBatchNum)
	log.Debugf("EstimateGas[processBatchRequestV1.OldStateRoot]: %v", hex.EncodeToHex(processBatchRequestV1.OldStateRoot))
	log.Debugf("EstimateGas[processBatchRequestV1.OldAccInputHash]: %v", hex.EncodeToHex(processBatchRequestV1.OldAccInputHash))
	log.Debugf("EstimateGas[processBatchRequestV1.ForkId]: %v", processBatchRequestV1.ForkId)
	log.Debugf("EstimateGas[processBatchRequestV1.Coinbase]: %v", processBatchRequestV1.Coinbase)
	log.Debugf("EstimateGas[processBatchRequestV1.ChainId]: %v", processBatchRequestV1.ChainId)
	log.Debugf("EstimateGas[processBatchRequestV1.UpdateMerkleTree]: %v", processBatchRequestV1.UpdateMerkleTree)
	log.Debugf("EstimateGas[processBatchRequestV1.ContextId]: %v", processBatchRequestV1.ContextId)
	log.Debugf("EstimateGas[processBatchRequestV1.GlobalExitRoot]: %v", hex.EncodeToHex(processBatchRequestV1.GlobalExitRoot))
	log.Debugf("EstimateGas[processBatchRequestV1.EthTimestamp]: %v", processBatchRequestV1.EthTimestamp)

	txExecutionOnExecutorTime := time.Now()
	processBatchResponse, err := s.executorClient.ProcessBatch(ctx, processBatchRequestV1)
	log.Debugf("executor time: %vms", time.Since(txExecutionOnExecutorTime).Milliseconds())
	if err != nil {
		log.Errorf("error estimating gas: %v", err)
		return false, false, gasUsed, nil, err
	}
	if processBatchResponse.Error != executor.ExecutorError_EXECUTOR_ERROR_NO_ERROR {
		err = executor.ExecutorErr(processBatchResponse.Error)
		s.eventLog.LogExecutorError(ctx, processBatchResponse.Error, processBatchRequestV1)
		return false, false, gasUsed, nil, err
	}
	gasUsed = processBatchResponse.Responses[0].GasUsed

	txResponse := processBatchResponse.Responses[0]
	// Check if an out of gas error happened during EVM execution
	if txResponse.Error != executor.RomError_ROM_ERROR_NO_ERROR {
		err := executor.RomErr(txResponse.Error)

		if (isGasEVMError(err) || isGasApplyError(err)) && shouldOmitErr {
			// Specifying the transaction failed, but not providing an error
			// is an indication that a valid error occurred due to low gas,
			// which will increase the lower bound for the search
			return true, false, gasUsed, nil, nil
		}

		if isEVMRevertError(err) {
			// The EVM reverted during execution, attempt to extract the
			// error message and return it
			returnValue := txResponse.ReturnValue
			return true, true, gasUsed, returnValue, constructErrorFromRevert(err, returnValue)
		}

		return true, false, gasUsed, nil, err
	}

	return false, false, gasUsed, nil, nil
}

// internalTestGasEstimationTransactionV2 is used by the EstimateGas to test the tx execution
// during the binary search process to define the gas estimation of a given tx for l2 blocks
// after ETROG
func (s *State) internalTestGasEstimationTransactionV2(ctx context.Context, batch *Batch, l2Block *L2Block, latestL2BlockNumber uint64,
	transaction *types.Transaction, forkID uint64, senderAddress common.Address,
	gas uint64, nonce uint64, shouldOmitErr bool) (failed, reverted bool, gasUsed uint64, returnValue []byte, err error) {
	deltaTimestamp := uint32(uint64(time.Now().Unix()) - l2Block.Time())
	transactions := s.BuildChangeL2Block(deltaTimestamp, uint32(0))

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       transaction.To(),
		Value:    transaction.Value(),
		Gas:      gas,
		GasPrice: transaction.GasPrice(),
		Data:     transaction.Data(),
	})

	batchL2Data, err := EncodeUnsignedTransaction(*tx, s.cfg.ChainID, &nonce, forkID)
	if err != nil {
		log.Errorf("error encoding unsigned transaction ", err)
		return false, false, gasUsed, nil, err
	}

	transactions = append(transactions, batchL2Data...)

	// Create a batch to be sent to the executor
	processBatchRequestV2 := &executor.ProcessBatchRequestV2{
		From:             senderAddress.String(),
		OldBatchNum:      batch.BatchNumber,
		OldStateRoot:     l2Block.Root().Bytes(),
		OldAccInputHash:  batch.AccInputHash.Bytes(),
		Coinbase:         batch.Coinbase.String(),
		ForkId:           forkID,
		BatchL2Data:      transactions,
		ChainId:          s.cfg.ChainID,
		UpdateMerkleTree: cFalse,
		ContextId:        uuid.NewString(),

		// v2 fields
		L1InfoRoot:             l2Block.BlockInfoRoot().Bytes(),
		TimestampLimit:         uint64(time.Now().Unix()),
		SkipFirstChangeL2Block: cTrue,
		SkipWriteBlockInfoRoot: cTrue,
	}

	log.Debugf("EstimateGas[processBatchRequestV2.From]: %v", processBatchRequestV2.From)
	log.Debugf("EstimateGas[processBatchRequestV2.OldBatchNum]: %v", processBatchRequestV2.OldBatchNum)
	log.Debugf("EstimateGas[processBatchRequestV2.OldStateRoot]: %v", hex.EncodeToHex(processBatchRequestV2.OldStateRoot))
	log.Debugf("EstimateGas[processBatchRequestV2.OldAccInputHash]: %v", hex.EncodeToHex(processBatchRequestV2.OldAccInputHash))
	log.Debugf("EstimateGas[processBatchRequestV2.Coinbase]: %v", processBatchRequestV2.Coinbase)
	log.Debugf("EstimateGas[processBatchRequestV2.ForkId]: %v", processBatchRequestV2.ForkId)
	log.Debugf("EstimateGas[processBatchRequestV2.ChainId]: %v", processBatchRequestV2.ChainId)
	log.Debugf("EstimateGas[processBatchRequestV2.UpdateMerkleTree]: %v", processBatchRequestV2.UpdateMerkleTree)
	log.Debugf("EstimateGas[processBatchRequestV2.ContextId]: %v", processBatchRequestV2.ContextId)

	log.Debugf("EstimateGas[processBatchRequestV2.L1InfoRoot]: %v", hex.EncodeToHex(processBatchRequestV2.L1InfoRoot))
	log.Debugf("EstimateGas[processBatchRequestV2.TimestampLimit]: %v", processBatchRequestV2.TimestampLimit)
	log.Debugf("EstimateGas[processBatchRequestV2.SkipFirstChangeL2Block]: %v", processBatchRequestV2.SkipFirstChangeL2Block)
	log.Debugf("EstimateGas[processBatchRequestV2.SkipWriteBlockInfoRoot]: %v", processBatchRequestV2.SkipWriteBlockInfoRoot)

	txExecutionOnExecutorTime := time.Now()
	processBatchResponse, err := s.executorClient.ProcessBatchV2(ctx, processBatchRequestV2)
	log.Debugf("executor time: %vms", time.Since(txExecutionOnExecutorTime).Milliseconds())
	if err != nil {
		log.Errorf("error estimating gas: %v", err)
		return false, false, gasUsed, nil, err
	}
	if processBatchResponse.Error != executor.ExecutorError_EXECUTOR_ERROR_NO_ERROR {
		err = executor.ExecutorErr(processBatchResponse.Error)
		s.eventLog.LogExecutorErrorV2(ctx, processBatchResponse.Error, processBatchRequestV2)
		return false, false, gasUsed, nil, err
	}
	gasUsed = processBatchResponse.BlockResponses[0].GasUsed

	txResponse := processBatchResponse.BlockResponses[0].Responses[0]
	// Check if an out of gas error happened during EVM execution
	if txResponse.Error != executor.RomError_ROM_ERROR_NO_ERROR {
		err := executor.RomErr(txResponse.Error)

		if (isGasEVMError(err) || isGasApplyError(err)) && shouldOmitErr {
			// Specifying the transaction failed, but not providing an error
			// is an indication that a valid error occurred due to low gas,
			// which will increase the lower bound for the search
			return true, false, gasUsed, nil, nil
		}

		if isEVMRevertError(err) {
			// The EVM reverted during execution, attempt to extract the
			// error message and return it
			returnValue := txResponse.ReturnValue
			return true, true, gasUsed, returnValue, constructErrorFromRevert(err, returnValue)
		}

		return true, false, gasUsed, nil, err
	}

	return false, false, gasUsed, nil, nil
}

// Checks if executor level valid gas errors occurred
func isGasApplyError(err error) bool {
	return errors.Is(err, ErrNotEnoughIntrinsicGas)
}

// Checks if EVM level valid gas errors occurred
func isGasEVMError(err error) bool {
	return errors.Is(err, runtime.ErrOutOfGas)
}

// Checks if the EVM reverted during execution
func isEVMRevertError(err error) bool {
	return errors.Is(err, runtime.ErrExecutionReverted)
}
