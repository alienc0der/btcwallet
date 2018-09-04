package chain

import (
	"container/list"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

var (
	// ErrBitcoindClientShuttingDown is an error returned when we attempt
	// to receive a notification for a specific item and the bitcoind client
	// is in the middle of shutting down.
	ErrBitcoindClientShuttingDown = errors.New("client is shutting down")
)

// BitcoindClient represents a persistent client connection to a bitcoind server
// for information regarding the current best block chain.
type BitcoindClient struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	// birthday is the earliest time for which we should begin scanning the
	// chain.
	birthday time.Time

	// chainParams are the parameters of the current chain this client is
	// active under.
	chainParams *chaincfg.Params

	// id is the unique ID of this client assigned by the backing bitcoind
	// connection.
	id uint64

	// chainConn is the backing client to our rescan client that contains
	// the RPC and ZMQ connections to a bitcoind node.
	chainConn *BitcoindConn

	// bestBlock keeps track of the tip of the current best chain.
	bestBlockMtx sync.RWMutex
	bestBlock    waddrmgr.BlockStamp

	// notifyBlocks signals whether the client is sending block
	// notifications to the caller.
	notifyBlocks uint32

	// rescanUpdate is a channel will be sent items that we should match
	// transactions against while processing a chain rescan to determine if
	// they are relevant to the client.
	rescanUpdate chan interface{}

	// watchedAddresses, watchedOutPoints, and watchedTxs are the set of
	// items we should match transactions against while processing a chain
	// rescan to determine if they are relevant to the client.
	watchMtx         sync.RWMutex
	watchedAddresses map[string]struct{}
	watchedOutPoints map[wire.OutPoint]struct{}
	watchedTxs       map[chainhash.Hash]struct{}

	// mempool keeps track of all relevant transactions that have yet to be
	// confirmed. This is used to shortcut the filtering process of a
	// transaction when a new confirmed transaction notification is
	// received.
	//
	// NOTE: This requires the watchMtx to be held.
	mempool map[chainhash.Hash]struct{}

	// expiredMempool keeps track of a set of confirmed transactions along
	// with the height at which they were included in a block. These
	// transactions will then be removed from the mempool after a period of
	// 288 blocks. This is done to ensure the transactions are safe from a
	// reorg in the chain.
	//
	// NOTE: This requires the watchMtx to be held.
	expiredMempool map[int32]map[chainhash.Hash]struct{}

	// notificationQueue is a concurrent unbounded queue that handles
	// dispatching notifications to the subscriber of this client.
	//
	// TODO: Rather than leaving this as an unbounded queue for all types of
	// notifications, try dropping ones where a later enqueued notification
	// can fully invalidate one waiting to be processed. For example,
	// BlockConnected notifications for greater block heights can remove the
	// need to process earlier notifications still waiting to be processed.
	notificationQueue *ConcurrentQueue

	// zmqTxNtfns is a channel through which ZMQ transaction events will be
	// retrieved from the backing bitcoind connection.
	zmqTxNtfns chan *wire.MsgTx

	// zmqBlockNtfns is a channel through which ZMQ block events will be
	// retrieved from the backing bitcoind connection.
	zmqBlockNtfns chan *wire.MsgBlock

	quit chan struct{}
	wg   sync.WaitGroup
}

// BackEnd returns the name of the driver.
func (c *BitcoindClient) BackEnd() string {
	return "bitcoind"
}

// GetBestBlock returns the highest block known to bitcoind.
func (c *BitcoindClient) GetBestBlock() (*chainhash.Hash, int32, error) {
	bcinfo, err := c.chainConn.client.GetBlockChainInfo()
	if err != nil {
		return nil, 0, err
	}

	hash, err := chainhash.NewHashFromStr(bcinfo.BestBlockHash)
	if err != nil {
		return nil, 0, err
	}

	return hash, bcinfo.Blocks, nil
}

// GetBlockHeight returns the height for the hash, if known, or returns an
// error.
func (c *BitcoindClient) GetBlockHeight(hash *chainhash.Hash) (int32, error) {
	header, err := c.chainConn.client.GetBlockHeaderVerbose(hash)
	if err != nil {
		return 0, err
	}

	return header.Height, nil
}

// GetBlock returns a block from the hash.
func (c *BitcoindClient) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	return c.chainConn.client.GetBlock(hash)
}

