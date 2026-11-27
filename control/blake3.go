package control

import (
	"encoding/binary"
	"math/bits"
)

// BLAKE3 in pure Go, as the reference implementation computes it: the
// keys a control daemon is found by (BuildKey, SetupKey) are BLAKE3 digests,
// and the daemon computes them with its own, so this one must agree to the
// byte. Only the default hash mode with a 32-byte output is needed. It is
// small and unoptimized: a key hashes a few kilobytes at most.

const (
	blake3ChunkLen   = 1024
	blake3BlockLen   = 64
	blake3OutLen     = 32
	blake3ChunkStart = 1 << 0
	blake3ChunkEnd   = 1 << 1
	blake3Parent     = 1 << 2
	blake3Root       = 1 << 3
)

var blake3IV = [8]uint32{
	0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A,
	0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19,
}

var blake3Permutation = [16]int{2, 6, 3, 10, 7, 0, 4, 13, 1, 11, 12, 5, 9, 14, 15, 8}

func blake3G(state *[16]uint32, a, b, c, d int, mx, my uint32) {
	state[a] = state[a] + state[b] + mx
	state[d] = bits.RotateLeft32(state[d]^state[a], -16)
	state[c] = state[c] + state[d]
	state[b] = bits.RotateLeft32(state[b]^state[c], -12)
	state[a] = state[a] + state[b] + my
	state[d] = bits.RotateLeft32(state[d]^state[a], -8)
	state[c] = state[c] + state[d]
	state[b] = bits.RotateLeft32(state[b]^state[c], -7)
}

func blake3Round(state *[16]uint32, m *[16]uint32) {
	blake3G(state, 0, 4, 8, 12, m[0], m[1])
	blake3G(state, 1, 5, 9, 13, m[2], m[3])
	blake3G(state, 2, 6, 10, 14, m[4], m[5])
	blake3G(state, 3, 7, 11, 15, m[6], m[7])
	blake3G(state, 0, 5, 10, 15, m[8], m[9])
	blake3G(state, 1, 6, 11, 12, m[10], m[11])
	blake3G(state, 2, 7, 8, 13, m[12], m[13])
	blake3G(state, 3, 4, 9, 14, m[14], m[15])
}

func blake3Compress(cv *[8]uint32, block *[16]uint32, counter uint64, blockLen, flags uint32) [16]uint32 {
	state := [16]uint32{
		cv[0], cv[1], cv[2], cv[3], cv[4], cv[5], cv[6], cv[7],
		blake3IV[0], blake3IV[1], blake3IV[2], blake3IV[3],
		uint32(counter), uint32(counter >> 32), blockLen, flags,
	}
	m := *block
	for round := 0; round < 7; round++ {
		blake3Round(&state, &m)
		if round < 6 {
			var permuted [16]uint32
			for i := range permuted {
				permuted[i] = m[blake3Permutation[i]]
			}
			m = permuted
		}
	}
	for i := 0; i < 8; i++ {
		state[i] ^= state[i+8]
		state[i+8] ^= cv[i]
	}
	return state
}

func blake3Words(block []byte) [16]uint32 {
	var padded [blake3BlockLen]byte
	copy(padded[:], block)
	var words [16]uint32
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(padded[i*4:])
	}
	return words
}

// blake3Output is what a chunk or a parent node yields before its last
// compression: as a chaining value, or as the root's output.
type blake3Output struct {
	cv       [8]uint32
	block    [16]uint32
	counter  uint64
	blockLen uint32
	flags    uint32
}

func (o blake3Output) chainingValue() [8]uint32 {
	out := blake3Compress(&o.cv, &o.block, o.counter, o.blockLen, o.flags)
	var cv [8]uint32
	copy(cv[:], out[:8])
	return cv
}

func (o blake3Output) rootBytes() [blake3OutLen]byte {
	out := blake3Compress(&o.cv, &o.block, 0, o.blockLen, o.flags|blake3Root)
	var digest [blake3OutLen]byte
	for i := 0; i < 8; i++ {
		binary.LittleEndian.PutUint32(digest[i*4:], out[i])
	}
	return digest
}

