package types

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v4"
)

// ArgUint64 helps to marshal uint64 values provided in the RPC requests
type ArgUint64 uint64

// MarshalText marshals into text
func (b ArgUint64) MarshalText() ([]byte, error) {
	buf := make([]byte, 2) //nolint:gomnd
	copy(buf, `0x`)
	buf = strconv.AppendUint(buf, uint64(b), hex.Base)
	return buf, nil
}

// UnmarshalText unmarshals from text
func (b *ArgUint64) UnmarshalText(input []byte) error {
	str := strings.TrimPrefix(string(input), "0x")
	num, err := strconv.ParseUint(str, hex.Base, hex.BitSize64)
	if err != nil {
		return err
	}
	*b = ArgUint64(num)
	return nil
}

// Hex returns a hexadecimal representation
func (b ArgUint64) Hex() string {
	bb, _ := b.MarshalText()
	return string(bb)
}

// ArgUint64Ptr returns the pointer of the provided ArgUint64
func ArgUint64Ptr(a ArgUint64) *ArgUint64 {
	return &a
}

// ArgBytes helps to marshal byte array values provided in the RPC requests
type ArgBytes []byte

// MarshalText marshals into text
func (b ArgBytes) MarshalText() ([]byte, error) {
	return encodeToHex(b), nil
}

// UnmarshalText unmarshals from text
func (b *ArgBytes) UnmarshalText(input []byte) error {
	hh, err := decodeToHex(input)
	if err != nil {
		return nil
	}
	aux := make([]byte, len(hh))
	copy(aux[:], hh[:])
	*b = aux
	return nil
}

// Hex returns a hexadecimal representation
func (b ArgBytes) Hex() string {
	bb, _ := b.MarshalText()
	return string(bb)
}

// ArgBytesPtr helps to marshal byte array values provided in the RPC requests
func ArgBytesPtr(b []byte) *ArgBytes {
	bb := ArgBytes(b)

	return &bb
}

// ArgBig helps to marshal big number values provided in the RPC requests
type ArgBig big.Int

// UnmarshalText unmarshals an instance of ArgBig into an array of bytes
func (a *ArgBig) UnmarshalText(input []byte) error {
	buf, err := decodeToHex(input)
	if err != nil {
		return err
	}

	b := new(big.Int)
	b.SetBytes(buf)
	*a = ArgBig(*b)

	return nil
}

// MarshalText marshals an array of bytes into an instance of ArgBig
func (a ArgBig) MarshalText() ([]byte, error) {
	b := (*big.Int)(&a)

	return []byte("0x" + b.Text(hex.Base)), nil
}

// Hex returns a hexadecimal representation
func (b ArgBig) Hex() string {
	bb, _ := b.MarshalText()
	return string(bb)
}

func decodeToHex(b []byte) ([]byte, error) {
	str := string(b)
	str = strings.TrimPrefix(str, "0x")
	if len(str)%2 != 0 {
		str = "0" + str
	}
	return hex.DecodeString(str)
}

func encodeToHex(b []byte) []byte {
	str := hex.EncodeToString(b)
	if len(str)%2 != 0 {
		str = "0" + str
	}
	return []byte("0x" + str)
}

// ArgHash represents a common.Hash that accepts strings
// shorter than 64 bytes, like 0x00
type ArgHash common.Hash

// UnmarshalText unmarshals from text
func (arg *ArgHash) UnmarshalText(input []byte) error {
	if !hex.IsValid(string(input)) {
		return fmt.Errorf("invalid hash, it needs to be a hexadecimal value")
	}

	str := strings.TrimPrefix(string(input), "0x")
	*arg = ArgHash(common.HexToHash(str))
	return nil
}

// Hash returns an instance of common.Hash
func (arg *ArgHash) Hash() common.Hash {
	result := common.Hash{}
	if arg != nil {
		result = common.Hash(*arg)
	}
	return result
}

// ArgAddress represents a common.Address that accepts strings
// shorter than 32 bytes, like 0x00
type ArgAddress common.Address

// UnmarshalText unmarshals from text
func (b *ArgAddress) UnmarshalText(input []byte) error {
	if !hex.IsValid(string(input)) {
		return fmt.Errorf("invalid address, it needs to be a hexadecimal value")
	}

	str := strings.TrimPrefix(string(input), "0x")
	*b = ArgAddress(common.HexToAddress(str))
	return nil
}

// Address returns an instance of common.Address
func (arg *ArgAddress) Address() common.Address {
	result := common.Address{}
	if arg != nil {
		result = common.Address(*arg)
	}
	return result
}