// GetBlockVerbose returns a verbose block from the hash.
func (c *BitcoindClient) GetBlockVerbose(
	hash *chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {

	return c.chainConn.client.GetBlockVerbose(hash)
}

// GetBlockHash returns a block hash from the height.
func (c *BitcoindClient) GetBlockHash(height int64) (*chainhash.Hash, error) {
	return c.chainConn.client.GetBlockHash(height)
}

// GetBlockHeader returns a block header from the hash.
func (c *BitcoindClient) GetBlockHeader(
	hash *chainhash.Hash) (*wire.BlockHeader, error) {

	return c.chainConn.client.GetBlockHeader(hash)
}

// GetBlockHeaderVerbose returns a block header from the hash.
func (c *BitcoindClient) GetBlockHeaderVerbose(
	hash *chainhash.Hash) (*btcjson.GetBlockHeaderVerboseResult, error) {

	return c.chainConn.client.GetBlockHeaderVerbose(hash)
}

// GetRawTransactionVerbose returns a transaction from the tx hash.
func (c *BitcoindClient) GetRawTransactionVerbose(
	hash *chainhash.Hash) (*btcjson.TxRawResult, error) {

	return c.chainConn.client.GetRawTransactionVerbose(hash)
}

// GetTxOut returns a txout from the outpoint info provided.
func (c *BitcoindClient) GetTxOut(txHash *chainhash.Hash, index uint32,
	mempool bool) (*btcjson.GetTxOutResult, error) {

	return c.chainConn.client.GetTxOut(txHash, index, mempool)
}

// SendRawTransaction sends a raw transaction via bitcoind.
func (c *BitcoindClient) SendRawTransaction(tx *wire.MsgTx,
	allowHighFees bool) (*chainhash.Hash, error) {

	return c.chainConn.client.SendRawTransaction(tx, allowHighFees)
}

// Notifications returns a channel to retrieve notifications from.
//
// NOTE: This is part of the chain.Interface interface.
func (c *BitcoindClient) Notifications() <-chan interface{} {
	return c.notificationQueue.ChanOut()
}

// NotifyReceived allows the chain backend to notify the caller whenever a
// transaction pays to any of the given addresses.
//
// NOTE: This is part of the chain.Interface interface.
func (c *BitcoindClient) NotifyReceived(addrs []btcutil.Address) error {
	c.NotifyBlocks()

	select {
	case c.rescanUpdate <- addrs:
	case <-c.quit:
		return ErrBitcoindClientShuttingDown
	}

	return nil
}

// NotifySpent allows the chain backend to notify the caller whenever a
// transaction spends any of the given outpoints.
func (c *BitcoindClient) NotifySpent(outPoints []*wire.OutPoint) error {
	c.NotifyBlocks()

	select {
	case c.rescanUpdate <- outPoints:
	case <-c.quit:
		return ErrBitcoindClientShuttingDown
	}

	return nil
}

// NotifyTx allows the chain backend to notify the caller whenever any of the
// given transactions confirm within the chain.
func (c *BitcoindClient) NotifyTx(txids []chainhash.Hash) error {
	c.NotifyBlocks()

	select {
	case c.rescanUpdate <- txids:
	case <-c.quit:
		return ErrBitcoindClientShuttingDown
	}

	return nil
}

// NotifyBlocks allows the chain backend to notify the caller whenever a block
// is connected or disconnected.
//
// NOTE: This is part of the chain.Interface interface.
func (c *BitcoindClient) NotifyBlocks() error {
	atomic.StoreUint32(&c.notifyBlocks, 1)
	return nil
}

// shouldNotifyBlocks determines whether the client should send block
// notifications to the caller.
func (c *BitcoindClient) shouldNotifyBlocks() bool {
	return atomic.LoadUint32(&c.notifyBlocks) == 1
}

// LoadTxFilter uses the given filters to what we should match transactions
// against to determine if they are relevant to the client. The reset argument
// is used to reset the current filters.
//
// The current filters supported are of the following types:
//	[]btcutil.Address
//	[]wire.OutPoint
//	[]*wire.OutPoint
//	map[wire.OutPoint]btcutil.Address
//	[]chainhash.Hash
//	[]*chainhash.Hash
func (c *BitcoindClient) LoadTxFilter(reset bool, filters ...interface{}) error {
	if reset {
		select {
		case c.rescanUpdate <- struct{}{}:
		case <-c.quit:
			return ErrBitcoindClientShuttingDown
		}
	}

	updateFilter := func(filter interface{}) error {
		select {
		case c.rescanUpdate <- filter:
		case <-c.quit:
			return ErrBitcoindClientShuttingDown
		}

		return nil
	}

	// In order to make this operation atomic, we'll iterate through the
	// filters twice: the first to ensure there aren't any unsupported
	// filter types, and the second to actually update our filters.
	for _, filter := range filters {
		switch filter := filter.(type) {
		case []btcutil.Address, []wire.OutPoint, []*wire.OutPoint,
			map[wire.OutPoint]btcutil.Address, []chainhash.Hash,
			[]*chainhash.Hash:

			// Proceed to check the next filter type.
		default:
			return fmt.Errorf("unsupported filter type %T", filter)
		}
	}

	for _, filter := range filters {
		if err := updateFilter(filter); err != nil {
			return err
		}
	}

	return nil
}

// RescanBlocks rescans any blocks passed, returning only the blocks that
// matched as []btcjson.BlockDetails.
func (c *BitcoindClient) RescanBlocks(
	blockHashes []chainhash.Hash) ([]btcjson.RescannedBlock, error) {

	rescannedBlocks := make([]btcjson.RescannedBlock, 0, len(blockHashes))
	for _, hash := range blockHashes {
		header, err := c.GetBlockHeaderVerbose(&hash)
		if err != nil {
			log.Warnf("Unable to get header %s from bitcoind: %s",
				hash, err)
			continue
		}

		block, err := c.GetBlock(&hash)
		if err != nil {
			log.Warnf("Unable to get block %s from bitcoind: %s",
				hash, err)
			continue
		}

		relevantTxs, err := c.filterBlock(block, header.Height, false)
		if len(relevantTxs) > 0 {
			rescannedBlock := btcjson.RescannedBlock{
				Hash: hash.String(),
			}
			for _, tx := range relevantTxs {
				rescannedBlock.Transactions = append(
					rescannedBlock.Transactions,
					hex.EncodeToString(tx.SerializedTx),
				)
			}

			rescannedBlocks = append(rescannedBlocks, rescannedBlock)
		}
	}

	return rescannedBlocks, nil
}

// Rescan rescans from the block with the given hash until the current block,
// after adding the passed addresses and outpoints to the client's watch list.
func (c *BitcoindClient) Rescan(blockHash *chainhash.Hash,
	addresses []btcutil.Address, outPoints map[wire.OutPoint]btcutil.Address) error {

	// A block hash is required to use as the starting point of the rescan.
	if blockHash == nil {
		return errors.New("rescan requires a starting block hash")
	}

	// We'll then update our filters with the given outpoints and addresses.
	select {
	case c.rescanUpdate <- addresses:
	case <-c.quit:
		return ErrBitcoindClientShuttingDown
	}

	select {
	case c.rescanUpdate <- outPoints:
	case <-c.quit:
		return ErrBitcoindClientShuttingDown
	}

	// Once the filters have been updated, we can begin the rescan.
	select {
	case c.rescanUpdate <- *blockHash:
	case <-c.quit:
		return ErrBitcoindClientShuttingDown
	}

	return nil
}

// Start initializes the bitcoind rescan client using the backing bitcoind
// connection and starts all goroutines necessary in order to process rescans
// and ZMQ notifications.
//
// NOTE: This is part of the chain.Interface interface.
func (c *BitcoindClient) Start() error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}

	// Start the notification queue and immediately dispatch a
	// ClientConnected notification to the caller. This is needed as some of
	// the callers will require this notification before proceeding.
	c.notificationQueue.Start()
	c.notificationQueue.ChanIn() <- ClientConnected{}

	// Retrieve the best block of the chain.
	bestHash, bestHeight, err := c.GetBestBlock()
	if err != nil {
		return fmt.Errorf("unable to retrieve best block: %v", err)
	}
	bestHeader, err := c.GetBlockHeaderVerbose(bestHash)
	if err != nil {
		return fmt.Errorf("unable to retrieve header for best block: "+
			"%v", err)
	}

	c.bestBlockMtx.Lock()
	c.bestBlock = waddrmgr.BlockStamp{
		Hash:      *bestHash,
		Height:    bestHeight,
		Timestamp: time.Unix(bestHeader.Time, 0),
	}
	c.bestBlockMtx.Unlock()

	// Once the client has started successfully, we'll include it in the set
	// of rescan clients of the backing bitcoind connection in order to
	// received ZMQ event notifications.
	c.chainConn.AddClient(c)

	c.wg.Add(2)
	go c.rescanHandler()
	go c.ntfnHandler()

	return nil
}

