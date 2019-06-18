// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"bytes"
	"fmt"
	"hash"
	"io"
	"math/big"
	"runtime"
	"sort"

	lru "github.com/hashicorp/golang-lru"
	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/rlp"
	"github.com/ledgerwatch/turbo-geth/trie"
	"golang.org/x/crypto/sha3"
)

// Trie cache generation limit after which to evict trie nodes from memory.
var MaxTrieCacheGen = uint32(4 * 1024 * 1024)

var AccountsBucket = []byte("AT")
var AccountsHistoryBucket = []byte("hAT")
var StorageBucket = []byte("ST")
var StorageHistoryBucket = []byte("hST")
var CodeBucket = []byte("CODE")

const (
	// Number of past tries to keep. This value is chosen such that
	// reasonable chain reorg depths will hit an existing trie.
	maxPastTries = 12

	// Number of codehash->size associations to keep.
	codeSizeCacheSize = 100000
)

type StateReader interface {
	ReadAccountData(address common.Address) (*accounts.Account, error)
	ReadAccountStorage(address common.Address, key *common.Hash) ([]byte, error)
	ReadAccountCode(codeHash common.Hash) ([]byte, error)
	ReadAccountCodeSize(codeHash common.Hash) (int, error)
}

type StateWriter interface {
	UpdateAccountData(address common.Address, original, account *accounts.Account) error
	UpdateAccountCode(codeHash common.Hash, code []byte) error
	DeleteAccount(address common.Address, original *accounts.Account) error
	WriteAccountStorage(address common.Address, key, original, value *common.Hash) error
}

// keccakState wraps sha3.state. In addition to the usual hash methods, it also supports
// Read to get a variable amount of data from the hash state. Read is faster than Sum
// because it doesn't copy the internal state, but also modifies the internal state.
type keccakState interface {
	hash.Hash
	Read([]byte) (int, error)
}

type hasher struct {
	sha keccakState
}

var hasherPool = make(chan *hasher, 128)

func newHasher() *hasher {
	var h *hasher
	select {
	case h = <-hasherPool:
	default:
		h = &hasher{sha: sha3.NewLegacyKeccak256().(keccakState)}
	}
	return h
}

func returnHasherToPool(h *hasher) {
	select {
	case hasherPool <- h:
	default:
		fmt.Printf("Allowing hasher to be garbage collected, pool is full\n")
	}
}

type NoopWriter struct {
}

func NewNoopWriter() *NoopWriter {
	return &NoopWriter{}
}

func (nw *NoopWriter) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
	return nil
}

func (nw *NoopWriter) DeleteAccount(address common.Address, original *accounts.Account) error {
	return nil
}

func (nw *NoopWriter) UpdateAccountCode(codeHash common.Hash, code []byte) error {
	return nil
}

func (nw *NoopWriter) WriteAccountStorage(address common.Address, key, original, value *common.Hash) error {
	return nil
}

// Structure holding updates, deletes, and reads registered within one change period
// A change period can be transaction within a block, or a block within group of blocks
type Buffer struct {
	storageUpdates map[common.Address]map[common.Hash][]byte
	storageReads   map[common.Address]map[common.Hash]struct{}
	accountUpdates map[common.Hash]*accounts.Account
	accountReads   map[common.Hash]struct{}
	deleted        map[common.Address]struct{}
}

// Prepares buffer for work or clears previous data
func (b *Buffer) initialise() {
	b.storageUpdates = make(map[common.Address]map[common.Hash][]byte)
	b.storageReads = make(map[common.Address]map[common.Hash]struct{})
	b.accountUpdates = make(map[common.Hash]*accounts.Account)
	b.accountReads = make(map[common.Hash]struct{})
	b.deleted = make(map[common.Address]struct{})
}

// Replaces account pointer with pointers to the copies
func (b *Buffer) detachAccounts() {
	for addrHash, account := range b.accountUpdates {
		if account != nil {
			b.accountUpdates[addrHash] = &accounts.Account{
				Nonce:       account.Nonce,
				Balance:     new(big.Int).Set(account.Balance),
				Root:        account.Root,
				CodeHash:    account.CodeHash,
				StorageSize: account.StorageSize,
			}
		}
	}
}

