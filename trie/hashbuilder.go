package trie

import (
	"bytes"
	"fmt"
	"io"
	"math/bits"

	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/crypto"
	"github.com/ledgerwatch/turbo-geth/rlp"
	"github.com/ledgerwatch/turbo-geth/trie/rlphacks"
)

const hashStackStride = common.HashLength + 1 // + 1 byte for RLP encoding
var EmptyCodeHash = crypto.Keccak256Hash(nil)

// HashBuilder implements the interface `structInfoReceiver` and opcodes that the structural information of the trie
// is comprised of
// DESCRIBED: docs/programmers_guide/guide.md#separation-of-keys-and-the-structure
type HashBuilder struct {
	byteArrayWriter *ByteArrayWriter

	hashStack    []byte                // Stack of sub-slices, each 33 bytes each, containing RLP encodings of node hashes (or of nodes themselves, if shorter than 32 bytes)
	nodeStack    []node                // Stack of nodes
	dataLenStack []uint64              // Stack of witness length. It's very similar to hashStack, just calculating another kind of hash
	acc          accounts.Account      // Working account instance (to avoid extra allocations)
	sha          keccakState           // Keccak primitive that can absorb data (Write), and get squeezed to the hash out (Read)
	hashBuf      [hashStackStride]byte // RLP representation of hash (or un-hashes value)
	keyPrefix    [1]byte
	lenPrefix    [4]byte
	valBuf       [128]byte // Enough to accomodate hash encoding of any account
	b            [1]byte   // Buffer for single byte
	prefixBuf    [8]byte
	trace        bool // Set to true when HashBuilder is required to print trace information for diagnostics
}

// NewHashBuilder creates a new HashBuilder
func NewHashBuilder(trace bool) *HashBuilder {
	return &HashBuilder{
		sha:             sha3.NewLegacyKeccak256().(keccakState),
		byteArrayWriter: &ByteArrayWriter{},
		trace:           trace,
	}
}

// Reset makes the HashBuilder suitable for reuse
func (hb *HashBuilder) Reset() {
	if len(hb.hashStack) > 0 {
		hb.hashStack = hb.hashStack[:0]
	}
	if len(hb.nodeStack) > 0 {
		hb.nodeStack = hb.nodeStack[:0]
	}
	if len(hb.dataLenStack) > 0 {
		hb.dataLenStack = hb.dataLenStack[:0]
	}
}

func (hb *HashBuilder) leaf(length int, keyHex []byte, val rlphacks.RlpSerializable) error {
	if hb.trace {
		fmt.Printf("LEAF %d\n", length)
	}
	if length < 0 {
		return fmt.Errorf("length %d", length)
	}
	key := keyHex[len(keyHex)-length:]
	s := &shortNode{Key: common.CopyBytes(key), Val: valueNode(common.CopyBytes(val.RawBytes()))}
	hb.nodeStack = append(hb.nodeStack, s)
	if err := hb.leafHashWithKeyVal(key, val); err != nil {
		return err
	}
	copy(s.ref.data[:], hb.hashStack[len(hb.hashStack)-common.HashLength:])
	s.ref.len = hb.hashStack[len(hb.hashStack)-common.HashLength-1] - 0x80
	if s.ref.len > 32 {
		s.ref.len = hb.hashStack[len(hb.hashStack)-common.HashLength-1] - 0xc0 + 1
		copy(s.ref.data[:], hb.hashStack[len(hb.hashStack)-common.HashLength-1:])
	}
	s.witnessLength = hb.dataLenStack[len(hb.dataLenStack)-1]
	if hb.trace {
		fmt.Printf("Stack depth: %d, %d\n", len(hb.nodeStack), len(hb.dataLenStack))

	}
	return nil
}