// Stop stops the bitcoind rescan client from processing rescans and ZMQ
// notifications.
//
// NOTE: This is part of the chain.Interface interface.
func (c *BitcoindClient) Stop() {
	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return
	}

	close(c.quit)

	// Remove this client's reference from the bitcoind connection to
	// prevent sending notifications to it after it's been stopped.
	c.chainConn.RemoveClient(c.id)

	c.notificationQueue.Stop()
}

// WaitForShutdown blocks until the client has finished disconnecting and all
// handlers have exited.
//
// NOTE: This is part of the chain.Interface interface.
func (c *BitcoindClient) WaitForShutdown() {
	c.wg.Wait()
}

// rescanHandler handles the logic needed for the caller to trigger a chain
// rescan.
//
// NOTE: This must be called as a goroutine.
func (c *BitcoindClient) rescanHandler() {
	defer c.wg.Done()

	for {
		select {
		case update := <-c.rescanUpdate:
			switch update := update.(type) {

			// We're clearing the filters.
			case struct{}:
				c.watchMtx.Lock()
				c.watchedOutPoints = make(map[wire.OutPoint]struct{})
				c.watchedAddresses = make(map[string]struct{})
				c.watchedTxs = make(map[chainhash.Hash]struct{})
				c.watchMtx.Unlock()

			// We're adding the addresses to our filter.
			case []btcutil.Address:
				c.watchMtx.Lock()
				for _, addr := range update {
					c.watchedAddresses[addr.String()] = struct{}{}
				}
				c.watchMtx.Unlock()

			// We're adding the outpoints to our filter.
			case []wire.OutPoint:
				c.watchMtx.Lock()
				for _, op := range update {
					c.watchedOutPoints[op] = struct{}{}
				}
				c.watchMtx.Unlock()
			case []*wire.OutPoint:
				c.watchMtx.Lock()
				for _, op := range update {
					c.watchedOutPoints[*op] = struct{}{}
				}
				c.watchMtx.Unlock()

			// We're adding the outpoints that map to the scripts
			// that we should scan for to our filter.
			case map[wire.OutPoint]btcutil.Address:
				c.watchMtx.Lock()
				for op := range update {
					c.watchedOutPoints[op] = struct{}{}
				}
				c.watchMtx.Unlock()

			// We're adding the transactions to our filter.
			case []chainhash.Hash:
				c.watchMtx.Lock()
				for _, txid := range update {
					c.watchedTxs[txid] = struct{}{}
				}
				c.watchMtx.Unlock()
			case []*chainhash.Hash:
				c.watchMtx.Lock()
				for _, txid := range update {
					c.watchedTxs[*txid] = struct{}{}
				}
				c.watchMtx.Unlock()

			// We're starting a rescan from the hash.
			case chainhash.Hash:
				if err := c.rescan(update); err != nil {
					log.Errorf("Unable to complete chain "+
						"rescan: %v", err)
				}
			default:
				log.Warnf("Received unexpected filter type %T",
					update)
			}
		case <-c.quit:
			return
		}
	}
}