// Merges the content of another buffer into this one
func (b *Buffer) merge(other *Buffer) {
	for address, om := range other.storageUpdates {
		m, ok := b.storageUpdates[address]
		if !ok {
			m = make(map[common.Hash][]byte)
			b.storageUpdates[address] = m
		}
		for keyHash, v := range om {
			m[keyHash] = v
		}
	}
	for address, om := range other.storageReads {
		m, ok := b.storageReads[address]
		if !ok {
			m = make(map[common.Hash]struct{})
			b.storageReads[address] = m
		}
		for keyHash := range om {
			m[keyHash] = struct{}{}
		}
	}
	for addrHash, account := range other.accountUpdates {
		b.accountUpdates[addrHash] = account
	}
	for addrHash := range other.accountReads {
		b.accountReads[addrHash] = struct{}{}
	}
	for address := range other.deleted {
		b.deleted[address] = struct{}{}
	}
}

// Implements StateReader by wrapping a trie and a database, where trie acts as a cache for the database
type TrieDbState struct {
	t                *trie.Trie
	db               ethdb.Database
	blockNr          uint64
	storageTries     map[common.Address]*trie.Trie
	buffers          []*Buffer
	aggregateBuffer  *Buffer // Merge of all buffers
	currentBuffer    *Buffer
	codeCache        *lru.Cache
	codeSizeCache    *lru.Cache
	historical       bool
	generationCounts map[uint64]int
	nodeCount        int
	oldestGeneration uint64
	noHistory        bool
	resolveReads     bool
	pg               *trie.ProofGenerator
}

func NewTrieDbState(root common.Hash, db ethdb.Database, blockNr uint64) (*TrieDbState, error) {
	csc, err := lru.New(100000)
	if err != nil {
		return nil, err
	}
	cc, err := lru.New(10000)
	if err != nil {
		return nil, err
	}
	t := trie.New(root, false)
	tds := TrieDbState{
		t:             t,
		db:            db,
		blockNr:       blockNr,
		storageTries:  make(map[common.Address]*trie.Trie),
		codeCache:     cc,
		codeSizeCache: csc,
		pg:            trie.NewProofGenerator(),
	}
	t.MakeListed(tds.joinGeneration, tds.leftGeneration)
	tds.generationCounts = make(map[uint64]int, 4096)
	tds.oldestGeneration = blockNr
	return &tds, nil
}

func (tds *TrieDbState) SetHistorical(h bool) {
	tds.historical = h
}

func (tds *TrieDbState) SetResolveReads(rr bool) {
	tds.resolveReads = rr
}

func (tds *TrieDbState) SetNoHistory(nh bool) {
	tds.noHistory = nh
}

func (tds *TrieDbState) Copy() *TrieDbState {
	tcopy := *tds.t
	cpy := TrieDbState{
		t:            &tcopy,
		db:           tds.db,
		blockNr:      tds.blockNr,
		storageTries: make(map[common.Address]*trie.Trie),
	}
	return &cpy
}

func (tds *TrieDbState) Database() ethdb.Database {
	return tds.db
}

func (tds *TrieDbState) AccountTrie() *trie.Trie {
	return tds.t
}

func (tds *TrieDbState) StartNewBuffer() {
	if tds.currentBuffer != nil {
		if tds.aggregateBuffer == nil {
			tds.aggregateBuffer = &Buffer{}
			tds.aggregateBuffer.initialise()
		}
		tds.aggregateBuffer.merge(tds.currentBuffer)
		tds.currentBuffer.detachAccounts()
	}
	tds.currentBuffer = &Buffer{}
	tds.currentBuffer.initialise()
	tds.buffers = append(tds.buffers, tds.currentBuffer)
}

func (tds *TrieDbState) LastRoot() common.Hash {
	return tds.t.Hash()
}

func (tds *TrieDbState) ComputeTrieRoots() ([]common.Hash, error) {
	roots, err := tds.computeTrieRoots(true)
	tds.clearUpdates()
	return roots, err
}

func (tds *TrieDbState) PrintTrie(w io.Writer) {
	tds.t.Print(w)
	for _, storageTrie := range tds.storageTries {
		storageTrie.Print(w)
	}
}

func (tds *TrieDbState) PrintStorageTrie(w io.Writer, address common.Address) {
	storageTrie := tds.storageTries[address]
	storageTrie.Print(w)
}

type Hashes []common.Hash

func (hashes Hashes) Len() int {
	return len(hashes)
}
func (hashes Hashes) Less(i, j int) bool {
	return bytes.Compare(hashes[i][:], hashes[j][:]) == -1
}
func (hashes Hashes) Swap(i, j int) {
	hashes[i], hashes[j] = hashes[j], hashes[i]
}

