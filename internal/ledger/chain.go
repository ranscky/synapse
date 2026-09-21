package ledger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// genesisLabel is the string every tenant's chain starts from. It is a label
// rather than a fixed digest so the rule stays readable in the code that depends
// on it: the first entry chains from sha256(genesis).
const genesisLabel = "genesis"

// genesisHash returns the prev_hash of the first entry of any tenant's chain:
// hex(sha256("genesis")).
//
// The same value for every tenant is what makes a first entry recognizable
// without a special-case column -- and it is derived here rather than stored
// anywhere, because a verifier has to be able to reconstruct a chain from the
// rows and this rule alone.
func genesisHash() string {
	return hexDigest([]byte(genesisLabel))
}

// chainMessage is the exact byte string a signature covers: the entry's
// identifying fields and its trace, concatenated with no separator.
//
// No separator is needed. The first four parts have fixed widths -- a 36
// character uuid, a 36 character uuid, a 36 character uuid, and a 64 character
// hex digest -- so no boundary between them can shift, and everything after them
// is the trace. The trace is included verbatim: any normalization here would make
// a stored row's hash unreproducible from that row.
func chainMessage(e LedgerEntry) []byte {
	return []byte(e.ID + e.TenantID + e.RequestID + e.PrevHash + e.TraceJSON)
}

// signature returns the hex HMAC-SHA256 of e's chain message under secret.
//
// HMAC rather than a plain digest because the chain has to be unforgeable by
// whoever can write rows: without the tenant's secret, a tampered trace cannot be
// re-signed, so the stored hash_value stops matching what the row says and the
// tampering is visible. A plain sha256 would let anyone who can insert a row
// produce a consistent-looking chain.
func signature(secret []byte, e LedgerEntry) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(chainMessage(e))

	return hex.EncodeToString(mac.Sum(nil))
}

// hexDigest is the hex-encoded SHA-256 of b.
func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}