// To be called internally
func (hb *HashBuilder) leafHashWithKeyVal(key []byte, val rlphacks.RlpSerializable) error {
	// Compute the total length of binary representation
	var kp, kl int
	// Write key
	var compactLen int
	var ni int
	var compact0 byte
	if hasTerm(key) {
		compactLen = (len(key)-1)/2 + 1
		if len(key)&1 == 0 {
			compact0 = 0x30 + key[0] // Odd: (3<<4) + first nibble
			ni = 1
		} else {
			compact0 = 0x20
		}
	} else {
		compactLen = len(key)/2 + 1
		if len(key)&1 == 1 {
			compact0 = 0x10 + key[0] // Odd: (1<<4) + first nibble
			ni = 1
		}
	}
	if compactLen > 1 {
		hb.keyPrefix[0] = 0x80 + byte(compactLen)
		kp = 1
		kl = compactLen
	} else {
		kl = 1
	}

	err := hb.completeLeafHash(kp, kl, compactLen, key, compact0, ni, val)
	if err != nil {
		return err
	}
	hb.dataLenStack = append(hb.dataLenStack, uint64(len(val.RawBytes()))+1+uint64(len(key))/2) // + node opcode + len(key)/2

	hb.hashStack = append(hb.hashStack, hb.hashBuf[:]...)
	if len(hb.hashStack) > hashStackStride*len(hb.nodeStack) {
		hb.nodeStack = append(hb.nodeStack, nil)
	}
	return nil
}

func (hb *HashBuilder) completeLeafHash(kp, kl, compactLen int, key []byte, compact0 byte, ni int, val rlphacks.RlpSerializable) error {
	totalLen := kp + kl + val.DoubleRLPLen()
	pt := rlphacks.GenerateStructLen(hb.lenPrefix[:], totalLen)

	var writer io.Writer
	var reader io.Reader

	if totalLen+pt < common.HashLength {
		// Embedded node
		hb.byteArrayWriter.Setup(hb.hashBuf[:], 0)
		writer = hb.byteArrayWriter
	} else {
		hb.sha.Reset()
		writer = hb.sha
		reader = hb.sha
	}

	if _, err := writer.Write(hb.lenPrefix[:pt]); err != nil {
		return err
	}
	if _, err := writer.Write(hb.keyPrefix[:kp]); err != nil {
		return err
	}
	hb.b[0] = compact0
	if _, err := writer.Write(hb.b[:]); err != nil {
		return err
	}
	for i := 1; i < compactLen; i++ {
		hb.b[0] = key[ni]*16 + key[ni+1]
		if _, err := writer.Write(hb.b[:]); err != nil {
			return err
		}
		ni += 2
	}

	if err := val.ToDoubleRLP(writer, hb.prefixBuf[:]); err != nil {
		return err
	}

	if reader != nil {
		hb.hashBuf[0] = 0x80 + common.HashLength
		if _, err := reader.Read(hb.hashBuf[1:]); err != nil {
			return err
		}
	}

	return nil
}

func (hb *HashBuilder) leafHash(length int, keyHex []byte, val rlphacks.RlpSerializable) error {
	if hb.trace {
		fmt.Printf("LEAFHASH %d\n", length)
	}
	if length < 0 {
		return fmt.Errorf("length %d", length)
	}
	key := keyHex[len(keyHex)-length:]
	return hb.leafHashWithKeyVal(key, val)
}