// ntfnHandler handles the logic to retrieve ZMQ notifications from the backing
// bitcoind connection.
//
// NOTE: This must be called as a goroutine.
func (c *BitcoindClient) ntfnHandler() {
	defer c.wg.Done()

	for {
		select {
		case tx := <-c.zmqTxNtfns:
			if _, _, err := c.filterTx(tx, nil, true); err != nil {
				log.Errorf("Unable to filter transaction %v: %v",
					tx.TxHash(), err)
			}
		case newBlock := <-c.zmqBlockNtfns:
			// If the new block's previous hash matches the best
			// hash known to us, then the new block is the next
			// successor, so we'll update our best block to reflect
			// this and determine if this new block matches any of
			// our existing filters.
			c.bestBlockMtx.Lock()
			bestBlock := c.bestBlock
			c.bestBlockMtx.Unlock()
			if newBlock.Header.PrevBlock == bestBlock.Hash {
				newBlockHeight := bestBlock.Height + 1
				_, err := c.filterBlock(
					newBlock, newBlockHeight, true,
				)
				if err != nil {
					log.Errorf("Unable to filter block %v: %v",
						newBlock.BlockHash(), err)
					continue
				}

				// With the block succesfully filtered, we'll
				// make it our new best block.
				bestBlock.Hash = newBlock.BlockHash()
				bestBlock.Height = newBlockHeight
				bestBlock.Timestamp = newBlock.Header.Timestamp

				c.bestBlockMtx.Lock()
				c.bestBlock = bestBlock
				c.bestBlockMtx.Unlock()

				continue
			}

			// Otherwise, we've encountered a reorg.
			if err := c.reorg(bestBlock, newBlock); err != nil {
				log.Errorf("Unable to process chain reorg: %v",
					err)
			}
		case <-c.quit:
			return
		}
	}
}

// BlockStamp returns the latest block notified by the client, or an error
// if the client has been shut down.
func (c *BitcoindClient) BlockStamp() (*waddrmgr.BlockStamp, error) {
	c.bestBlockMtx.RLock()
	bestBlock := c.bestBlock
	c.bestBlockMtx.RUnlock()

	return &bestBlock, nil
}

// onBlockConnected is a callback that's executed whenever a new block has been
// detected. This will queue a BlockConnected notification to the caller.
func (c *BitcoindClient) onBlockConnected(hash *chainhash.Hash, height int32,
	timestamp time.Time) {

	if c.shouldNotifyBlocks() {
		select {
		case c.notificationQueue.ChanIn() <- BlockConnected{
			Block: wtxmgr.Block{
				Hash:   *hash,
				Height: height,
			},
			Time: timestamp,
		}:
		case <-c.quit:
		}
	}
}

// onFilteredBlockConnected is an alternative callback that's executed whenever
// a new block has been detected. It serves the same purpose as
// onBlockConnected, but it also includes a list of the relevant transactions
// found within the block being connected. This will queue a
// FilteredBlockConnected notification to the caller.
func (c *BitcoindClient) onFilteredBlockConnected(height int32,
	header *wire.BlockHeader, relevantTxs []*wtxmgr.TxRecord) {

	if c.shouldNotifyBlocks() {
		select {
		case c.notificationQueue.ChanIn() <- FilteredBlockConnected{
			Block: &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{
					Hash:   header.BlockHash(),
					Height: height,
				},
				Time: header.Timestamp,
			},
			RelevantTxs: relevantTxs,
		}:
		case <-c.quit:
		}
	}
}