// Builds a map where for each address (of a smart contract) there is
// a sorted list of all key hashes that were touched within the
// period for which we are aggregating updates
func (tds *TrieDbState) buildStorageTouches() map[common.Address]Hashes {
	storageTouches := make(map[common.Address]Hashes)
	for address, m := range tds.aggregateBuffer.storageUpdates {
		var hashes Hashes
		mRead := tds.aggregateBuffer.storageReads[address]
		i := 0
		hashes = make(Hashes, len(m)+len(mRead))
		for keyHash := range m {
			hashes[i] = keyHash
			i++
		}
		for keyHash := range mRead {
			if _, ok := m[keyHash]; !ok {
				hashes[i] = keyHash
				i++
			}
		}
		if len(hashes) > 0 {
			sort.Sort(hashes)
			storageTouches[address] = hashes
		}
	}
	for address, m := range tds.aggregateBuffer.storageReads {
		if _, ok := tds.aggregateBuffer.storageUpdates[address]; ok {
			continue
		}
		hashes := make(Hashes, len(m))
		i := 0
		for keyHash := range m {
			hashes[i] = keyHash
			i++
		}
		sort.Sort(hashes)
		storageTouches[address] = hashes
	}
	return storageTouches
}

// Expands the storage tries (by loading data from the database) if it is required
// for accessing storage slots containing in the storageTouches map
func (tds *TrieDbState) resolveStorageTouches(storageTouches map[common.Address]Hashes) error {
	var resolver *trie.TrieResolver
	for address, hashes := range storageTouches {
		storageTrie, err := tds.getStorageTrie(address, true)
		if err != nil {
			return err
		}
		var contract = address // To avoid the value being overwritten, though still shared between continuations
		for _, keyHash := range hashes {
			if need, req := storageTrie.NeedResolution(contract[:], keyHash[:]); need {
				if resolver == nil {
					resolver = trie.NewResolver(false, false, tds.blockNr)
					resolver.SetHistorical(tds.historical)
				}
				resolver.AddRequest(req)
			}
		}
	}
	if resolver != nil {
		if err := resolver.ResolveWithDb(tds.db, tds.blockNr); err != nil {
			return err
		}
	}
	return nil
}

// Populate pending block proof so that it will be sufficient for accessing all storage slots in storageTouches
func (tds *TrieDbState) populateStorageBlockProof(storageTouches map[common.Address]Hashes) error {
	for address, hashes := range storageTouches {
		if _, ok := tds.aggregateBuffer.deleted[address]; ok && len(tds.aggregateBuffer.storageReads[address]) == 0 {
			// We can only skip the proof of storage entirely if
			// there were no reads before writes and account got deleted
			continue
		}
		storageTrie, err := tds.getStorageTrie(address, true)
		if err != nil {
			return err
		}
		var contract = address
		for _, keyHash := range hashes {
			storageTrie.PopulateBlockProofData(contract[:], keyHash[:], tds.pg)
		}
	}
	return nil
}

// Builds a sorted list of all adsresss hashes that were touched within the
// period for which we are aggregating updates
func (tds *TrieDbState) buildAccountTouches() Hashes {
	accountTouches := make(Hashes, len(tds.aggregateBuffer.accountUpdates)+len(tds.aggregateBuffer.accountReads))
	i := 0
	for addrHash := range tds.aggregateBuffer.accountUpdates {
		accountTouches[i] = addrHash
		i++
	}
	for addrHash := range tds.aggregateBuffer.accountReads {
		if _, ok := tds.aggregateBuffer.accountUpdates[addrHash]; !ok {
			accountTouches[i] = addrHash
			i++
		}
	}
	sort.Sort(accountTouches)
	return accountTouches
}

// Expands the accounts trie (by loading data from the database) if it is required
// for accessing accounts whose addresses are contained in the accountTouches
func (tds *TrieDbState) resolveAccountTouches(accountTouches Hashes) error {
	var resolver *trie.TrieResolver
	for _, addrHash := range accountTouches {
		if need, req := tds.t.NeedResolution(nil, addrHash[:]); need {
			if resolver == nil {
				resolver = trie.NewResolver(false, true, tds.blockNr)
				resolver.SetHistorical(tds.historical)
			}
			resolver.AddRequest(req)
		}
	}
	if resolver != nil {
		if err := resolver.ResolveWithDb(tds.db, tds.blockNr); err != nil {
			return err
		}
		resolver = nil
	}
	return nil
}

func (tds *TrieDbState) populateAccountBlockProof(accountTouches Hashes) {
	for _, addrHash := range accountTouches {
		tds.t.PopulateBlockProofData(nil, addrHash[:], tds.pg)
	}
}

