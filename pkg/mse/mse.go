// Package mse implements Message Stream Encryption (MSE) / Protocol Encryption (PE)
// as specified in BEP 8 (https://www.bittorrent.org/beps/bep_0008.html).
//
// MSE wraps a TCP connection with a Diffie-Hellman key exchange followed by
// optional RC4 stream encryption.  It provides obfuscation (not strong security)
// to defeat ISP deep-packet inspection that throttles or blocks BitTorrent traffic.
//
// # Wire format (step numbers match BEP 8 §3)
//
//  1. A→B: Ya (96 bytes DH public key) || PadA (0–512 random bytes)
//  2. B→A: Yb (96 bytes DH public key) || PadB (0–512 random bytes)
//  3. A→B: HASH('req1',S)                             [20 bytes, plaintext]
//     HASH('req2',SKEY) XOR HASH('req3',S)       [20 bytes, plaintext]
//     ENCRYPT_A(VC || crypto_provide || len(PadC) || PadC || len(IA))
//     ENCRYPT_A(IA)
//  4. B→A: ENCRYPT_B(VC || crypto_select || len(PadD) || PadD)
//     ENCRYPT_B(payload stream)
//  5. A→B: ENCRYPT_A(payload stream)
//
// Where:
//   - S = DH shared secret (Yb^xa mod P = Xa^yb mod P)
//   - SKEY = info hash (20 bytes) identifying the torrent
//   - ENCRYPT_A uses RC4(SHA1("keyA" || S || SKEY)) — A encrypts, B decrypts
//   - ENCRYPT_B uses RC4(SHA1("keyB" || S || SKEY)) — B encrypts, A decrypts
//   - VC = 8 zero bytes (verification constant)
//   - IA = initial application data (typically the BEP 3 handshake bytes)
//
// # Synchronisation
//
// Because PadA and PadB have unknown length, both sides scan the incoming stream
// for a known pattern to find where the padding ends:
//   - Responder B scans for HASH('req1',S) in plaintext to skip PadA.
//   - Initiator A scans the RC4-decrypted stream for the 8-byte VC to skip PadB.
//
// # Crypto methods
//
//	CryptoPlaintext (0x01): no RC4 after handshake — just key exchange for obfuscation
//	CryptoRC4       (0x02): RC4 stream encryption for payload
package mse

import (
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1" //nolint:gosec // SHA-1 mandated by BEP 8 spec
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"

	"github.com/cbluth/bittorrent/pkg/dht"
)

// ── BEP 8 Diffie-Hellman parameters (768-bit MODP group) ─────────────────────

var (
	// P is the 768-bit safe prime defined in BEP 8.
	P, _ = new(big.Int).SetString(
		"FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD1"+
			"29024E088A67CC74020BBEA63B139B22514A08798E3404DD"+
			"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245"+
			"E485B576625E7EC6F44C42E9A63A36210000000000090563",
		16,
	)
	// G is the generator (2).
	G = big.NewInt(2)
)

// ── Constants ────────────────────────────────────────────────────────────────

const (
	// KeySize is the DH public/private key size in bytes (768 bits).
	KeySize = 96

	// VCLength is the length of the verification constant (8 zero bytes).
	VCLength = 8

	// maxPadLen is the maximum padding length on each side.
	maxPadLen = 512

	// syncSearchLen is the maximum bytes to scan when synchronising.
	// 96 (DH key already read) + 512 (max pad) + 20 (pattern length) = 628.
	syncSearchLen = 628

	// HandshakeTimeout is the wall-clock budget for the entire MSE handshake.
	HandshakeTimeout = 30 * time.Second
)

// ── Crypto method bitmask (BEP 8 §4) ─────────────────────────────────────────

const (
	CryptoPlaintext uint32 = 0x01 // No RC4; key exchange only
	CryptoRC4       uint32 = 0x02 // RC4 stream encryption
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

var (
	// ErrNoCryptoMethod is returned when no mutually supported crypto method exists.
	ErrNoCryptoMethod = errors.New("mse: no mutually supported crypto method")

	// ErrVCMismatch is returned when the VC sync scan exhausts its search budget.
	ErrVCMismatch = errors.New("mse: verification constant not found (sync failed)")

	// ErrUnknownInfoHash is returned by the responder when req2 does not match any
	// known torrent info hash.
	ErrUnknownInfoHash = errors.New("mse: unknown info hash")
)

// ── Conn ─────────────────────────────────────────────────────────────────────

// Conn is an MSE-wrapped net.Conn.  After a successful Handshake call, all
// reads and writes are transparently encrypted (RC4) or passed through (plaintext).
type Conn struct {
	net.Conn
	method    uint32      // negotiated CryptoMethod
	encStream *rc4.Cipher // nil when method == CryptoPlaintext
	decStream *rc4.Cipher // nil when method == CryptoPlaintext
}

// Method returns the negotiated crypto method.
func (c *Conn) Method() uint32 { return c.method }

// Read decrypts incoming bytes when RC4 is active.
func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.decStream != nil && n > 0 {
		c.decStream.XORKeyStream(b[:n], b[:n])
	}
	return n, err
}