func (hb *HashBuilder) accountLeaf(length int, keyHex []byte, balance *uint256.Int, nonce uint64, incarnation uint64, fieldSet uint32) (err error) {
	if hb.trace {
		fmt.Printf("ACCOUNTLEAF %d (%b)\n", length, fieldSet)
	}
	key := keyHex[len(keyHex)-length:]
	copy(hb.acc.Root[:], EmptyRoot[:])
	copy(hb.acc.CodeHash[:], EmptyCodeHash[:])
	hb.acc.Nonce = nonce
	hb.acc.Balance.Set(balance)
	hb.acc.Initialised = true
	hb.acc.Incarnation = incarnation

	popped := 0
	var root node
	if fieldSet&uint32(4) != 0 {
		copy(hb.acc.Root[:], hb.hashStack[len(hb.hashStack)-popped*hashStackStride-common.HashLength:len(hb.hashStack)-popped*hashStackStride])
		if hb.acc.Root != EmptyRoot {
			// Root is on top of the stack
			root = hb.nodeStack[len(hb.nodeStack)-popped-1]
			l := hb.dataLenStack[len(hb.dataLenStack)-popped-1]
			if root == nil {
				root = hashNode{hash: common.CopyBytes(hb.acc.Root[:]), witnessLength: l}
			}
		}
		popped++
	}
	var accountCode codeNode
	if fieldSet&uint32(8) != 0 {
		copy(hb.acc.CodeHash[:], hb.hashStack[len(hb.hashStack)-popped*hashStackStride-common.HashLength:len(hb.hashStack)-popped*hashStackStride])
		ok := false
		if !bytes.Equal(hb.acc.CodeHash[:], EmptyCodeHash[:]) {
			stackTop := hb.nodeStack[len(hb.nodeStack)-popped-1]
			if stackTop != nil { // if we don't have any stack top it might be okay because we didn't resolve the code yet (stateful resolver)
				// but if we have something on top of the stack that isn't `nil`, it has to be a codeNode
				accountCode, ok = stackTop.(codeNode)
				if !ok {
					return fmt.Errorf("unexpected node type on the node stack, wanted codeNode, got %t:%s", stackTop, stackTop)
				}
			}
		}
		popped++
	}
	var accCopy accounts.Account
	accCopy.Copy(&hb.acc)

	accountCodeSize := codeSizeUncached
	if !bytes.Equal(accCopy.CodeHash[:], EmptyCodeHash[:]) && accountCode != nil {
		accountCodeSize = len(accountCode)
	}

	a := &accountNode{accCopy, root, true, accountCode, accountCodeSize}
	s := &shortNode{Key: common.CopyBytes(key), Val: a}
	// this invocation will take care of the popping given number of items from both hash stack and node stack,
	// pushing resulting hash to the hash stack, and nil to the node stack
	if err = hb.accountLeafHashWithKey(key, popped); err != nil {
		return err
	}
	copy(s.ref.data[:], hb.hashStack[len(hb.hashStack)-common.HashLength:])
	s.ref.len = 32
	s.witnessLength = hb.dataLenStack[len(hb.dataLenStack)-1]
	// Replace top of the stack
	hb.nodeStack[len(hb.nodeStack)-1] = s
	if hb.trace {
		fmt.Printf("Stack depth: %d, %d\n", len(hb.nodeStack), len(hb.dataLenStack))

	}
	return nil
}

func (hb *HashBuilder) accountLeafHash(length int, keyHex []byte, balance *uint256.Int, nonce uint64, incarnation uint64, fieldSet uint32) (err error) {
	if hb.trace {
		fmt.Printf("ACCOUNTLEAFHASH %d (%b)\n", length, fieldSet)
	}
	key := keyHex[len(keyHex)-length:]
	hb.acc.Nonce = nonce
	hb.acc.Balance.Set(balance)
	hb.acc.Initialised = true
	hb.acc.Incarnation = incarnation

	popped := 0
	if fieldSet&AccountFieldStorageOnly != 0 {
		copy(hb.acc.Root[:], hb.hashStack[len(hb.hashStack)-popped*hashStackStride-common.HashLength:len(hb.hashStack)-popped*hashStackStride])
		popped++
	} else {
		copy(hb.acc.Root[:], EmptyRoot[:])
	}

	if fieldSet&AccountFieldCodeOnly != 0 {
		copy(hb.acc.CodeHash[:], hb.hashStack[len(hb.hashStack)-popped*hashStackStride-common.HashLength:len(hb.hashStack)-popped*hashStackStride])
		popped++
	} else {
		copy(hb.acc.CodeHash[:], EmptyCodeHash[:])
	}

	return hb.accountLeafHashWithKey(key, popped)
}

// To be called internally
func (hb *HashBuilder) accountLeafHashWithKey(key []byte, popped int) error {
	// Compute the total length of binary representation
	var kp, kl int
	// Write key
	var compactLen int
	var ni int
	var compact0 byte
	if hasTerm(key) {
		compactLen = (len(key)-1)/2 + 1
		if len(key)&1 == 0 {
			compact0 = 48 + key[0] // Odd (1<<4) + first nibble
			ni = 1
		} else {
			compact0 = 32
		}
	} else {
		compactLen = len(key)/2 + 1
		if len(key)&1 == 1 {
			compact0 = 16 + key[0] // Odd (1<<4) + first nibble
			ni = 1
		}
	}
	if compactLen > 1 {
		hb.keyPrefix[0] = byte(128 + compactLen)
		kp = 1
		kl = compactLen
	} else {
		kl = 1
	}
	valLen := hb.acc.EncodingLengthForHashing()
	hb.acc.EncodeForHashing(hb.valBuf[:])
	val := rlphacks.RlpEncodedBytes(hb.valBuf[:valLen])

	err := hb.completeLeafHash(kp, kl, compactLen, key, compact0, ni, val)
	if err != nil {
		return err
	}
	dataLen := 1 + uint64(hb.acc.EncodingLengthForStorage()) // + opcode + account data len
	dataLen += 1 + uint64(len(key))/2                        // + opcode + len(key)/2

	if popped > 0 {
		hb.hashStack = hb.hashStack[:len(hb.hashStack)-popped*hashStackStride]
		hb.nodeStack = hb.nodeStack[:len(hb.nodeStack)-popped]
		for _, l := range hb.dataLenStack[len(hb.dataLenStack)-popped:] { // + storage size + code size
			dataLen += l
		}
		hb.dataLenStack = hb.dataLenStack[:len(hb.dataLenStack)-popped]
	}
	hb.hashStack = append(hb.hashStack, hb.hashBuf[:]...)
	hb.nodeStack = append(hb.nodeStack, nil)
	hb.dataLenStack = append(hb.dataLenStack, dataLen)
	if hb.trace {
		fmt.Printf("Stack depth: %d, %d\n", len(hb.nodeStack), len(hb.dataLenStack))
	}
	return nil
}

