// ==============================================================================
// MasterDnsVPN
// Author: MasterkinG32
// Github: https://github.com/masterking32
// Year: 2026
// ==============================================================================

package security

import "sync"

// Step 13 — sized buffer pool tuned for crypto Seal/Open scratch.
//
// The legacy `cryptoBufferPool` was a single sync.Pool of 4096-byte buffers
// with a doubling-grow path that *dropped* over-sized buffers on Put. For
// payloads larger than 4KB this defeated reuse entirely; for smaller ones it
// kept the buffer larger than needed but never tightened.
//
// We keep that pool as a fallback (for callers that ask for arbitrary sizes
// without knowing tier boundaries) and add a four-tier sized pool dedicated
// to AEAD/ChaCha encrypt outputs. Tier sizes are chosen to match the typical
// MasterDnsVPN packet life-cycle:
//
//     tier        cap        typical use
//     small       512       DNS A/AAAA-sized payloads, session-accept blobs
//     medium     2048       single ARQ segment + nonce + tag
//     large      8192       MTU-merged batches, larger upstream answers
//     jumbo     65536       fragment-store joins, batched control payloads

const (
	cryptoTierSmall  = 512
	cryptoTierMedium = 2 * 1024
	cryptoTierLarge  = 8 * 1024
	cryptoTierJumbo  = 64 * 1024
)

var (
	cryptoSmall  = newSizedCryptoPool(cryptoTierSmall)
	cryptoMedium = newSizedCryptoPool(cryptoTierMedium)
	cryptoLarge  = newSizedCryptoPool(cryptoTierLarge)
	cryptoJumbo  = newSizedCryptoPool(cryptoTierJumbo)
)

func newSizedCryptoPool(size int) *sync.Pool {
	return &sync.Pool{
		New: func() any {
			buf := make([]byte, size)
			return &buf
		},
	}
}

// getSizedCryptoBuffer returns a pointer to a slice whose cap is at least
// `needed`. The second return is the pool the buffer should be returned to;
// nil for over-jumbo requests (those are made fresh and dropped on Put).
//
// Callers re-slice the returned buffer as they see fit; the buffer is sized
// to the *full tier* on return so the next Get sees that capacity again.
func getSizedCryptoBuffer(needed int) (*[]byte, *sync.Pool) {
	switch {
	case needed <= cryptoTierSmall:
		return cryptoSmall.Get().(*[]byte), cryptoSmall
	case needed <= cryptoTierMedium:
		return cryptoMedium.Get().(*[]byte), cryptoMedium
	case needed <= cryptoTierLarge:
		return cryptoLarge.Get().(*[]byte), cryptoLarge
	case needed <= cryptoTierJumbo:
		return cryptoJumbo.Get().(*[]byte), cryptoJumbo
	default:
		buf := make([]byte, needed)
		return &buf, nil
	}
}

// putSizedCryptoBuffer returns a buffer to its tier. nil-safe; over-jumbo
// buffers (pool==nil) are simply dropped on the floor.
func putSizedCryptoBuffer(bufPtr *[]byte, pool *sync.Pool) {
	if bufPtr == nil || pool == nil {
		return
	}
	pool.Put(bufPtr)
}
