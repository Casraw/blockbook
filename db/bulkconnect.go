package db

import (
	"time"

	"github.com/golang/glog"
	"github.com/linxGnu/grocksdb"
	"github.com/trezor/blockbook/bchain"
)

// bulk connect
// in bulk mode the data are cached and stored to db in batches
// it speeds up the import in two ways:
// 1) balances and txAddresses are modified several times during the import, there is a chance that the modifications are done before write to DB
// 2) rocksdb seems to handle better fewer larger batches than continuous stream of smaller batches

type bulkAddresses struct {
	bi        BlockInfo
	addresses addressesMap
}

// BulkConnect is used to connect blocks in bulk, faster but if interrupted inconsistent way
type BulkConnect struct {
	d                  *RocksDB
	chainType          bchain.ChainType
	bulkAddresses      []bulkAddresses
	bulkAddressesCount int
	ethBlockTxs        []ethBlockTx
	txAddressesMap     map[string]*TxAddresses
	blockFilters       map[string][]byte
	balances           map[string]*AddrBalance
	addressContracts   map[string]*unpackedAddrContracts
	height             uint32
}

const (
	maxBulkAddresses          = 80000
	maxBulkTxAddresses        = 500000
	partialStoreAddresses     = maxBulkTxAddresses / 10
	maxBulkBalances           = 700000
	partialStoreBalances      = maxBulkBalances / 10
	maxBulkAddrContracts      = 1200000
	partialStoreAddrContracts = maxBulkAddrContracts / 10
	maxBlockFilters           = 1000
)

// InitBulkConnect initializes bulk connect and switches DB to inconsistent state
func (d *RocksDB) InitBulkConnect() (*BulkConnect, error) {
	b := &BulkConnect{
		d:                d,
		chainType:        d.chainParser.GetChainType(),
		txAddressesMap:   make(map[string]*TxAddresses),
		balances:         make(map[string]*AddrBalance),
		addressContracts: make(map[string]*unpackedAddrContracts),
		blockFilters:     make(map[string][]byte),
	}
	if err := d.SetInconsistentState(true); err != nil {
		return nil, err
	}
	glog.Info("rocksdb: bulk connect init, db set to inconsistent state")
	return b, nil
}

func (b *BulkConnect) storeTxAddresses(wb *grocksdb.WriteBatch, all bool) (int, int, error) {
	var txm map[string]*TxAddresses
	var sp int
	if all {
		txm = b.txAddressesMap
		b.txAddressesMap = make(map[string]*TxAddresses)
	} else {
		txm = make(map[string]*TxAddresses)
		for k, a := range b.txAddressesMap {
			// store all completely spent transactions, they will not be modified again
			r := true
			for _, o := range a.Outputs {
				if o.Spent == false {
					r = false
					break
				}
			}
			if r {
				txm[k] = a
				delete(b.txAddressesMap, k)
			}
		}
		sp = len(txm)
		// store some other random transactions if necessary
		if len(txm) < partialStoreAddresses {
			for k, a := range b.txAddressesMap {
				txm[k] = a
				delete(b.txAddressesMap, k)
				if len(txm) >= partialStoreAddresses {
					break
				}
			}
		}
	}
	if err := b.d.storeTxAddresses(wb, txm); err != nil {
		return 0, 0, err
	}
	return len(txm), sp, nil
}

func (b *BulkConnect) parallelStoreTxAddresses(c chan error, all bool) {
	defer close(c)
	start := time.Now()
	wb := grocksdb.NewWriteBatch()
	defer wb.Destroy()
	count, sp, err := b.storeTxAddresses(wb, all)
	if err != nil {
		c <- err
		return
	}
	if err := b.d.WriteBatch(wb); err != nil {
		c <- err
		return
	}
	glog.Info("rocksdb: height ", b.height, ", stored ", count, " (", sp, " spent) txAddresses, ", len(b.txAddressesMap), " remaining, done in ", time.Since(start))
	c <- nil
}