func (hb *HashBuilder) extension(key []byte) error {
	if hb.trace {
		fmt.Printf("EXTENSION %x\n", key)
	}
	nd := hb.nodeStack[len(hb.nodeStack)-1]
	var s *shortNode
	switch n := nd.(type) {
	case nil:
		branchHash := common.CopyBytes(hb.hashStack[len(hb.hashStack)-common.HashLength:])
		dataLen := hb.dataLenStack[len(hb.dataLenStack)-1]
		s = &shortNode{Key: common.CopyBytes(key), Val: hashNode{hash: branchHash, witnessLength: dataLen}}
	case *fullNode:
		s = &shortNode{Key: common.CopyBytes(key), Val: n}
	default:
		return fmt.Errorf("wrong Val type for an extension: %T", nd)
	}
	hb.nodeStack[len(hb.nodeStack)-1] = s
	if err := hb.extensionHash(key); err != nil {
		return err
	}
	copy(s.ref.data[:], hb.hashStack[len(hb.hashStack)-common.HashLength:])
	s.ref.len = 32
	s.witnessLength = hb.dataLenStack[len(hb.dataLenStack)-1]
	if hb.trace {
		fmt.Printf("Stack depth: %d, %d\n", len(hb.nodeStack), len(hb.dataLenStack))
	}
	return nil
}

func (hb *HashBuilder) extensionHash(key []byte) error {
	if hb.trace {
		fmt.Printf("EXTENSIONHASH %x\n", key)
	}
	branchHash := hb.hashStack[len(hb.hashStack)-hashStackStride:]
	// Compute the total length of binary representation
	var kp, kl int
	// Write key
	var compactLen int
	var ni int
	var compact0 byte
	// https://github.com/ethereum/wiki/wiki/Patricia-Tree#specification-compact-encoding-of-hex-sequence-with-optional-terminator
	if hasTerm(key) {
		compactLen = (len(key)-1)/2 + 1
		if len(key)&1 == 0 {
			compact0 = 0x30 + key[0] // Odd: (3<<4) + first nibble
			ni = 1
		} else {
			compact0 = 0x20
		}
	} else {
		compactLen = len(key)/2 + 1
		if len(key)&1 == 1 {
			compact0 = 0x10 + key[0] // Odd: (1<<4) + first nibble
			ni = 1
		}
	}
	if compactLen > 1 {
		hb.keyPrefix[0] = 0x80 + byte(compactLen)
		kp = 1
		kl = compactLen
	} else {
		kl = 1
	}
	totalLen := kp + kl + 33
	pt := rlphacks.GenerateStructLen(hb.lenPrefix[:], totalLen)
	hb.sha.Reset()
	if _, err := hb.sha.Write(hb.lenPrefix[:pt]); err != nil {
		return err
	}
	if _, err := hb.sha.Write(hb.keyPrefix[:kp]); err != nil {
		return err
	}
	hb.b[0] = compact0
	if _, err := hb.sha.Write(hb.b[:]); err != nil {
		return err
	}
	for i := 1; i < compactLen; i++ {
		hb.b[0] = key[ni]*16 + key[ni+1]
		if _, err := hb.sha.Write(hb.b[:]); err != nil {
			return err
		}
		ni += 2
	}
	if _, err := hb.sha.Write(branchHash[:common.HashLength+1]); err != nil {
		return err
	}
	// Replace previous hash with the new one
	if _, err := hb.sha.Read(hb.hashStack[len(hb.hashStack)-common.HashLength:]); err != nil {
		return err
	}
	hb.hashStack[len(hb.hashStack)-hashStackStride] = 0x80 + common.HashLength
	hb.dataLenStack[len(hb.dataLenStack)-1] = 1 + uint64(len(key))/2 + hb.dataLenStack[len(hb.dataLenStack)-1] // + opcode + len(key)/2 + childrenWitnessLen
	if _, ok := hb.nodeStack[len(hb.nodeStack)-1].(*fullNode); ok {
		return fmt.Errorf("extensionHash cannot be emitted when a node is on top of the stack")
	}
	return nil
}