// TxArgs is the transaction argument for the rpc endpoints
type TxArgs struct {
	From     *common.Address
	To       *common.Address
	Gas      *ArgUint64
	GasPrice *ArgBytes
	Value    *ArgBytes
	Data     *ArgBytes
	Input    *ArgBytes
	Nonce    *ArgUint64
}

// ToTransaction transforms txnArgs into a Transaction
func (args *TxArgs) ToTransaction(ctx context.Context, st StateInterface, maxCumulativeGasUsed uint64, root common.Hash, defaultSenderAddress common.Address, dbTx pgx.Tx) (common.Address, *types.Transaction, error) {
	sender := defaultSenderAddress
	nonce := uint64(0)
	if args.From != nil && *args.From != state.ZeroAddress {
		sender = *args.From
		n, err := st.GetNonce(ctx, sender, root)
		if err != nil {
			return common.Address{}, nil, err
		}
		nonce = n
	}

	value := big.NewInt(0)
	if args.Value != nil {
		value.SetBytes(*args.Value)
	}

	gasPrice := big.NewInt(0)
	if args.GasPrice != nil {
		gasPrice.SetBytes(*args.GasPrice)
	}

	var data []byte
	if args.Data != nil {
		data = *args.Data
	} else if args.Input != nil {
		data = *args.Input
	} else if args.To == nil {
		return common.Address{}, nil, fmt.Errorf("contract creation without data provided")
	}

	gas := maxCumulativeGasUsed
	if args.Gas != nil && uint64(*args.Gas) > 0 && uint64(*args.Gas) < maxCumulativeGasUsed {
		gas = uint64(*args.Gas)
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       args.To,
		Value:    value,
		Gas:      gas,
		GasPrice: gasPrice,
		Data:     data,
	})

	return sender, tx, nil
}

// Block structure
type Block struct {
	ParentHash      common.Hash         `json:"parentHash"`
	Sha3Uncles      common.Hash         `json:"sha3Uncles"`
	Miner           *common.Address     `json:"miner"`
	StateRoot       common.Hash         `json:"stateRoot"`
	TxRoot          common.Hash         `json:"transactionsRoot"`
	ReceiptsRoot    common.Hash         `json:"receiptsRoot"`
	LogsBloom       types.Bloom         `json:"logsBloom"`
	Difficulty      ArgUint64           `json:"difficulty"`
	TotalDifficulty *ArgUint64          `json:"totalDifficulty"`
	Size            ArgUint64           `json:"size"`
	Number          ArgUint64           `json:"number"`
	GasLimit        ArgUint64           `json:"gasLimit"`
	GasUsed         ArgUint64           `json:"gasUsed"`
	Timestamp       ArgUint64           `json:"timestamp"`
	ExtraData       ArgBytes            `json:"extraData"`
	MixHash         common.Hash         `json:"mixHash"`
	Nonce           *ArgBytes           `json:"nonce"`
	Hash            *common.Hash        `json:"hash"`
	Transactions    []TransactionOrHash `json:"transactions"`
	Uncles          []common.Hash       `json:"uncles"`
	GlobalExitRoot  common.Hash         `json:"globalExitRoot"`
	BlockInfoRoot   common.Hash         `json:"blockInfoRoot"`
}