func (b *BulkConnect) storeBalances(wb *grocksdb.WriteBatch, all bool) (int, error) {
	var bal map[string]*AddrBalance
	if all {
		bal = b.balances
		b.balances = make(map[string]*AddrBalance)
	} else {
		bal = make(map[string]*AddrBalance)
		// store some random balances
		for k, a := range b.balances {
			bal[k] = a
			delete(b.balances, k)
			if len(bal) >= partialStoreBalances {
				break
			}
		}
	}
	if err := b.d.storeBalances(wb, bal); err != nil {
		return 0, err
	}
	return len(bal), nil
}

func (b *BulkConnect) parallelStoreBalances(c chan error, all bool) {
	defer close(c)
	start := time.Now()
	wb := grocksdb.NewWriteBatch()
	defer wb.Destroy()
	count, err := b.storeBalances(wb, all)
	if err != nil {
		c <- err
		return
	}
	if err := b.d.WriteBatch(wb); err != nil {
		c <- err
		return
	}
	glog.Info("rocksdb: height ", b.height, ", stored ", count, " balances, ", len(b.balances), " remaining, done in ", time.Since(start))
	c <- nil
}

func (b *BulkConnect) storeBulkAddresses(wb *grocksdb.WriteBatch) error {
	for _, ba := range b.bulkAddresses {
		if err := b.d.storeAddresses(wb, ba.bi.Height, ba.addresses); err != nil {
			return err
		}
		if err := b.d.writeHeight(wb, ba.bi.Height, &ba.bi, opInsert); err != nil {
			return err
		}
	}
	b.bulkAddressesCount = 0
	b.bulkAddresses = b.bulkAddresses[:0]
	return nil
}

func (b *BulkConnect) storeBulkBlockFilters(wb *grocksdb.WriteBatch) error {
	for blockHash, blockFilter := range b.blockFilters {
		if err := b.d.storeBlockFilter(wb, blockHash, blockFilter); err != nil {
			return err
		}
	}
	b.blockFilters = make(map[string][]byte)
	return nil
}

func (b *BulkConnect) connectBlockBitcoinType(block *bchain.Block, storeBlockTxs bool) error {
	addresses := make(addressesMap)
	gf, err := bchain.NewGolombFilter(b.d.is.BlockGolombFilterP, b.d.is.BlockFilterScripts, block.BlockHeader.Hash, b.d.is.BlockFilterUseZeroedKey)
	if err != nil {
		glog.Error("connectBlockBitcoinType golomb filter error ", err)
		gf = nil
	} else if gf != nil && !gf.Enabled {
		gf = nil
	}
	if err := b.d.processAddressesBitcoinType(block, addresses, b.txAddressesMap, b.balances, gf); err != nil {
		return err
	}
	var storeAddressesChan, storeBalancesChan chan error
	var sa bool
	if len(b.txAddressesMap) > maxBulkTxAddresses || len(b.balances) > maxBulkBalances {
		sa = true
		if len(b.txAddressesMap)+partialStoreAddresses > maxBulkTxAddresses {
			storeAddressesChan = make(chan error)
			go b.parallelStoreTxAddresses(storeAddressesChan, false)
		}
		if len(b.balances)+partialStoreBalances > maxBulkBalances {
			storeBalancesChan = make(chan error)
			go b.parallelStoreBalances(storeBalancesChan, false)
		}
	}
	b.bulkAddresses = append(b.bulkAddresses, bulkAddresses{
		bi: BlockInfo{
			Hash:   block.Hash,
			Time:   block.Time,
			Txs:    uint32(len(block.Txs)),
			Size:   uint32(block.Size),
			Height: block.Height,
		},
		addresses: addresses,
	})
	b.bulkAddressesCount += len(addresses)
	if gf != nil {
		b.blockFilters[block.BlockHeader.Hash] = gf.Compute()
	}
	// open WriteBatch only if going to write
	if sa || b.bulkAddressesCount > maxBulkAddresses || storeBlockTxs || len(b.blockFilters) > maxBlockFilters {
		start := time.Now()
		wb := grocksdb.NewWriteBatch()
		defer wb.Destroy()
		bac := b.bulkAddressesCount
		if sa || b.bulkAddressesCount > maxBulkAddresses {
			if err := b.storeBulkAddresses(wb); err != nil {
				return err
			}
		}
		if storeBlockTxs {
			if err := b.d.storeAndCleanupBlockTxs(wb, block); err != nil {
				return err
			}
		}
		if len(b.blockFilters) > maxBlockFilters {
			if err := b.storeBulkBlockFilters(wb); err != nil {
				return err
			}
		}
		if err := b.d.WriteBatch(wb); err != nil {
			return err
		}
		if bac > b.bulkAddressesCount {
			glog.Info("rocksdb: height ", b.height, ", stored ", bac, " addresses, done in ", time.Since(start))
		}
	}
	if storeAddressesChan != nil {
		if err := <-storeAddressesChan; err != nil {
			return err
		}
	}
	if storeBalancesChan != nil {
		if err := <-storeBalancesChan; err != nil {
			return err
		}
	}
	return nil
}

