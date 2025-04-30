package prefixstore

import (
	"fmt"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/daulet/tokenizers"
	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	// DefaultBlockSize defines how many tokens each block contains in the prefix cache.
	DefaultBlockSize = 256
	// DefaultMaxCacheSize sets the maximum number of blocks the LRU cache can store.
	DefaultMaxCacheSize = 500000
)

// LRUStoreConfig contains initialization settings for LRUTokenStore (block size and cache size).
type LRUStoreConfig struct {
	CacheSize int
	BlockSize int
}

// Block holds the tokens contained in the block.
// A token is contained iff its [_, high] offset is associated with a substring
// of the chunk that was used to generate the hash (key) of the block.
type Block struct {
	Tokens []uint32
}

// LRUTokenStore is an in-memory prefix-to-block cache with xxhash keys and LRU
// eviction.
type LRUTokenStore struct {
	mu sync.RWMutex

	cacheSize int
	blockSize int

	store map[string]*lru.Cache[uint64, Block]
}

// NewLRUTokenStore initializes the LRUTokenStore with LRU cache.
func NewLRUTokenStore(cfg *LRUStoreConfig) (Indexer, error) {
	cacheSize := DefaultMaxCacheSize
	blockSize := DefaultBlockSize

	if cfg != nil {
		if cfg.CacheSize > 0 {
			cacheSize = cfg.CacheSize
		}
		if cfg.BlockSize > 0 {
			blockSize = cfg.BlockSize
		}
	}

	return &LRUTokenStore{
		cacheSize: cacheSize,
		blockSize: blockSize,
		store:     make(map[string]*lru.Cache[uint64, Block]),
	}, nil
}

// AddTokenization adds the full tokenization of a string to the
// indexer for a given model.
// The function assumes tokens and offsets are of the same length.
// The function assumes that tokens will not be mutated after the call.
func (c *LRUTokenStore) AddTokenization(modelName string, text string, tokens []uint32,
	offsets []tokenizers.Offset,
) error {
	if text == "" || len(tokens) == 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Get or create the LRU cache for the model
	cache, ok := c.store[modelName]
	if !ok {
		var err error
		cache, err = lru.New[uint64, Block](c.cacheSize)
		if err != nil {
			return fmt.Errorf("failed to create LRU cache for model %s: %w", modelName, err)
		}

		c.store[modelName] = cache
	}

	tokenIdxIterator := 0
	// Chunk the text into blocks and populate the cache
	for start := 0; start < len(text); start += c.blockSize {
		end := start + c.blockSize
		if end > len(text) {
			end = len(text)
		}

		// Compute the hash for the current block
		digest := xxhash.New()
		//nolint:gocritic // Cast inside
		if _, err := digest.Write([]byte(text[start:end])); err != nil {
			return fmt.Errorf("failed to add token: %w", err)
		}

		blockHash := digest.Sum64()

		// Only add tokens with [_, high] offset associated with the chunk range.
		// If a token's [low, _] index is less than the start, it is OK as long as
		// the above condition is satisfied.

		block := Block{Tokens: []uint32{}}
		for ; tokenIdxIterator < len(tokens); tokenIdxIterator++ {
			//nolint:gosec // Again end is tied to context-window size, safe to assume it won't reach max int32
			if offsets[tokenIdxIterator][1] <= uint(end) {
				block.Tokens = append(block.Tokens, tokens[tokenIdxIterator])
			} else {
				break
			}
		}

		cache.Add(blockHash, block)
	}

	return nil
}

// FindLongestContainedTokens finds the sequence of contained tokens for
// the longest matching prefix.
func (c *LRUTokenStore) FindLongestContainedTokens(prompt, modelName string) []uint32 {
	c.mu.RLock()
	cache, ok := c.store[modelName]
	c.mu.RUnlock()

	if !ok {
		return nil
	}

	containedTokens := []uint32{}

	// Chunk the text into blocks and populate the cache
	for i := 0; i < len(prompt); i += c.blockSize {
		end := i + c.blockSize
		if end > len(prompt) {
			end = len(prompt)
		}

		// Compute the hash for the current block
		digest := xxhash.New()
		//nolint:gocritic // Cast inside
		if _, err := digest.Write([]byte(prompt[i:end])); err != nil {
			return containedTokens
		}

		blockHash := digest.Sum64()
		block, ok := cache.Get(blockHash)
		if !ok {
			break // early-stop
		}

		containedTokens = append(containedTokens, block.Tokens...)
	}

	return containedTokens
}