func (hb *HashBuilder) branch(set uint16) error {
	if hb.trace {
		fmt.Printf("BRANCH (%b)\n", set)
	}
	if hb.trace {
		fmt.Printf("Stack depth: %d, %d\n", len(hb.nodeStack), len(hb.dataLenStack))
	}
	f := &fullNode{}
	digits := bits.OnesCount16(set)
	if len(hb.nodeStack) < digits {
		return fmt.Errorf("len(hb.nodeStask) %d < digits %d", len(hb.nodeStack), digits)
	}
	nodes := hb.nodeStack[len(hb.nodeStack)-digits:]
	hashes := hb.hashStack[len(hb.hashStack)-hashStackStride*digits:]
	dataLengths := hb.dataLenStack[len(hb.dataLenStack)-digits:]
	var i int
	for digit := uint(0); digit < 16; digit++ {
		if ((uint16(1) << digit) & set) != 0 {
			if nodes[i] == nil {
				f.Children[digit] = hashNode{hash: common.CopyBytes(hashes[hashStackStride*i+1 : hashStackStride*(i+1)]), witnessLength: dataLengths[i]}
			} else {
				f.Children[digit] = nodes[i]
			}
			i++
		}
	}
	hb.nodeStack = hb.nodeStack[:len(hb.nodeStack)-digits+1]
	hb.nodeStack[len(hb.nodeStack)-1] = f
	if err := hb.branchHash(set); err != nil {
		return err
	}
	copy(f.ref.data[:], hb.hashStack[len(hb.hashStack)-common.HashLength:])
	f.ref.len = 32
	f.witnessLength = hb.dataLenStack[len(hb.dataLenStack)-1]
	if hb.trace {
		fmt.Printf("Stack depth: %d, %d\n", len(hb.nodeStack), len(hb.dataLenStack))
	}

	return nil
}

func (hb *HashBuilder) branchHash(set uint16) error {
	if hb.trace {
		fmt.Printf("BRANCHHASH (%b)\n", set)
	}
	digits := bits.OnesCount16(set)
	if len(hb.hashStack) < hashStackStride*digits {
		return fmt.Errorf("len(hb.hashStack) %d < hashStackStride*digits %d", len(hb.hashStack), hashStackStride*digits)
	}
	hashes := hb.hashStack[len(hb.hashStack)-hashStackStride*digits:]
	// Calculate the size of the resulting RLP
	totalSize := 17 // These are 17 length prefixes
	var i int
	for digit := uint(0); digit < 16; digit++ {
		if ((uint16(1) << digit) & set) != 0 {
			if hashes[hashStackStride*i] == 0x80+common.HashLength {
				totalSize += common.HashLength
			} else {
				// Embedded node
				totalSize += int(hashes[hashStackStride*i] - rlp.EmptyListCode)
			}
			i++
		}
	}
	hb.sha.Reset()
	pt := rlphacks.GenerateStructLen(hb.lenPrefix[:], totalSize)
	if _, err := hb.sha.Write(hb.lenPrefix[:pt]); err != nil {
		return err
	}
	// Output children hashes or embedded RLPs
	i = 0
	hb.b[0] = rlp.EmptyStringCode
	for digit := uint(0); digit < 17; digit++ {
		if ((uint16(1) << digit) & set) != 0 {
			if hashes[hashStackStride*i] == byte(0x80+common.HashLength) {
				if _, err := hb.sha.Write(hashes[hashStackStride*i : hashStackStride*i+hashStackStride]); err != nil {
					return err
				}
			} else {
				// Embedded node
				size := int(hashes[hashStackStride*i]) - rlp.EmptyListCode
				if _, err := hb.sha.Write(hashes[hashStackStride*i : hashStackStride*i+size+1]); err != nil {
					return err
				}
			}
			i++
		} else {
			if _, err := hb.sha.Write(hb.b[:]); err != nil {
				return err
			}
		}
	}
	hb.hashStack = hb.hashStack[:len(hb.hashStack)-hashStackStride*digits+hashStackStride]
	hb.hashStack[len(hb.hashStack)-hashStackStride] = 0x80 + common.HashLength
	if _, err := hb.sha.Read(hb.hashStack[len(hb.hashStack)-common.HashLength:]); err != nil {
		return err
	}
	dataLen := uint64(1 + 1) // fullNode: opcode + mask + childrenWitnessLen
	for _, l := range hb.dataLenStack[len(hb.dataLenStack)-digits:] {
		dataLen += l
	}
	hb.dataLenStack[len(hb.dataLenStack)-digits] = dataLen
	hb.dataLenStack = hb.dataLenStack[:len(hb.dataLenStack)-digits+1]

	if hashStackStride*len(hb.nodeStack) > len(hb.hashStack) {
		hb.nodeStack = hb.nodeStack[:len(hb.nodeStack)-digits+1]
		hb.nodeStack[len(hb.nodeStack)-1] = nil
		if hb.trace {
			fmt.Printf("Setting hb.nodeStack[%d] to nil\n", len(hb.nodeStack)-1)
		}
	}
	if hb.trace {
		fmt.Printf("Stack depth: %d, %d\n", len(hb.nodeStack), len(hb.dataLenStack))
	}
	return nil
}