// onBlockDisconnected is a callback that's executed whenever a block has been
// disconnected. This will queue a BlockDisconnected notification to the caller
// with the details of the block being disconnected.
func (c *BitcoindClient) onBlockDisconnected(hash *chainhash.Hash, height int32,
	timestamp time.Time) {

	if c.shouldNotifyBlocks() {
		select {
		case c.notificationQueue.ChanIn() <- BlockDisconnected{
			Block: wtxmgr.Block{
				Hash:   *hash,
				Height: height,
			},
			Time: timestamp,
		}:
		case <-c.quit:
		}
	}
}

// onRelevantTx is a callback that's executed whenever a transaction is relevant
// to the caller. This means that the transaction matched a specific item in the
// client's different filters. This will queue a RelevantTx notification to the
// caller.
func (c *BitcoindClient) onRelevantTx(tx *wtxmgr.TxRecord,
	blockDetails *btcjson.BlockDetails) {

	block, err := parseBlock(blockDetails)
	if err != nil {
		log.Errorf("Unable to send onRelevantTx notification, failed "+
			"parse block: %v", err)
		return
	}

	select {
	case c.notificationQueue.ChanIn() <- RelevantTx{
		TxRecord: tx,
		Block:    block,
	}:
	case <-c.quit:
	}
}

// onRescanProgress is a callback that's executed whenever a rescan is in
// progress. This will queue a RescanProgress notification to the caller with
// the current rescan progress details.
func (c *BitcoindClient) onRescanProgress(hash *chainhash.Hash, height int32,
	timestamp time.Time) {

	select {
	case c.notificationQueue.ChanIn() <- &RescanProgress{
		Hash:   hash,
		Height: height,
		Time:   timestamp,
	}:
	case <-c.quit:
	}
}

// onRescanFinished is a callback that's executed whenever a rescan has
// finished. This will queue a RescanFinished notification to the caller with
// the details of the last block in the range of the rescan.
func (c *BitcoindClient) onRescanFinished(hash *chainhash.Hash, height int32,
	timestamp time.Time) {

	log.Infof("Rescan finished at %d (%s)", height, hash)

	select {
	case c.notificationQueue.ChanIn() <- &RescanFinished{
		Hash:   hash,
		Height: height,
		Time:   timestamp,
	}:
	case <-c.quit:
	}
}

// reorg processes a reorganization during chain synchronization. This is
// separate from a rescan's handling of a reorg. This will rewind back until it
// finds a common ancestor and notify all the new blocks since then.
func (c *BitcoindClient) reorg(currentBlock waddrmgr.BlockStamp,
	reorgBlock *wire.MsgBlock) error {

	log.Debugf("Possible reorg at block %s", reorgBlock.BlockHash())

	// Retrieve the best known height based on the block which caused the
	// reorg. This way, we can preserve the chain of blocks we need to
	// retrieve.
	bestHash := reorgBlock.BlockHash()
	bestHeight, err := c.GetBlockHeight(&bestHash)
	if err != nil {
		return err
	}

	if bestHeight < currentBlock.Height {
		log.Debug("Detected multiple reorgs")
		return nil
	}

	// We'll now keep track of all the blocks known to the *chain*, starting
	// from the best block known to us until the best block in the chain.
	// This will let us fast-forward despite any future reorgs.
	blocksToNotify := list.New()
	blocksToNotify.PushFront(reorgBlock)
	previousBlock := reorgBlock.Header.PrevBlock
	for i := bestHeight - 1; i >= currentBlock.Height; i-- {
		block, err := c.GetBlock(&previousBlock)
		if err != nil {
			return err
		}
		blocksToNotify.PushFront(block)
		previousBlock = block.Header.PrevBlock
	}

	// Rewind back to the last common ancestor block using the previous
	// block hash from each header to avoid any race conditions. If we
	// encounter more reorgs, they'll be queued and we'll repeat the cycle.
	//
	// We'll start by retrieving the header to the best block known to us.
	currentHeader, err := c.GetBlockHeader(&currentBlock.Hash)
	if err != nil {
		return err
	}

	// Then, we'll walk backwards in the chain until we find our common
	// ancestor.
	for previousBlock != currentHeader.PrevBlock {
		// Since the previous hashes don't match, the current block has
		// been reorged out of the chain, so we should send a
		// BlockDisconnected notification for it.
		log.Debugf("Disconnecting block %d (%v)", currentBlock.Height,
			currentBlock.Hash)

		c.onBlockDisconnected(
			&currentBlock.Hash, currentBlock.Height,
			currentBlock.Timestamp,
		)

		// Our current block should now reflect the previous one to
		// continue the common ancestor search.
		currentHeader, err = c.GetBlockHeader(&currentHeader.PrevBlock)
		if err != nil {
			return err
		}

		currentBlock.Height--
		currentBlock.Hash = currentHeader.PrevBlock
		currentBlock.Timestamp = currentHeader.Timestamp

		// Store the correct block in our list in order to notify it
		// once we've found our common ancestor.
		block, err := c.GetBlock(&previousBlock)
		if err != nil {
			return err
		}
		blocksToNotify.PushFront(block)
		previousBlock = block.Header.PrevBlock
	}

	// Disconnect the last block from the old chain. Since the previous
	// block remains the same between the old and new chains, the tip will
	// now be the last common ancestor.
	log.Debugf("Disconnecting block %d (%v)", currentBlock.Height,
		currentBlock.Hash)

	c.onBlockDisconnected(
		&currentBlock.Hash, currentBlock.Height, currentHeader.Timestamp,
	)

	currentBlock.Height--

	// Now we fast-forward to the new block, notifying along the way.
	for blocksToNotify.Front() != nil {
		nextBlock := blocksToNotify.Front().Value.(*wire.MsgBlock)
		nextHeight := currentBlock.Height + 1
		nextHash := nextBlock.BlockHash()
		nextHeader, err := c.GetBlockHeader(&nextHash)
		if err != nil {
			return err
		}

		_, err = c.filterBlock(nextBlock, nextHeight, true)
		if err != nil {
			return err
		}

		currentBlock.Height = nextHeight
		currentBlock.Hash = nextHash
		currentBlock.Timestamp = nextHeader.Timestamp

		blocksToNotify.Remove(blocksToNotify.Front())
	}

	c.bestBlockMtx.Lock()
	c.bestBlock = currentBlock
	c.bestBlockMtx.Unlock()

	return nil
}