// NewBlock creates a Block instance
func NewBlock(hash *common.Hash, b *state.L2Block, receipts []types.Receipt, fullTx, includeReceipts bool) (*Block, error) {
	h := b.Header()

	var miner *common.Address
	if h.Coinbase.String() != state.ZeroAddress.String() {
		cb := h.Coinbase
		miner = &cb
	}

	var nonce *ArgBytes
	if h.Nonce.Uint64() > 0 {
		nBig := big.NewInt(0).SetUint64(h.Nonce.Uint64())
		nBytes := common.LeftPadBytes(nBig.Bytes(), 8) //nolint:gomnd
		n := ArgBytes(nBytes)
		nonce = &n
	}

	difficulty := ArgUint64(0)
	var totalDifficulty *ArgUint64
	if h.Difficulty != nil && h.Difficulty.Uint64() > 0 {
		difficulty = ArgUint64(h.Difficulty.Uint64())
		totalDifficulty = &difficulty
	}

	res := &Block{
		ParentHash:      h.ParentHash,
		Sha3Uncles:      h.UncleHash,
		Miner:           miner,
		StateRoot:       h.Root,
		TxRoot:          h.TxHash,
		ReceiptsRoot:    h.ReceiptHash,
		LogsBloom:       h.Bloom,
		Difficulty:      difficulty,
		TotalDifficulty: totalDifficulty,
		Size:            ArgUint64(b.Size()),
		Number:          ArgUint64(b.Number().Uint64()),
		GasLimit:        ArgUint64(h.GasLimit),
		GasUsed:         ArgUint64(h.GasUsed),
		Timestamp:       ArgUint64(h.Time),
		ExtraData:       ArgBytes(h.Extra),
		MixHash:         h.MixDigest,
		Nonce:           nonce,
		Hash:            hash,
		Transactions:    []TransactionOrHash{},
		Uncles:          []common.Hash{},
		GlobalExitRoot:  h.GlobalExitRoot,
		BlockInfoRoot:   h.BlockInfoRoot,
	}

	receiptsMap := make(map[common.Hash]types.Receipt, len(receipts))
	for _, receipt := range receipts {
		receiptsMap[receipt.TxHash] = receipt
	}

	for _, tx := range b.Transactions() {
		if fullTx {
			var receiptPtr *types.Receipt
			if receipt, found := receiptsMap[tx.Hash()]; found {
				receiptPtr = &receipt
			}

			rpcTx, err := NewTransaction(*tx, receiptPtr, includeReceipts)
			if err != nil {
				return nil, err
			}
			res.Transactions = append(
				res.Transactions,
				TransactionOrHash{Tx: rpcTx},
			)
		} else {
			h := tx.Hash()
			res.Transactions = append(
				res.Transactions,
				TransactionOrHash{Hash: &h},
			)
		}
	}

	for _, uncle := range b.Uncles() {
		res.Uncles = append(res.Uncles, uncle.Hash())
	}

	return res, nil
}

// Batch structure
type Batch struct {
	Number              ArgUint64           `json:"number"`
	ForcedBatchNumber   *ArgUint64          `json:"forcedBatchNumber,omitempty"`
	Coinbase            common.Address      `json:"coinbase"`
	StateRoot           common.Hash         `json:"stateRoot"`
	GlobalExitRoot      common.Hash         `json:"globalExitRoot"`
	MainnetExitRoot     common.Hash         `json:"mainnetExitRoot"`
	RollupExitRoot      common.Hash         `json:"rollupExitRoot"`
	LocalExitRoot       common.Hash         `json:"localExitRoot"`
	AccInputHash        common.Hash         `json:"accInputHash"`
	Timestamp           ArgUint64           `json:"timestamp"`
	SendSequencesTxHash *common.Hash        `json:"sendSequencesTxHash"`
	VerifyBatchTxHash   *common.Hash        `json:"verifyBatchTxHash"`
	Closed              bool                `json:"closed"`
	Blocks              []BlockOrHash       `json:"blocks"`
	Transactions        []TransactionOrHash `json:"transactions"`
	BatchL2Data         ArgBytes            `json:"batchL2Data"`
}

// NewBatch creates a Batch instance
func NewBatch(batch *state.Batch, virtualBatch *state.VirtualBatch, verifiedBatch *state.VerifiedBatch, blocks []state.L2Block, receipts []types.Receipt, fullTx, includeReceipts bool, ger *state.GlobalExitRoot) (*Batch, error) {
	batchL2Data := batch.BatchL2Data
	closed := !batch.WIP
	res := &Batch{
		Number:          ArgUint64(batch.BatchNumber),
		GlobalExitRoot:  batch.GlobalExitRoot,
		MainnetExitRoot: ger.MainnetExitRoot,
		RollupExitRoot:  ger.RollupExitRoot,
		AccInputHash:    batch.AccInputHash,
		Timestamp:       ArgUint64(batch.Timestamp.Unix()),
		StateRoot:       batch.StateRoot,
		Coinbase:        batch.Coinbase,
		LocalExitRoot:   batch.LocalExitRoot,
		BatchL2Data:     ArgBytes(batchL2Data),
		Closed:          closed,
	}

	if batch.ForcedBatchNum != nil {
		fb := ArgUint64(*batch.ForcedBatchNum)
		res.ForcedBatchNumber = &fb
	}

	if virtualBatch != nil {
		res.SendSequencesTxHash = &virtualBatch.TxHash
	}

	if verifiedBatch != nil {
		res.VerifyBatchTxHash = &verifiedBatch.TxHash
	}

	receiptsMap := make(map[common.Hash]types.Receipt, len(receipts))
	for _, receipt := range receipts {
		receiptsMap[receipt.TxHash] = receipt
	}

	for _, tx := range batch.Transactions {
		if fullTx {
			var receiptPtr *types.Receipt
			if receipt, found := receiptsMap[tx.Hash()]; found {
				receiptPtr = &receipt
			}
			rpcTx, err := NewTransaction(tx, receiptPtr, includeReceipts)
			if err != nil {
				return nil, err
			}
			res.Transactions = append(res.Transactions, TransactionOrHash{Tx: rpcTx})
		} else {
			h := tx.Hash()
			res.Transactions = append(res.Transactions, TransactionOrHash{Hash: &h})
		}
	}

	for _, b := range blocks {
		b := b
		if fullTx {
			block, err := NewBlock(state.HashPtr(b.Hash()), &b, nil, false, false)
			if err != nil {
				return nil, err
			}
			res.Blocks = append(res.Blocks, BlockOrHash{Block: block})
		} else {
			h := b.Hash()
			res.Blocks = append(res.Blocks, BlockOrHash{Hash: &h})
		}
	}

	return res, nil
}