func (b *BulkConnect) storeAddressContracts(wb *grocksdb.WriteBatch, all bool) (int, error) {
	var ac map[string]*unpackedAddrContracts
	if all {
		ac = b.addressContracts
		b.addressContracts = make(map[string]*unpackedAddrContracts)
	} else {
		ac = make(map[string]*unpackedAddrContracts)
		// store some random address contracts
		for k, a := range b.addressContracts {
			ac[k] = a
			delete(b.addressContracts, k)
			if len(ac) >= partialStoreAddrContracts {
				break
			}
		}
	}
	if err := b.d.storeUnpackedAddressContracts(wb, ac); err != nil {
		return 0, err
	}
	return len(ac), nil
}

func (b *BulkConnect) parallelStoreAddressContracts(c chan error, all bool) {
	defer close(c)
	start := time.Now()
	wb := grocksdb.NewWriteBatch()
	defer wb.Destroy()
	count, err := b.storeAddressContracts(wb, all)
	if err != nil {
		c <- err
		return
	}
	if err := b.d.WriteBatch(wb); err != nil {
		c <- err
		return
	}
	glog.Info("rocksdb: height ", b.height, ", stored ", count, " addressContracts, ", len(b.addressContracts), " remaining, done in ", time.Since(start))
	c <- nil
}

func (b *BulkConnect) connectBlockEthereumType(block *bchain.Block, storeBlockTxs bool) error {
	addresses := make(addressesMap)
	blockTxs, err := b.d.processAddressesEthereumType(block, addresses, b.addressContracts)
	if err != nil {
		return err
	}
	b.ethBlockTxs = append(b.ethBlockTxs, blockTxs...)
	var storeAddrContracts chan error
	var sa bool
	if len(b.addressContracts) > maxBulkAddrContracts {
		sa = true
		storeAddrContracts = make(chan error)
		go b.parallelStoreAddressContracts(storeAddrContracts, false)
	}
	b.bulkAddresses = append(b.bulkAddresses, bulkAddresses{
		bi: BlockInfo{
			Hash:   block.Hash,
			Time:   block.Time,
			Txs:    uint32(len(block.Txs)),
			Size:   uint32(block.Size),
			Height: block.Height,
		},
		addresses: addresses,
	})
	b.bulkAddressesCount += len(addresses)
	// open WriteBatch only if going to write
	if sa || b.bulkAddressesCount > maxBulkAddresses || storeBlockTxs {
		start := time.Now()
		wb := grocksdb.NewWriteBatch()
		defer wb.Destroy()
		bac := b.bulkAddressesCount
		if sa || b.bulkAddressesCount > maxBulkAddresses {
			if err = b.storeBulkAddresses(wb); err != nil {
				return err
			}
		}
		if err = b.d.storeInternalDataEthereumType(wb, b.ethBlockTxs); err != nil {
			return err
		}
		b.ethBlockTxs = b.ethBlockTxs[:0]
		if err = b.d.storeBlockSpecificDataEthereumType(wb, block); err != nil {
			return err
		}
		if storeBlockTxs {
			if err = b.d.storeAndCleanupBlockTxsEthereumType(wb, block, blockTxs); err != nil {
				return err
			}
		}
		if err = b.d.WriteBatch(wb); err != nil {
			return err
		}
		if bac > b.bulkAddressesCount {
			glog.Info("rocksdb: height ", b.height, ", stored ", bac, " addresses, done in ", time.Since(start))
		}
	} else {
		// if there are blockSpecificData, store them
		blockSpecificData, _ := block.CoinSpecificData.(*bchain.EthereumBlockSpecificData)
		if blockSpecificData != nil {
			wb := grocksdb.NewWriteBatch()
			defer wb.Destroy()
			if err = b.d.storeBlockSpecificDataEthereumType(wb, block); err != nil {
				return err
			}
			if err := b.d.WriteBatch(wb); err != nil {
				return err
			}
		}
	}
	if storeAddrContracts != nil {
		if err := <-storeAddrContracts; err != nil {
			return err
		}
	}
	return nil
}