func (tds *TrieDbState) computeTrieRoots(forward bool) ([]common.Hash, error) {
	fmt.Println("computeTrieRoots")
	// Aggregating the current buffer, if any
	if tds.currentBuffer != nil {
		if tds.aggregateBuffer == nil {
			tds.aggregateBuffer = &Buffer{}
			tds.aggregateBuffer.initialise()
		}
		tds.aggregateBuffer.merge(tds.currentBuffer)
	}
	if tds.aggregateBuffer == nil {
		return nil, nil
	}
	accountUpdates := tds.aggregateBuffer.accountUpdates
	//fmt.Println("accountUpdates")
	//spew.Dump(accountUpdates)

	// Prepare (resolve) storage tries so that actual modifications can proceed without database access
	storageTouches := tds.buildStorageTouches()
	//fmt.Println("storageTouches")
	//spew.Dump(storageTouches)

	if err := tds.resolveStorageTouches(storageTouches); err != nil {
		return nil, err
	}
	if tds.resolveReads {
		if err := tds.populateStorageBlockProof(storageTouches); err != nil {
			return nil, err
		}
	}

	// Prepare (resolve) accounts trie so that actual modifications can proceed without database access
	accountTouches := tds.buildAccountTouches()
	if err := tds.resolveAccountTouches(accountTouches); err != nil {
		return nil, err
	}
	if tds.resolveReads {
		tds.populateAccountBlockProof(accountTouches)
	}

	// Perform actual updates on the tries, and compute one trie root per buffer
	// These roots can be used to populate receipt.PostState on pre-Byzantium
	roots := make([]common.Hash, len(tds.buffers))
	for i, b := range tds.buffers {
		for address, m := range b.storageUpdates {
			addrHash, err := tds.HashAddress(&address, false /*save*/)
			if err != nil {
				return nil, err
			}
			if _, ok := b.deleted[address]; ok {
				if account, ok := b.accountUpdates[addrHash]; ok && account != nil {
					account.Root = emptyRoot
				}
				if account, ok := accountUpdates[addrHash]; ok && account != nil {
					account.Root = emptyRoot
				}
				storageTrie, err := tds.getStorageTrie(address, false)
				if err != nil {
					return nil, err
				}
				if storageTrie != nil {
					delete(tds.storageTries, address)
					storageTrie.PrepareToRemove()
				}
				continue
			}
			storageTrie, err := tds.getStorageTrie(address, true)
			//fmt.Println("storageTrie", m, address, storageTrie.Root())
			if err != nil {
				return nil, err
			}
			for keyHash, v := range m {
				if len(v) > 0 {
					storageTrie.Update(keyHash[:], v, tds.blockNr)
				} else {
					storageTrie.Delete(keyHash[:], tds.blockNr)
				}
			}
			if forward {
				if account, ok := b.accountUpdates[addrHash]; ok && account != nil {
					account.Root = storageTrie.Hash()
				}
				if account, ok := accountUpdates[addrHash]; ok && account != nil {
					account.Root = storageTrie.Hash()
				}
			}
		}
		//fmt.Println("b.accountUpdates ")
		//spew.Dump(b.accountUpdates)
		for addrHash, account := range b.accountUpdates {
			if account != nil {
				data, err := rlp.EncodeToBytes(account)
				if err != nil {
					return nil, err
				}
				tds.t.Update(addrHash[:], data, tds.blockNr)
			} else {
				tds.t.Delete(addrHash[:], tds.blockNr)
			}
		}
		roots[i] = tds.t.Hash()
	}
	return roots, nil
}

func (tds *TrieDbState) clearUpdates() {
	tds.buffers = nil
	tds.currentBuffer = nil
	tds.aggregateBuffer = nil
}

func (tds *TrieDbState) Rebuild() {
	tr := tds.AccountTrie()
	tr.Rebuild(tds.db, tds.blockNr)
}

func (tds *TrieDbState) SetBlockNr(blockNr uint64) {
	tds.blockNr = blockNr
}