// TransactionOrHash for union type of transaction and types.Hash
type TransactionOrHash struct {
	Hash *common.Hash
	Tx   *Transaction
}

// MarshalJSON marshals into json
func (th TransactionOrHash) MarshalJSON() ([]byte, error) {
	if th.Hash != nil {
		return json.Marshal(th.Hash)
	}
	return json.Marshal(th.Tx)
}

// UnmarshalJSON unmarshals from json
func (th *TransactionOrHash) UnmarshalJSON(input []byte) error {
	v := string(input)
	if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "\"0x") {
		var h common.Hash
		err := json.Unmarshal(input, &h)
		if err != nil {
			return err
		}
		*th = TransactionOrHash{Hash: &h}
		return nil
	}

	var t Transaction
	err := json.Unmarshal(input, &t)
	if err != nil {
		return err
	}
	*th = TransactionOrHash{Tx: &t}
	return nil
}

// BlockOrHash for union type of block and types.Hash
type BlockOrHash struct {
	Hash  *common.Hash
	Block *Block
}

// MarshalJSON marshals into json
func (bh BlockOrHash) MarshalJSON() ([]byte, error) {
	if bh.Hash != nil {
		return json.Marshal(bh.Hash)
	}
	return json.Marshal(bh.Block)
}

// UnmarshalJSON unmarshals from json
func (bh *BlockOrHash) UnmarshalJSON(input []byte) error {
	v := string(input)
	if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "\"0x") {
		var h common.Hash
		err := json.Unmarshal(input, &h)
		if err != nil {
			return err
		}
		*bh = BlockOrHash{Hash: &h}
		return nil
	}

	var b Block
	err := json.Unmarshal(input, &b)
	if err != nil {
		return err
	}
	*bh = BlockOrHash{Block: &b}
	return nil
}

// Transaction structure
type Transaction struct {
	Nonce       ArgUint64       `json:"nonce"`
	GasPrice    ArgBig          `json:"gasPrice"`
	Gas         ArgUint64       `json:"gas"`
	To          *common.Address `json:"to"`
	Value       ArgBig          `json:"value"`
	Input       ArgBytes        `json:"input"`
	V           ArgBig          `json:"v"`
	R           ArgBig          `json:"r"`
	S           ArgBig          `json:"s"`
	Hash        common.Hash     `json:"hash"`
	From        common.Address  `json:"from"`
	BlockHash   *common.Hash    `json:"blockHash"`
	BlockNumber *ArgUint64      `json:"blockNumber"`
	TxIndex     *ArgUint64      `json:"transactionIndex"`
	ChainID     ArgBig          `json:"chainId"`
	Type        ArgUint64       `json:"type"`
	Receipt     *Receipt        `json:"receipt,omitempty"`
}

// CoreTx returns a geth core type Transaction
func (t Transaction) CoreTx() *types.Transaction {
	return types.NewTx(&types.LegacyTx{
		Nonce:    uint64(t.Nonce),
		GasPrice: (*big.Int)(&t.GasPrice),
		Gas:      uint64(t.Gas),
		To:       t.To,
		Value:    (*big.Int)(&t.Value),
		Data:     t.Input,
		V:        (*big.Int)(&t.V),
		R:        (*big.Int)(&t.R),
		S:        (*big.Int)(&t.S),
	})
}