// Write encrypts outgoing bytes when RC4 is active.
func (c *Conn) Write(b []byte) (int, error) {
	if c.encStream == nil {
		return c.Conn.Write(b)
	}
	buf := make([]byte, len(b))
	c.encStream.XORKeyStream(buf, b)
	return c.Conn.Write(buf)
}

// ── Public handshake functions ────────────────────────────────────────────────

// InitiatorHandshake performs the MSE handshake as the connection initiator (A).
//
// infoHash identifies the torrent we want to download.
// provide is a bitmask of the crypto methods we accept (CryptoPlaintext | CryptoRC4).
//
// On success the returned *Conn is ready for BEP 3 handshake data.
func InitiatorHandshake(conn net.Conn, infoHash dht.Key, provide uint32) (*Conn, error) {
	if err := conn.SetDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
		return nil, fmt.Errorf("mse: set deadline: %w", err)
	}

	// ── Step 1: generate key pair, send Ya + PadA ────────────────────────────
	xa, err := generateDHPrivate()
	if err != nil {
		return nil, err
	}
	Ya := dhPublicKey(xa)

	if _, err := conn.Write(padTo96(Ya)); err != nil {
		return nil, fmt.Errorf("mse: send Ya: %w", err)
	}
	if _, err := conn.Write(randomPad()); err != nil {
		return nil, fmt.Errorf("mse: send PadA: %w", err)
	}

	// ── Step 2: read Yb (96 bytes); pad is consumed during VC sync below ─────
	ybBuf := make([]byte, KeySize)
	if _, err := io.ReadFull(conn, ybBuf); err != nil {
		return nil, fmt.Errorf("mse: read Yb: %w", err)
	}
	Yb := new(big.Int).SetBytes(ybBuf)

	// ── Compute shared secret ─────────────────────────────────────────────────
	S := padTo96(dhSharedSecret(Yb, xa))

	// Derive cipher streams from A's perspective:
	//   encA = RC4(SHA1("keyA" || S || SKEY))  — A encrypts, B decrypts
	//   decA = RC4(SHA1("keyB" || S || SKEY))  — B encrypts, A decrypts
	encA, decA, err := deriveKeys(S, infoHash[:], "keyA", "keyB")
	if err != nil {
		return nil, err
	}

	// ── Step 3: send req1 + req2 + ENCRYPT_A(handshake) ─────────────────────
	req1 := sha1HashStr("req1", S)
	req2 := xor20(sha1HashStr("req2", infoHash[:]), sha1HashStr("req3", S))

	if _, err := conn.Write(req1[:]); err != nil {
		return nil, fmt.Errorf("mse: send req1: %w", err)
	}
	if _, err := conn.Write(req2[:]); err != nil {
		return nil, fmt.Errorf("mse: send req2: %w", err)
	}

	// Encrypt and send: VC + crypto_provide + len(PadC) + PadC + len(IA)=0
	padC := randomPad()
	payload := make([]byte, VCLength+4+2+len(padC)+2)
	// VC: bytes 0–7 = zeros (zero value of array slice)
	binary.BigEndian.PutUint32(payload[VCLength:], provide)
	binary.BigEndian.PutUint16(payload[VCLength+4:], uint16(len(padC)))
	copy(payload[VCLength+4+2:], padC)
	// len(IA) at end = 0 (we emit no IA; caller sends BEP 3 handshake directly)
	encA.XORKeyStream(payload, payload)
	if _, err := conn.Write(payload); err != nil {
		return nil, fmt.Errorf("mse: send encrypted handshake: %w", err)
	}

	// ── Step 4: synchronise on B's VC (skip PadB in B's output) ─────────────
	// B's stream at this point: PadB || ENCRYPT_B(VC || crypto_select || PadD)
	// We decrypt byte-by-byte with decA and scan for 8 zero bytes (VC).
	if err := syncOnVC(conn, decA); err != nil {
		return nil, err
	}

	// Read crypto_select (4 bytes, already decrypted by syncOnVC draining the stream)
	var csBuf [4]byte
	if err := decRead(decA, conn, csBuf[:]); err != nil {
		return nil, fmt.Errorf("mse: read crypto_select: %w", err)
	}
	method := binary.BigEndian.Uint32(csBuf[:])
	if method&provide == 0 {
		return nil, ErrNoCryptoMethod
	}

	// Read and discard PadD.
	var pdLenBuf [2]byte
	if err := decRead(decA, conn, pdLenBuf[:]); err != nil {
		return nil, fmt.Errorf("mse: read PadD length: %w", err)
	}
	if n := int(binary.BigEndian.Uint16(pdLenBuf[:])); n > 0 {
		if err := decRead(decA, conn, make([]byte, n)); err != nil {
			return nil, fmt.Errorf("mse: read PadD: %w", err)
		}
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("mse: clear deadline: %w", err)
	}

	if method == CryptoPlaintext {
		return &Conn{Conn: conn, method: method}, nil
	}
	return &Conn{Conn: conn, method: method, encStream: encA, decStream: decA}, nil
}