func (hb *HashBuilder) hash(hash []byte, dataLen uint64) error {
	if hb.trace {
		fmt.Printf("HASH %d\n", dataLen)
	}
	hb.hashStack = append(hb.hashStack, 0x80+common.HashLength)
	hb.hashStack = append(hb.hashStack, hash...)
	hb.nodeStack = append(hb.nodeStack, nil)
	hb.dataLenStack = append(hb.dataLenStack, dataLen) // count only data nodes, not hash nodes. so, no opcode added.
	if hb.trace {
		fmt.Printf("Stack depth: %d, %d\n", len(hb.nodeStack), len(hb.dataLenStack))
	}

	return nil
}

func (hb *HashBuilder) code(code []byte) error {
	if hb.trace {
		fmt.Printf("CODE\n")
	}
	codeCopy := common.CopyBytes(code)
	n := codeNode(codeCopy)
	hb.nodeStack = append(hb.nodeStack, n)
	hb.dataLenStack = append(hb.dataLenStack, 1+uint64(len(code))) // opcode + len(code)
	hb.sha.Reset()
	if _, err := hb.sha.Write(codeCopy); err != nil {
		return err
	}
	var hash [hashStackStride]byte // RLP representation of hash (or un-hashes value)
	hash[0] = 0x80 + common.HashLength
	if _, err := hb.sha.Read(hash[1:]); err != nil {
		return err
	}
	hb.hashStack = append(hb.hashStack, hash[:]...)
	return nil
}

func (hb *HashBuilder) emptyRoot() {
	if hb.trace {
		fmt.Printf("EMPTYROOT\n")
	}
	hb.nodeStack = append(hb.nodeStack, nil)
	hb.dataLenStack = append(hb.dataLenStack, 0)
	var hash [hashStackStride]byte // RLP representation of hash (or un-hashes value)
	hash[0] = 0x80 + common.HashLength
	copy(hash[1:], EmptyRoot[:])
	hb.hashStack = append(hb.hashStack, hash[:]...)
}

func (hb *HashBuilder) RootHash() (common.Hash, error) {
	if !hb.hasRoot() {
		return common.Hash{}, fmt.Errorf("no root in the tree")
	}
	return hb.rootHash(), nil
}

func (hb *HashBuilder) rootHash() common.Hash {
	var hash common.Hash
	copy(hash[:], hb.hashStack[len(hb.hashStack)-hashStackStride+1:])
	return hash
}

func (hb *HashBuilder) root() node {
	if hb.trace && len(hb.nodeStack) > 0 {
		fmt.Printf("len(hb.nodeStack)=%d\n", len(hb.nodeStack))
		fmt.Printf("len(hb.dataLenStack)=%d\n", len(hb.dataLenStack))
	}
	return hb.nodeStack[len(hb.nodeStack)-1]
}

func (hb *HashBuilder) hasRoot() bool {
	return len(hb.nodeStack) > 0
}