type blake3Chunk struct {
	cv               [8]uint32
	counter          uint64
	block            [blake3BlockLen]byte
	blockLen         int
	blocksCompressed int
}

func newBlake3Chunk(counter uint64) blake3Chunk {
	return blake3Chunk{cv: blake3IV, counter: counter}
}

func (c *blake3Chunk) len() int { return c.blocksCompressed*blake3BlockLen + c.blockLen }

func (c *blake3Chunk) startFlag() uint32 {
	if c.blocksCompressed == 0 {
		return blake3ChunkStart
	}
	return 0
}

func (c *blake3Chunk) update(input []byte) {
	for len(input) > 0 {
		if c.blockLen == blake3BlockLen {
			words := blake3Words(c.block[:])
			out := blake3Compress(&c.cv, &words, c.counter, blake3BlockLen, c.startFlag())
			copy(c.cv[:], out[:8])
			c.blocksCompressed++
			c.block = [blake3BlockLen]byte{}
			c.blockLen = 0
		}
		n := copy(c.block[c.blockLen:], input)
		c.blockLen += n
		input = input[n:]
	}
}

func (c *blake3Chunk) output() blake3Output {
	return blake3Output{
		cv:       c.cv,
		block:    blake3Words(c.block[:c.blockLen]),
		counter:  c.counter,
		blockLen: uint32(c.blockLen),
		flags:    c.startFlag() | blake3ChunkEnd,
	}
}

func blake3ParentOutput(left, right [8]uint32) blake3Output {
	var block [16]uint32
	copy(block[:8], left[:])
	copy(block[8:], right[:])
	return blake3Output{cv: blake3IV, block: block, blockLen: blake3BlockLen, flags: blake3Parent}
}

// Blake3 is an incremental BLAKE3 hasher in the default mode.
type Blake3 struct {
	chunk   blake3Chunk
	stack   [54][8]uint32
	stackN  int
	written bool
}

// NewBlake3 returns a hasher in the default mode.
func NewBlake3() *Blake3 {
	return &Blake3{chunk: newBlake3Chunk(0)}
}

func (h *Blake3) push(cv [8]uint32) {
	h.stack[h.stackN] = cv
	h.stackN++
}

func (h *Blake3) pop() [8]uint32 {
	h.stackN--
	return h.stack[h.stackN]
}

// addChunk merges completed subtrees: a chunk count with k trailing zero bits
// completes k subtrees, each of which joins the one before it on the stack.
func (h *Blake3) addChunk(cv [8]uint32, total uint64) {
	for total&1 == 0 {
		cv = blake3ParentOutput(h.pop(), cv).chainingValue()
		total >>= 1
	}
	h.push(cv)
}

// Write adds input to the hash. It never fails.
func (h *Blake3) Write(input []byte) (int, error) {
	n := len(input)
	for len(input) > 0 {
		if h.chunk.len() == blake3ChunkLen {
			cv := h.chunk.output().chainingValue()
			total := h.chunk.counter + 1
			h.addChunk(cv, total)
			h.chunk = newBlake3Chunk(total)
		}
		want := blake3ChunkLen - h.chunk.len()
		if want > len(input) {
			want = len(input)
		}
		h.chunk.update(input[:want])
		input = input[want:]
	}
	return n, nil
}

// Sum32 returns the 32-byte digest of everything written so far.
func (h *Blake3) Sum32() [blake3OutLen]byte {
	out := h.chunk.output()
	for i := h.stackN - 1; i >= 0; i-- {
		out = blake3ParentOutput(h.stack[i], out.chainingValue())
	}
	return out.rootBytes()
}

// Blake3Sum returns the BLAKE3 digest of data.
func Blake3Sum(data []byte) [blake3OutLen]byte {
	h := NewBlake3()
	_, _ = h.Write(data)
	return h.Sum32()
}