// ResponderHandshake performs the MSE handshake as the connection responder (B).
//
// lookupInfoHash is called with SHA1("req2", SKEY) — the caller must check each
// known info hash h whether SHA1("req2", h) equals the provided value and, if so,
// return h.  It returns ErrUnknownInfoHash when no match is found.
//
// accept is a bitmask of crypto methods we are willing to use.  The method chosen
// is the highest-priority bit that appears in both provide and accept (RC4 > plaintext).
func ResponderHandshake(conn net.Conn, lookupInfoHash func(dht.Key) (dht.Key, bool), accept uint32) (*Conn, error) {
	if err := conn.SetDeadline(time.Now().Add(HandshakeTimeout)); err != nil {
		return nil, fmt.Errorf("mse: set deadline: %w", err)
	}

	// ── Step 2 (responder side): read Ya, send Yb + PadB ────────────────────
	xaBuf := make([]byte, KeySize)
	if _, err := io.ReadFull(conn, xaBuf); err != nil {
		return nil, fmt.Errorf("mse: read Ya: %w", err)
	}
	Xa := new(big.Int).SetBytes(xaBuf)

	yb, err := generateDHPrivate()
	if err != nil {
		return nil, err
	}
	Yb := dhPublicKey(yb)

	if _, err := conn.Write(padTo96(Yb)); err != nil {
		return nil, fmt.Errorf("mse: send Yb: %w", err)
	}
	if _, err := conn.Write(randomPad()); err != nil {
		return nil, fmt.Errorf("mse: send PadB: %w", err)
	}

	// ── Compute shared secret ─────────────────────────────────────────────────
	S := padTo96(dhSharedSecret(Xa, yb))

	// ── Step 3 (receive): sync on req1, then read req2 ───────────────────────
	// A's stream: PadA || req1 || req2 || ENCRYPT_A(...)
	// Scan plaintext for req1 = SHA1("req1", S).
	req1 := sha1HashStr("req1", S)
	if err := findInStream(conn, req1[:]); err != nil {
		return nil, fmt.Errorf("mse: sync on req1: %w", err)
	}

	var req2Buf dht.Key
	if _, err := io.ReadFull(conn, req2Buf[:]); err != nil {
		return nil, fmt.Errorf("mse: read req2: %w", err)
	}

	// Recover SHA1("req2", SKEY) = req2 XOR SHA1("req3", S).
	obfuscated := xor20(req2Buf, sha1HashStr("req3", S))

	infoHash, ok := lookupInfoHash(obfuscated)
	if !ok {
		return nil, ErrUnknownInfoHash
	}

	// Derive cipher streams from B's perspective:
	//   decB = RC4(SHA1("keyA" || S || SKEY))  — A encrypts, B decrypts
	//   encB = RC4(SHA1("keyB" || S || SKEY))  — B encrypts, A decrypts
	decB, encB, err := deriveKeys(S, infoHash[:], "keyA", "keyB")
	if err != nil {
		return nil, err
	}

	// ── Step 3 (receive cont.): decrypt VC + crypto_provide + PadC ───────────
	var vcBuf [VCLength]byte
	if err := decRead(decB, conn, vcBuf[:]); err != nil {
		return nil, fmt.Errorf("mse: read VC: %w", err)
	}
	if vcBuf != ([VCLength]byte{}) {
		return nil, ErrVCMismatch
	}

	var cpBuf [4]byte
	if err := decRead(decB, conn, cpBuf[:]); err != nil {
		return nil, fmt.Errorf("mse: read crypto_provide: %w", err)
	}
	provide := binary.BigEndian.Uint32(cpBuf[:])

	var padCLenBuf [2]byte
	if err := decRead(decB, conn, padCLenBuf[:]); err != nil {
		return nil, fmt.Errorf("mse: read PadC length: %w", err)
	}
	if n := int(binary.BigEndian.Uint16(padCLenBuf[:])); n > 0 {
		if err := decRead(decB, conn, make([]byte, n)); err != nil {
			return nil, fmt.Errorf("mse: read PadC: %w", err)
		}
	}

	// Read len(IA) — caller's BEP 3 handshake bytes bundled in step 3.
	var iaLenBuf [2]byte
	if err := decRead(decB, conn, iaLenBuf[:]); err != nil {
		return nil, fmt.Errorf("mse: read IA length: %w", err)
	}
	// IA itself: read and buffer for the caller if non-zero.
	// (We discard IA here; in a real integration this would be handed back.)
	if n := int(binary.BigEndian.Uint16(iaLenBuf[:])); n > 0 {
		if err := decRead(decB, conn, make([]byte, n)); err != nil {
			return nil, fmt.Errorf("mse: read IA: %w", err)
		}
	}

	// ── Step 4: select crypto method, send VC + crypto_select + PadD ─────────
	method := selectMethod(provide, accept)
	if method == 0 {
		return nil, ErrNoCryptoMethod
	}

	padD := randomPad()
	response := make([]byte, VCLength+4+2+len(padD))
	// VC: bytes 0–7 = zeros
	binary.BigEndian.PutUint32(response[VCLength:], method)
	binary.BigEndian.PutUint16(response[VCLength+4:], uint16(len(padD)))
	copy(response[VCLength+4+2:], padD)
	encB.XORKeyStream(response, response)
	if _, err := conn.Write(response); err != nil {
		return nil, fmt.Errorf("mse: send step-4 response: %w", err)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("mse: clear deadline: %w", err)
	}

	if method == CryptoPlaintext {
		return &Conn{Conn: conn, method: method}, nil
	}
	return &Conn{Conn: conn, method: method, encStream: encB, decStream: decB}, nil
}