// ConnectBlock connects block in bulk mode
func (b *BulkConnect) ConnectBlock(block *bchain.Block, storeBlockTxs bool) error {
	b.height = block.Height
	if b.chainType == bchain.ChainBitcoinType {
		return b.connectBlockBitcoinType(block, storeBlockTxs)
	} else if b.chainType == bchain.ChainEthereumType {
		return b.connectBlockEthereumType(block, storeBlockTxs)
	}
	// for default is to connect blocks in non bulk mode
	return b.d.ConnectBlock(block)
}

// Close flushes the cached data and switches DB from inconsistent state open
// after Close, the BulkConnect cannot be used
func (b *BulkConnect) Close() error {
	glog.Info("rocksdb: bulk connect closing")
	start := time.Now()
	var storeTxAddressesChan, storeBalancesChan, storeAddressContractsChan chan error
	if b.chainType == bchain.ChainBitcoinType {
		storeTxAddressesChan = make(chan error)
		go b.parallelStoreTxAddresses(storeTxAddressesChan, true)
		storeBalancesChan = make(chan error)
		go b.parallelStoreBalances(storeBalancesChan, true)
	} else if b.chainType == bchain.ChainEthereumType {
		storeAddressContractsChan = make(chan error)
		go b.parallelStoreAddressContracts(storeAddressContractsChan, true)
	}
	wb := grocksdb.NewWriteBatch()
	defer wb.Destroy()
	if err := b.d.storeInternalDataEthereumType(wb, b.ethBlockTxs); err != nil {
		return err
	}
	b.ethBlockTxs = b.ethBlockTxs[:0]
	bac := b.bulkAddressesCount
	if err := b.storeBulkAddresses(wb); err != nil {
		return err
	}
	if err := b.storeBulkBlockFilters(wb); err != nil {
		return err
	}
	if err := b.d.WriteBatch(wb); err != nil {
		return err
	}
	glog.Info("rocksdb: height ", b.height, ", stored ", bac, " addresses, done in ", time.Since(start))
	if storeTxAddressesChan != nil {
		if err := <-storeTxAddressesChan; err != nil {
			return err
		}
	}
	if storeBalancesChan != nil {
		if err := <-storeBalancesChan; err != nil {
			return err
		}
	}
	if storeAddressContractsChan != nil {
		if err := <-storeAddressContractsChan; err != nil {
			return err
		}
	}
	if err := b.d.SetInconsistentState(false); err != nil {
		return err
	}
	glog.Info("rocksdb: bulk connect closed, db set to open state")

	// set block times asynchronously (if not in unit test), it slows server startup for chains with large number of blocks
	if b.d.is.Coin == "coin-unittest" {
		b.d.setBlockTimes()
	} else {
		go b.d.setBlockTimes()
	}

	b.d = nil
	return nil
}