// NewTransaction creates a transaction instance
func NewTransaction(
	tx types.Transaction,
	receipt *types.Receipt,
	includeReceipt bool,
) (*Transaction, error) {
	v, r, s := tx.RawSignatureValues()

	from, _ := state.GetSender(tx)

	res := &Transaction{
		Nonce:    ArgUint64(tx.Nonce()),
		GasPrice: ArgBig(*tx.GasPrice()),
		Gas:      ArgUint64(tx.Gas()),
		To:       tx.To(),
		Value:    ArgBig(*tx.Value()),
		Input:    tx.Data(),
		V:        ArgBig(*v),
		R:        ArgBig(*r),
		S:        ArgBig(*s),
		Hash:     tx.Hash(),
		From:     from,
		ChainID:  ArgBig(*tx.ChainId()),
		Type:     ArgUint64(tx.Type()),
	}

	if receipt != nil {
		bn := ArgUint64(receipt.BlockNumber.Uint64())
		res.BlockNumber = &bn
		res.BlockHash = &receipt.BlockHash
		ti := ArgUint64(receipt.TransactionIndex)
		res.TxIndex = &ti
		rpcReceipt, err := NewReceipt(tx, receipt)
		if err != nil {
			return nil, err
		}
		if includeReceipt {
			res.Receipt = &rpcReceipt
		}
	}

	return res, nil
}

// Receipt structure
type Receipt struct {
	Root              common.Hash     `json:"root"`
	CumulativeGasUsed ArgUint64       `json:"cumulativeGasUsed"`
	LogsBloom         types.Bloom     `json:"logsBloom"`
	Logs              []*types.Log    `json:"logs"`
	Status            ArgUint64       `json:"status"`
	TxHash            common.Hash     `json:"transactionHash"`
	TxIndex           ArgUint64       `json:"transactionIndex"`
	BlockHash         common.Hash     `json:"blockHash"`
	BlockNumber       ArgUint64       `json:"blockNumber"`
	GasUsed           ArgUint64       `json:"gasUsed"`
	FromAddr          common.Address  `json:"from"`
	ToAddr            *common.Address `json:"to"`
	ContractAddress   *common.Address `json:"contractAddress"`
	Type              ArgUint64       `json:"type"`
	EffectiveGasPrice *ArgBig         `json:"effectiveGasPrice,omitempty"`
}

// NewReceipt creates a new Receipt instance
func NewReceipt(tx types.Transaction, r *types.Receipt) (Receipt, error) {
	to := tx.To()
	logs := r.Logs
	if logs == nil {
		logs = []*types.Log{}
	}

	var contractAddress *common.Address
	if r.ContractAddress != state.ZeroAddress {
		ca := r.ContractAddress
		contractAddress = &ca
	}

	blockNumber := ArgUint64(0)
	if r.BlockNumber != nil {
		blockNumber = ArgUint64(r.BlockNumber.Uint64())
	}

	from, err := state.GetSender(tx)
	if err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{
		Root:              common.BytesToHash(r.PostState),
		CumulativeGasUsed: ArgUint64(r.CumulativeGasUsed),
		LogsBloom:         r.Bloom,
		Logs:              logs,
		Status:            ArgUint64(r.Status),
		TxHash:            r.TxHash,
		TxIndex:           ArgUint64(r.TransactionIndex),
		BlockHash:         r.BlockHash,
		BlockNumber:       blockNumber,
		GasUsed:           ArgUint64(r.GasUsed),
		ContractAddress:   contractAddress,
		FromAddr:          from,
		ToAddr:            to,
		Type:              ArgUint64(r.Type),
	}
	if r.EffectiveGasPrice != nil {
		egp := ArgBig(*r.EffectiveGasPrice)
		receipt.EffectiveGasPrice = &egp
	}
	return receipt, nil
}

// Log structure
type Log struct {
	Address     common.Address `json:"address"`
	Topics      []common.Hash  `json:"topics"`
	Data        ArgBytes       `json:"data"`
	BlockNumber ArgUint64      `json:"blockNumber"`
	TxHash      common.Hash    `json:"transactionHash"`
	TxIndex     ArgUint64      `json:"transactionIndex"`
	BlockHash   common.Hash    `json:"blockHash"`
	LogIndex    ArgUint64      `json:"logIndex"`
	Removed     bool           `json:"removed"`
}

// NewLog creates a new instance of Log
func NewLog(l types.Log) Log {
	return Log{
		Address:     l.Address,
		Topics:      l.Topics,
		Data:        l.Data,
		BlockNumber: ArgUint64(l.BlockNumber),
		TxHash:      l.TxHash,
		TxIndex:     ArgUint64(l.TxIndex),
		BlockHash:   l.BlockHash,
		LogIndex:    ArgUint64(l.Index),
		Removed:     l.Removed,
	}
}

// ExitRoots structure
type ExitRoots struct {
	MainnetExitRoot common.Hash `json:"mainnetExitRoot"`
	RollupExitRoot  common.Hash `json:"rollupExitRoot"`
}