// ── Crypto helpers ────────────────────────────────────────────────────────────

// deriveKeys creates two RC4 ciphers using the given key names (e.g., "keyA", "keyB").
// firstKey → first returned cipher; secondKey → second returned cipher.
// Each cipher discards the first 1024 bytes of keystream as required by BEP 8.
func deriveKeys(S, infoHash []byte, firstKey, secondKey string) (*rc4.Cipher, *rc4.Cipher, error) {
	first, err := newRC4(firstKey, S, infoHash)
	if err != nil {
		return nil, nil, err
	}
	second, err := newRC4(secondKey, S, infoHash)
	if err != nil {
		return nil, nil, err
	}
	return first, second, nil
}

// newRC4 computes RC4(SHA1(label || S || SKEY)) and discards the first 1024 keystream bytes.
func newRC4(label string, S, infoHash []byte) (*rc4.Cipher, error) {
	h := sha1.New() //nolint:gosec
	h.Write([]byte(label))
	h.Write(S)
	h.Write(infoHash)
	key := h.Sum(nil)

	c, err := rc4.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("mse: rc4 %s: %w", label, err)
	}
	discard := make([]byte, 1024)
	c.XORKeyStream(discard, discard) // discard first 1024 bytes (BEP 8 §3)
	return c, nil
}

// sha1Hash computes SHA1(parts...).
func sha1Hash(parts ...[]byte) dht.Key {
	h := sha1.New() //nolint:gosec
	for _, p := range parts {
		h.Write(p)
	}
	var out dht.Key
	h.Sum(out[:0])
	return out
}

// sha1HashStr is a convenience wrapper when the first part is a string literal.
func sha1HashStr(label string, rest ...[]byte) dht.Key {
	return sha1Hash(append([][]byte{[]byte(label)}, rest...)...)
}