// FilterBlocks scans the blocks contained in the FilterBlocksRequest for any
// addresses of interest. Each block will be fetched and filtered sequentially,
// returning a FilterBlocksReponse for the first block containing a matching
// address. If no matches are found in the range of blocks requested, the
// returned response will be nil.
//
// NOTE: This is part of the chain.Interface interface.
func (c *BitcoindClient) FilterBlocks(
	req *FilterBlocksRequest) (*FilterBlocksResponse, error) {

	blockFilterer := NewBlockFilterer(c.chainParams, req)

	// Iterate over the requested blocks, fetching each from the rpc client.
	// Each block will scanned using the reverse addresses indexes generated
	// above, breaking out early if any addresses are found.
	for i, block := range req.Blocks {
		// TODO(conner): add prefetching, since we already know we'll be
		// fetching *every* block
		rawBlock, err := c.GetBlock(&block.Hash)
		if err != nil {
			return nil, err
		}

		if !blockFilterer.FilterBlock(rawBlock) {
			continue
		}

		// If any external or internal addresses were detected in this
		// block, we return them to the caller so that the rescan
		// windows can widened with subsequent addresses. The
		// `BatchIndex` is returned so that the caller can compute the
		// *next* block from which to begin again.
		resp := &FilterBlocksResponse{
			BatchIndex:         uint32(i),
			BlockMeta:          block,
			FoundExternalAddrs: blockFilterer.FoundExternal,
			FoundInternalAddrs: blockFilterer.FoundInternal,
			FoundOutPoints:     blockFilterer.FoundOutPoints,
			RelevantTxns:       blockFilterer.RelevantTxns,
		}

		return resp, nil
	}

	// No addresses were found for this range.
	return nil, nil
}