func (tds *TrieDbState) UnwindTo(blockNr uint64) error {
	fmt.Printf("Rewinding from block %d to block %d\n", tds.blockNr, blockNr)
	var accountPutKeys [][]byte
	var accountPutVals [][]byte
	var accountDelKeys [][]byte
	var storagePutKeys [][]byte
	var storagePutVals [][]byte
	var storageDelKeys [][]byte
	tds.StartNewBuffer()
	b := tds.currentBuffer
	if err := tds.db.RewindData(tds.blockNr, blockNr, func(bucket, key, value []byte) error {
		var err error
		if bytes.Equal(bucket, AccountsHistoryBucket) {
			var addrHash common.Hash
			copy(addrHash[:], key)
			if len(value) > 0 {
				b.accountUpdates[addrHash], err = encodingToAccount(value)
				if err != nil {
					return err
				}
				accountPutKeys = append(accountPutKeys, key)
				accountPutVals = append(accountPutVals, value)
			} else {
				b.accountUpdates[addrHash] = nil
				accountDelKeys = append(accountDelKeys, key)
			}
		} else if bytes.Equal(bucket, StorageHistoryBucket) {
			var address common.Address
			copy(address[:], key[:20])
			var keyHash common.Hash
			copy(keyHash[:], key[20:])
			m, ok := b.storageUpdates[address]
			if !ok {
				m = make(map[common.Hash][]byte)
				b.storageUpdates[address] = m
			}
			m[keyHash] = value
			if len(value) > 0 {
				storagePutKeys = append(storagePutKeys, key)
				storagePutVals = append(storagePutVals, value)
			} else {
				//fmt.Printf("Deleted storage item\n")
				storageDelKeys = append(storageDelKeys, key)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := tds.computeTrieRoots(false); err != nil {
		return err
	}
	for addrHash, account := range tds.aggregateBuffer.accountUpdates {
		if account == nil {
			if err := tds.db.Delete(AccountsBucket, addrHash[:]); err != nil {
				return err
			}
		} else {
			value, err := accountToEncoding(account)
			if err != nil {
				return err
			}
			if err := tds.db.Put(AccountsBucket, addrHash[:], value); err != nil {
				return err
			}
		}
	}
	for address, m := range tds.aggregateBuffer.storageUpdates {
		for keyHash, value := range m {
			if len(value) == 0 {
				if err := tds.db.Delete(StorageBucket, append(address[:], keyHash[:]...)); err != nil {
					return err
				}
			} else {
				if err := tds.db.Put(StorageBucket, append(address[:], keyHash[:]...), value); err != nil {
					return err
				}
			}
		}
	}
	for i := tds.blockNr; i > blockNr; i-- {
		if err := tds.db.DeleteTimestamp(i); err != nil {
			return err
		}
	}
	tds.clearUpdates()
	tds.blockNr = blockNr
	return nil
}

// Account before EIP-2027
type Account struct {
	Nonce    uint64
	Balance  *big.Int
	Root     common.Hash
	CodeHash []byte
}

func accountToEncoding(account *accounts.Account) ([]byte, error) {
	var data []byte
	var err error
	if (account.CodeHash == nil || bytes.Equal(account.CodeHash, emptyCodeHash)) && (account.Root == emptyRoot || account.Root == common.Hash{}) {
		if (account.Balance == nil || account.Balance.Sign() == 0) && account.Nonce == 0 {
			data = []byte{byte(192)}
		} else {
			var extAccount accounts.ExtAccount
			extAccount.Nonce = account.Nonce
			extAccount.Balance = account.Balance
			if extAccount.Balance == nil {
				extAccount.Balance = new(big.Int)
			}
			data, err = rlp.EncodeToBytes(extAccount)
			if err != nil {
				return nil, err
			}
		}
	} else {
		a := *account
		if a.Balance == nil {
			a.Balance = new(big.Int)
		}
		if a.CodeHash == nil {
			a.CodeHash = emptyCodeHash
		}
		if a.Root == (common.Hash{}) {
			a.Root = emptyRoot
		}

		if a.StorageSize == nil || *a.StorageSize == 0 {
			accBeforeEIP2027 := &Account{
				Nonce:    a.Nonce,
				Balance:  a.Balance,
				Root:     a.Root,
				CodeHash: a.CodeHash,
			}

			data, err = rlp.EncodeToBytes(accBeforeEIP2027)
			if err != nil {
				return nil, err
			}

			fmt.Println("*** 1", string(data))
			data1, _ := rlp.EncodeToBytes(a)
			fmt.Println("*** 2", string(data1))
		} else {
			data, err = rlp.EncodeToBytes(a)
			if err != nil {
				return nil, err
			}
		}
	}
	return data, err
}

func encodingToAccount(enc []byte) (*accounts.Account, error) {
	if enc == nil || len(enc) == 0 {
		//fmt.Println("--- 1")
		return nil, nil
	}
	var data accounts.Account
	// Kind of hacky
	fmt.Println("--- 5", len(enc))
	if len(enc) == 1 {
		data.Balance = new(big.Int)
		data.CodeHash = emptyCodeHash
		data.Root = emptyRoot
	} else if len(enc) < 60 {
		//fixme возможно размер после добавления поля изменился. откуда взялась константа 60?
		var extData accounts.ExtAccount
		if err := rlp.DecodeBytes(enc, &extData); err != nil {
			fmt.Println("--- 6", err)
			return nil, err
		}
		data.Nonce = extData.Nonce
		data.Balance = extData.Balance
		data.CodeHash = emptyCodeHash
		data.Root = emptyRoot
	} else {
		var dataWithoutStorage Account
		if err := rlp.DecodeBytes(enc, &dataWithoutStorage); err != nil {
			if err.Error() != "rlp: input list has too many elements for state.Account" {
				fmt.Println("--- 7", err)
				return nil, err
			}

			var dataWithStorage accounts.Account
			if err := rlp.DecodeBytes(enc, &dataWithStorage); err != nil {
				fmt.Println("--- 8", err)
				return nil, err
			}

			data = dataWithStorage
		} else {
			data.Nonce = dataWithoutStorage.Nonce
			data.Balance = dataWithoutStorage.Balance
			data.CodeHash = dataWithoutStorage.CodeHash
			data.Root = dataWithoutStorage.Root
		}
	}

	fmt.Println("--- 9", data)
	return &data, nil
}

func (tds *TrieDbState) joinGeneration(gen uint64) {
	tds.nodeCount++
	tds.generationCounts[gen]++

}

func (tds *TrieDbState) leftGeneration(gen uint64) {
	tds.nodeCount--
	tds.generationCounts[gen]--
}

func (tds *TrieDbState) ReadAccountData(address common.Address) (*accounts.Account, error) {
	h := newHasher()
	defer returnHasherToPool(h)
	h.sha.Reset()
	h.sha.Write(address[:])
	var buf common.Hash
	h.sha.Read(buf[:])
	if tds.resolveReads {
		if _, ok := tds.currentBuffer.accountUpdates[buf]; !ok {
			tds.currentBuffer.accountReads[buf] = struct{}{}
		}
	}
	enc, ok := tds.t.Get(buf[:], tds.blockNr)
	if !ok {
		// Not present in the trie, try the database
		var err error
		if tds.historical {
			enc, err = tds.db.GetAsOf(AccountsBucket, AccountsHistoryBucket, buf[:], tds.blockNr)
			if err != nil {
				enc = nil
			}
		} else {
			enc, err = tds.db.Get(AccountsBucket, buf[:])
			if err != nil {
				enc = nil
			}
		}
	}
	return encodingToAccount(enc)
}

func (tds *TrieDbState) savePreimage(save bool, hash, preimage []byte) error {
	if !save {
		return nil
	}
	return tds.db.Put(trie.SecureKeyPrefix, hash, preimage)
}

func (tds *TrieDbState) HashAddress(address *common.Address, save bool) (common.Hash, error) {
	h := newHasher()
	defer returnHasherToPool(h)
	h.sha.Reset()
	h.sha.Write(address[:])
	var buf common.Hash
	h.sha.Read(buf[:])
	return buf, tds.savePreimage(save, buf[:], address[:])
}

func (tds *TrieDbState) HashKey(key *common.Hash, save bool) (common.Hash, error) {
	h := newHasher()
	defer returnHasherToPool(h)
	h.sha.Reset()
	h.sha.Write(key[:])
	var buf common.Hash
	h.sha.Read(buf[:])
	return buf, tds.savePreimage(save, buf[:], key[:])
}

func (tds *TrieDbState) GetKey(shaKey []byte) []byte {
	key, _ := tds.db.Get(trie.SecureKeyPrefix, shaKey)
	return key
}

func (tds *TrieDbState) getStorageTrie(address common.Address, create bool) (*trie.Trie, error) {
	t, ok := tds.storageTries[address]
	if !ok && create {
		account, err := tds.ReadAccountData(address)
		if err != nil {
			return nil, err
		}
		if account == nil {
			t = trie.New(common.Hash{}, true)
		} else {
			t = trie.New(account.Root, true)
		}
		t.MakeListed(tds.joinGeneration, tds.leftGeneration)
		tds.storageTries[address] = t
	}
	return t, nil
}

func (tds *TrieDbState) ReadAccountStorage(address common.Address, key *common.Hash) ([]byte, error) {
	t, err := tds.getStorageTrie(address, true)
	if err != nil {
		return nil, err
	}
	seckey, err := tds.HashKey(key, false /*save*/)
	if err != nil {
		return nil, err
	}
	if tds.resolveReads {
		var addReadRecord = false
		if mWrite, ok := tds.currentBuffer.storageUpdates[address]; ok {
			if _, ok1 := mWrite[seckey]; !ok1 {
				addReadRecord = true
			}
		} else {
			addReadRecord = true
		}
		if addReadRecord {
			m, ok := tds.currentBuffer.storageReads[address]
			if !ok {
				m = make(map[common.Hash]struct{})
				tds.currentBuffer.storageReads[address] = m
			}
			m[seckey] = struct{}{}
		}
	}
	enc, ok := t.Get(seckey[:], tds.blockNr)
	if !ok {
		// Not present in the trie, try database
		cKey := make([]byte, len(address)+len(seckey))
		copy(cKey, address[:])
		copy(cKey[len(address):], seckey[:])
		if tds.historical {
			enc, err = tds.db.GetAsOf(StorageBucket, StorageHistoryBucket, cKey, tds.blockNr)
			if err != nil {
				enc = nil
			}
		} else {
			enc, err = tds.db.Get(StorageBucket, cKey)
			if err != nil {
				enc = nil
			}
		}
	}
	return enc, nil
}

func (tds *TrieDbState) ReadAccountCode(codeHash common.Hash) (code []byte, err error) {
	if bytes.Equal(codeHash[:], emptyCodeHash) {
		return nil, nil
	}
	if cached, ok := tds.codeCache.Get(codeHash); ok {
		code, err = cached.([]byte), nil
	} else {
		code, err = tds.db.Get(CodeBucket, codeHash[:])
		if err == nil {
			tds.codeSizeCache.Add(codeHash, len(code))
			tds.codeCache.Add(codeHash, code)
		}
	}
	if tds.resolveReads {
		tds.pg.ReadCode(codeHash, code)
	}
	return code, err
}

func (tds *TrieDbState) ReadAccountCodeSize(codeHash common.Hash) (codeSize int, err error) {
	var code []byte
	if cached, ok := tds.codeSizeCache.Get(codeHash); ok {
		codeSize, err = cached.(int), nil
		if tds.resolveReads {
			if cachedCode, ok := tds.codeCache.Get(codeHash); ok {
				code, err = cachedCode.([]byte), nil
			} else {
				code, err = tds.ReadAccountCode(codeHash)
				if err != nil {
					return 0, err
				}
			}
		}
	} else {
		code, err = tds.ReadAccountCode(codeHash)
		if err != nil {
			return 0, err
		}
		codeSize = len(code)
	}
	if tds.resolveReads {
		tds.pg.ReadCode(codeHash, code)
	}
	return codeSize, nil
}

var prevMemStats runtime.MemStats

func (tds *TrieDbState) PruneTries(print bool) {
	if tds.nodeCount > int(MaxTrieCacheGen) {
		toRemove := 0
		excess := tds.nodeCount - int(MaxTrieCacheGen)
		gen := tds.oldestGeneration
		for excess > 0 {
			excess -= tds.generationCounts[gen]
			toRemove += tds.generationCounts[gen]
			delete(tds.generationCounts, gen)
			gen++
		}
		// Unload all nodes with touch timestamp < gen
		for address, storageTrie := range tds.storageTries {
			empty := storageTrie.UnloadOlderThan(gen, false)
			if empty {
				delete(tds.storageTries, address)
			}
		}
		tds.t.UnloadOlderThan(gen, false)
		tds.oldestGeneration = gen
		tds.nodeCount -= toRemove
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.Info("Memory", "nodes", tds.nodeCount, "alloc", int(m.Alloc/1024), "sys", int(m.Sys/1024), "numGC", int(m.NumGC))
		if print {
			fmt.Printf("Pruning done. Nodes: %d, alloc: %d, sys: %d, numGC: %d\n", tds.nodeCount, int(m.Alloc/1024), int(m.Sys/1024), int(m.NumGC))
		}
	}
}

type TrieStateWriter struct {
	tds *TrieDbState
}

type DbStateWriter struct {
	tds *TrieDbState
}

func (tds *TrieDbState) TrieStateWriter() *TrieStateWriter {
	return &TrieStateWriter{tds: tds}
}

func (tds *TrieDbState) DbStateWriter() *DbStateWriter {
	return &DbStateWriter{tds: tds}
}

var emptyRoot = common.HexToHash("56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

func accountsEqual(a1, a2 *accounts.Account) bool {
	if a1.Nonce != a2.Nonce {
		return false
	}
	if a1.Balance == nil {
		if a2.Balance != nil {
			return false
		}
	} else if a2.Balance == nil {
		return false
	} else if a1.Balance.Cmp(a2.Balance) != 0 {
		return false
	}
	if a1.Root != a2.Root {
		return false
	}
	if a1.CodeHash == nil {
		if a2.CodeHash != nil {
			return false
		}
	} else if a2.CodeHash == nil {
		return false
	} else if !bytes.Equal(a1.CodeHash, a2.CodeHash) {
		return false
	}
	return true
}

func (tsw *TrieStateWriter) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
	addrHash, err := tsw.tds.HashAddress(&address, false /*save*/)
	if err != nil {
		return err
	}
	tsw.tds.currentBuffer.accountUpdates[addrHash] = account
	return nil
}

func (dsw *DbStateWriter) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
	data, err := accountToEncoding(account)
	if err != nil {
		return err
	}
	addrHash, err := dsw.tds.HashAddress(&address, true /*save*/)
	if err != nil {
		return err
	}
	if err = dsw.tds.db.Put(AccountsBucket, addrHash[:], data); err != nil {
		return err
	}
	if dsw.tds.noHistory {
		return nil
	}
	// Don't write historical record if the account did not change
	if accountsEqual(original, account) {
		return nil
	}
	var originalData []byte
	if original.Balance == nil {
		originalData = []byte{}
	} else {
		originalData, err = accountToEncoding(original)
		if err != nil {
			return err
		}
	}
	return dsw.tds.db.PutS(AccountsHistoryBucket, addrHash[:], originalData, dsw.tds.blockNr)
}

func (tsw *TrieStateWriter) DeleteAccount(address common.Address, original *accounts.Account) error {
	addrHash, err := tsw.tds.HashAddress(&address, false /*save*/)
	if err != err {
		return err
	}
	tsw.tds.currentBuffer.accountUpdates[addrHash] = nil
	tsw.tds.currentBuffer.deleted[address] = struct{}{}
	return nil
}

func (dsw *DbStateWriter) DeleteAccount(address common.Address, original *accounts.Account) error {
	addrHash, err := dsw.tds.HashAddress(&address, true /*save*/)
	if err != nil {
		return err
	}
	if err := dsw.tds.db.Delete(AccountsBucket, addrHash[:]); err != nil {
		return err
	}
	if dsw.tds.noHistory {
		return nil
	}
	var originalData []byte
	if original.Balance == nil {
		// Account has been created and deleted in the same block
		originalData = []byte{}
	} else {
		originalData, err = accountToEncoding(original)
		if err != nil {
			return err
		}
	}
	return dsw.tds.db.PutS(AccountsHistoryBucket, addrHash[:], originalData, dsw.tds.blockNr)
}

func (tsw *TrieStateWriter) UpdateAccountCode(codeHash common.Hash, code []byte) error {
	if tsw.tds.resolveReads {
		tsw.tds.pg.CreateCode(codeHash, code)
	}
	return nil
}

func (dsw *DbStateWriter) UpdateAccountCode(codeHash common.Hash, code []byte) error {
	if dsw.tds.resolveReads {
		dsw.tds.pg.CreateCode(codeHash, code)
	}
	return dsw.tds.db.Put(CodeBucket, codeHash[:], code)
}

func (tsw *TrieStateWriter) WriteAccountStorage(address common.Address, key, original, value *common.Hash) error {
	v := bytes.TrimLeft(value[:], "\x00")
	m, ok := tsw.tds.currentBuffer.storageUpdates[address]
	if !ok {
		m = make(map[common.Hash][]byte)
		tsw.tds.currentBuffer.storageUpdates[address] = m
	}
	seckey, err := tsw.tds.HashKey(key, false /*save*/)
	if err != nil {
		return err
	}
	if len(v) > 0 {
		m[seckey] = common.CopyBytes(v)
	} else {
		m[seckey] = nil
	}
	return nil
}

func (dsw *DbStateWriter) WriteAccountStorage(address common.Address, key, original, value *common.Hash) error {
	if *original == *value {
		return nil
	}
	seckey, err := dsw.tds.HashKey(key, true /*save*/)
	if err != nil {
		return err
	}
	v := bytes.TrimLeft(value[:], "\x00")
	vv := make([]byte, len(v))
	copy(vv, v)
	compositeKey := append(address[:], seckey[:]...)
	if len(v) == 0 {
		err = dsw.tds.db.Delete(StorageBucket, compositeKey)
	} else {
		err = dsw.tds.db.Put(StorageBucket, compositeKey, vv)
	}
	if err != nil {
		return err
	}
	if dsw.tds.noHistory {
		return nil
	}
	o := bytes.TrimLeft(original[:], "\x00")
	oo := make([]byte, len(o))
	copy(oo, o)
	return dsw.tds.db.PutS(StorageHistoryBucket, compositeKey, oo, dsw.tds.blockNr)
}

func (tds *TrieDbState) ExtractProofs(trace bool) trie.BlockProof {
	return tds.pg.ExtractProofs(trace)
}