// xor20 returns a XOR b.
func xor20(a, b dht.Key) dht.Key {
	var out dht.Key
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// selectMethod returns the highest-priority method present in both provide and accept.
// Priority: RC4 > plaintext.
func selectMethod(provide, accept uint32) uint32 {
	combined := provide & accept
	if combined&CryptoRC4 != 0 {
		return CryptoRC4
	}
	if combined&CryptoPlaintext != 0 {
		return CryptoPlaintext
	}
	return 0
}

// ── Stream I/O helpers ────────────────────────────────────────────────────────

// encWrite encrypts data with cipher and writes it to w.
func encWrite(cipher *rc4.Cipher, w io.Writer, data []byte) error {
	buf := make([]byte, len(data))
	cipher.XORKeyStream(buf, data)
	_, err := w.Write(buf)
	return err
}

// decRead reads exactly len(dst) bytes from r, decrypts them in-place with cipher.
func decRead(cipher *rc4.Cipher, r io.Reader, dst []byte) error {
	if _, err := io.ReadFull(r, dst); err != nil {
		return err
	}
	cipher.XORKeyStream(dst, dst)
	return nil
}

// syncOnVC reads bytes from r one at a time, decrypting each with cipher, until
// it finds the 8-byte VC (all zeros).  The sync consumes the VC itself.
// This is used by the initiator to skip the responder's PadB.
func syncOnVC(r io.Reader, cipher *rc4.Cipher) error {
	var window [VCLength]byte
	buf := [1]byte{}

	for i := 0; i < syncSearchLen; i++ {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return fmt.Errorf("mse: vc sync: %w", err)
		}
		cipher.XORKeyStream(buf[:], buf[:])

		// Slide the window.
		copy(window[:], window[1:])
		window[VCLength-1] = buf[0]

		if i >= VCLength-1 && window == ([VCLength]byte{}) {
			return nil // found VC
		}
	}
	return ErrVCMismatch
}

// findInStream reads bytes from r one at a time (plaintext) until it finds
// the given pattern, or exhausts syncSearchLen bytes.
// Used by the responder to skip PadA and locate req1.
func findInStream(r io.Reader, pattern []byte) error {
	n := len(pattern)
	window := make([]byte, n)
	buf := [1]byte{}

	for i := 0; i < syncSearchLen; i++ {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return fmt.Errorf("mse: req1 sync: %w", err)
		}
		// Slide the window.
		copy(window, window[1:])
		window[n-1] = buf[0]

		if i >= n-1 && bytes.Equal(window, pattern) {
			return nil // found pattern
		}
	}
	return fmt.Errorf("mse: req1 pattern not found in stream")
}

// ── DH helpers ────────────────────────────────────────────────────────────────

// generateDHPrivate generates a 160-bit (20-byte) random DH private key.
func generateDHPrivate() (*big.Int, error) {
	buf := make([]byte, 20)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return nil, fmt.Errorf("mse: generate DH private key: %w", err)
	}
	return new(big.Int).SetBytes(buf), nil
}

// dhPublicKey computes G^priv mod P.
func dhPublicKey(priv *big.Int) *big.Int {
	return new(big.Int).Exp(G, priv, P)
}

// dhSharedSecret computes peerPub^priv mod P.
func dhSharedSecret(peerPub, priv *big.Int) *big.Int {
	return new(big.Int).Exp(peerPub, priv, P)
}

// padTo96 encodes n as a 96-byte (768-bit) big-endian integer, zero-padding
// on the left if the natural encoding is shorter.
func padTo96(n *big.Int) []byte {
	b := n.Bytes()
	if len(b) >= KeySize {
		return b[:KeySize]
	}
	out := make([]byte, KeySize)
	copy(out[KeySize-len(b):], b)
	return out
}

// randomPad generates between 0 and maxPadLen random bytes.
func randomPad() []byte {
	var lenBuf [2]byte
	if _, err := rand.Read(lenBuf[:]); err != nil {
		return nil
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:])) % (maxPadLen + 1)
	if n == 0 {
		return nil
	}
	pad := make([]byte, n)
	if _, err := rand.Read(pad); err != nil {
		return nil
	}
	return pad
}

// ── Convenience helpers for callers ──────────────────────────────────────────

// ObfuscateInfoHash returns SHA1("req2", infoHash) — the value that should be
// compared against the req2⊕req3 value received from the initiator when
// implementing lookupInfoHash in the responder.
//
// Example usage:
//
//	obf := mse.ObfuscateInfoHash(knownHash)
//	lookup := func(candidate dht.Key) (dht.Key, bool) {
//	    for _, h := range myTorrents {
//	        if mse.ObfuscateInfoHash(h) == candidate { return h, true }
//	    }
//	    return [20]byte{}, false
//	}
func ObfuscateInfoHash(infoHash dht.Key) dht.Key {
	return sha1HashStr("req2", infoHash[:])
}

// keep sha1HashStr used (it's called only via ObfuscateInfoHash above).
var _ = sha1HashStr