// rescan performs a rescan of the chain using a bitcoind backend, from the
// specified hash to the best known hash, while watching out for reorgs that
// happen during the rescan. It uses the addresses and outputs being tracked by
// the client in the watch list. This is called only within a queue processing
// loop.
func (c *BitcoindClient) rescan(start chainhash.Hash) error {
	log.Infof("Starting rescan from block %s", start)

	// We start by getting the best already processed block. We only use
	// the height, as the hash can change during a reorganization, which we
	// catch by testing connectivity from known blocks to the previous
	// block.
	bestHash, bestHeight, err := c.GetBestBlock()
	if err != nil {
		return err
	}
	bestHeader, err := c.GetBlockHeaderVerbose(bestHash)
	if err != nil {
		return err
	}
	bestBlock := waddrmgr.BlockStamp{
		Hash:      *bestHash,
		Height:    bestHeight,
		Timestamp: time.Unix(bestHeader.Time, 0),
	}

	// Create a list of headers sorted in forward order. We'll use this in
	// the event that we need to backtrack due to a chain reorg.
	headers := list.New()
	previousHeader, err := c.GetBlockHeaderVerbose(&start)
	if err != nil {
		return err
	}
	previousHash, err := chainhash.NewHashFromStr(previousHeader.Hash)
	if err != nil {
		return err
	}
	headers.PushBack(previousHeader)

	// Queue a RescanFinished notification to the caller with the last block
	// processed throughout the rescan once done.
	defer c.onRescanFinished(
		previousHash, previousHeader.Height,
		time.Unix(previousHeader.Time, 0),
	)

	// Cycle through all of the blocks known to bitcoind, being mindful of
	// reorgs.
	for i := previousHeader.Height + 1; i <= bestBlock.Height; i++ {
		hash, err := c.GetBlockHash(int64(i))
		if err != nil {
			return err
		}

		// If the previous header is before the wallet birthday, fetch
		// the current header and construct a dummy block, rather than
		// fetching the whole block itself. This speeds things up as we
		// no longer have to fetch the whole block when we know it won't
		// match any of our filters.
		var block *wire.MsgBlock
		afterBirthday := previousHeader.Time >= c.birthday.Unix()
		if !afterBirthday {
			header, err := c.GetBlockHeader(hash)
			if err != nil {
				return err
			}
			block = &wire.MsgBlock{
				Header: *header,
			}

			afterBirthday = c.birthday.Before(header.Timestamp)
			if afterBirthday {
				c.onRescanProgress(
					previousHash, i,
					block.Header.Timestamp,
				)
			}
		}

		if afterBirthday {
			block, err = c.GetBlock(hash)
			if err != nil {
				return err
			}
		}

		for block.Header.PrevBlock.String() != previousHeader.Hash {
			// If we're in this for loop, it looks like we've been
			// reorganized. We now walk backwards to the common
			// ancestor between the best chain and the known chain.
			//
			// First, we signal a disconnected block to rewind the
			// rescan state.
			c.onBlockDisconnected(
				previousHash, previousHeader.Height,
				time.Unix(previousHeader.Time, 0),
			)

			// Get the previous block of the best chain.
			hash, err := c.GetBlockHash(int64(i - 1))
			if err != nil {
				return err
			}
			block, err = c.GetBlock(hash)
			if err != nil {
				return err
			}

			// Then, we'll the get the header of this previous
			// block.
			if headers.Back() != nil {
				// If it's already in the headers list, we can
				// just get it from there and remove the
				// current hash.
				headers.Remove(headers.Back())
				if headers.Back() != nil {
					previousHeader = headers.Back().
						Value.(*btcjson.GetBlockHeaderVerboseResult)
					previousHash, err = chainhash.NewHashFromStr(
						previousHeader.Hash,
					)
					if err != nil {
						return err
					}
				}
			} else {
				// Otherwise, we get it from bitcoind.
				previousHash, err = chainhash.NewHashFromStr(
					previousHeader.PreviousHash,
				)
				if err != nil {
					return err
				}
				previousHeader, err = c.GetBlockHeaderVerbose(
					previousHash,
				)
				if err != nil {
					return err
				}
			}
		}

		// Now that we've ensured we haven't come across a reorg, we'll
		// add the current block header to our list of headers.
		blockHash := block.BlockHash()
		previousHash = &blockHash
		previousHeader = &btcjson.GetBlockHeaderVerboseResult{
			Hash:         blockHash.String(),
			Height:       i,
			PreviousHash: block.Header.PrevBlock.String(),
			Time:         block.Header.Timestamp.Unix(),
		}
		headers.PushBack(previousHeader)

		// Notify the block and any of its relevant transacations.
		if _, err = c.filterBlock(block, i, true); err != nil {
			return err
		}

		if i%10000 == 0 {
			c.onRescanProgress(
				previousHash, i, block.Header.Timestamp,
			)
		}

		// If we've reached the previously best known block, check to
		// make sure the underlying node hasn't synchronized additional
		// blocks. If it has, update the best known block and continue
		// to rescan to that point.
		if i == bestBlock.Height {
			bestHash, bestHeight, err = c.GetBestBlock()
			if err != nil {
				return err
			}
			bestHeader, err = c.GetBlockHeaderVerbose(bestHash)
			if err != nil {
				return err
			}

			bestBlock.Hash = *bestHash
			bestBlock.Height = bestHeight
			bestBlock.Timestamp = time.Unix(bestHeader.Time, 0)
		}
	}

	return nil
}

// filterBlock filters a block for watched outpoints and addresses, and returns
// any matching transactions, sending notifications along the way.
func (c *BitcoindClient) filterBlock(block *wire.MsgBlock, height int32,
	notify bool) ([]*wtxmgr.TxRecord, error) {

	// If this block happened before the client's birthday, then we'll skip
	// it entirely.
	if block.Header.Timestamp.Before(c.birthday) {
		return nil, nil
	}

	if c.shouldNotifyBlocks() {
		log.Debugf("Filtering block %d (%s) with %d transactions",
			height, block.BlockHash(), len(block.Transactions))
	}

	// Create a block details template to use for all of the confirmed
	// transactions found within this block.
	blockHash := block.BlockHash()
	blockDetails := &btcjson.BlockDetails{
		Hash:   blockHash.String(),
		Height: height,
		Time:   block.Header.Timestamp.Unix(),
	}

	// Now, we'll through all of the transactions in the block keeping track
	// of any relevant to the caller.
	var relevantTxs []*wtxmgr.TxRecord
	confirmedTxs := make(map[chainhash.Hash]struct{})
	for i, tx := range block.Transactions {
		// Update the index in the block details with the index of this
		// transaction.
		blockDetails.Index = i
		isRelevant, rec, err := c.filterTx(tx, blockDetails, notify)
		if err != nil {
			log.Warnf("Unable to filter transaction %v: %v",
				tx.TxHash(), err)
			continue
		}

		if isRelevant {
			relevantTxs = append(relevantTxs, rec)
			confirmedTxs[tx.TxHash()] = struct{}{}
		}
	}

	// Update the expiration map by setting the block's confirmed
	// transactions and deleting any in the mempool that were confirmed
	// over 288 blocks ago.
	c.watchMtx.Lock()
	c.expiredMempool[height] = confirmedTxs
	if oldBlock, ok := c.expiredMempool[height-288]; ok {
		for txHash := range oldBlock {
			delete(c.mempool, txHash)
		}
		delete(c.expiredMempool, height-288)
	}
	c.watchMtx.Unlock()

	if notify {
		c.onFilteredBlockConnected(height, &block.Header, relevantTxs)
		c.onBlockConnected(&blockHash, height, block.Header.Timestamp)
	}

	return relevantTxs, nil
}

// filterTx determines whether a transaction is relevant to the client by
// inspecting the client's different filters.
func (c *BitcoindClient) filterTx(tx *wire.MsgTx,
	blockDetails *btcjson.BlockDetails,
	notify bool) (bool, *wtxmgr.TxRecord, error) {

	txDetails := btcutil.NewTx(tx)
	if blockDetails != nil {
		txDetails.SetIndex(blockDetails.Index)
	}

	rec, err := wtxmgr.NewTxRecordFromMsgTx(txDetails.MsgTx(), time.Now())
	if err != nil {
		log.Errorf("Cannot create transaction record for relevant "+
			"tx: %v", err)
		return false, nil, err
	}
	if blockDetails != nil {
		rec.Received = time.Unix(blockDetails.Time, 0)
	}

	// We'll begin the filtering process by holding the lock to ensure we
	// match exactly against what's currently in the filters.
	c.watchMtx.Lock()
	defer c.watchMtx.Unlock()

	// If we've already seen this transaction and it's now been confirmed,
	// then we'll shortcut the filter process by immediately sending a
	// notification to the caller that the filter matches.
	if _, ok := c.mempool[tx.TxHash()]; ok {
		if notify && blockDetails != nil {
			c.onRelevantTx(rec, blockDetails)
		}
		return true, rec, nil
	}

	// Otherwise, this is a new transaction we have yet to see. We'll need
	// to determine if this transaction is somehow relevant to the caller.
	var isRelevant bool

	// We'll start by cycling through its outputs to determine if it pays to
	// any of the currently watched addresses. If an output matches, we'll
	// add it to our watch list.
	for i, out := range tx.TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			out.PkScript, c.chainParams,
		)
		if err != nil {
			log.Debugf("Unable to parse output script in %s:%d: %v",
				tx.TxHash(), i, err)
			continue
		}

		for _, addr := range addrs {
			if _, ok := c.watchedAddresses[addr.String()]; ok {
				isRelevant = true
				op := wire.OutPoint{
					Hash:  tx.TxHash(),
					Index: uint32(i),
				}
				c.watchedOutPoints[op] = struct{}{}
			}
		}
	}

	// If the transaction didn't pay to any of our watched addresses, we'll
	// check if we're currently watching for the hash of this transaction.
	if !isRelevant {
		if _, ok := c.watchedTxs[tx.TxHash()]; ok {
			isRelevant = true
		}
	}

	// If the transaction didn't pay to any of our watched hashes, we'll
	// check if it spends any of our watched outpoints.
	if !isRelevant {
		for _, in := range tx.TxIn {
			if _, ok := c.watchedOutPoints[in.PreviousOutPoint]; ok {
				isRelevant = true
				break
			}
		}
	}

	// If the transaction is not relevant to us, we can simply exit.
	if !isRelevant {
		return false, rec, nil
	}

	// Otherwise, the transaction matched our filters, so we should dispatch
	// a notification for it. If it's still unconfirmed, we'll include it in
	// our mempool so that it can also be notified as part of
	// FilteredBlockConnected once it confirms.
	if blockDetails == nil {
		c.mempool[tx.TxHash()] = struct{}{}
	}

	c.onRelevantTx(rec, blockDetails)

	return true, rec, nil
}