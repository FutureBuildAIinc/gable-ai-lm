// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package license

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Token wire format.
//
//	ailm1.<kid>.<base64url(claims JSON)>.<base64url(ed25519 signature))>
//
// The signature is DETACHED over the raw claim-set JSON bytes — the exact bytes
// that were base64-encoded into segment 3, not a re-marshalling of them. A
// verifier therefore checks the issuer's bytes rather than its own idea of
// them, so no re-encoding difference can move the signed set. An unrecognised
// claim is REFUSED rather than ignored: a claim set records a determination,
// and a binary that cannot read all of it cannot faithfully report it.
//
// The key id is derived from the public key itself (KeyID) rather than being
// assigned, so a deployment configures a key and never a key *name*: there is
// no second thing to keep in sync, and a token can say which key signed it
// without anybody maintaining a registry. It is outside the signature on
// purpose — it only selects a key, and a tampered kid either names a key this
// deployment does not trust (ErrUnknownSigningKey) or names one the signature
// does not verify under (ErrBadSignature). Both refuse to boot.
const tokenPrefix = "ailm1"

var (
	// ErrMalformedToken means the token is not a token: wrong prefix, wrong
	// segment count, undecodable base64, or a claim set that is not JSON.
	ErrMalformedToken = errors.New("license: malformed token")

	// ErrUnknownSigningKey means the token names a signing key this deployment
	// does not trust. Never a downgrade — an untrusted issuer is not a weaker
	// licence, it is somebody else's licence.
	ErrUnknownSigningKey = errors.New("license: unknown signing key")

	// ErrBadSignature means the trusted key did not sign these bytes: a forged
	// token, or a genuine one with a claim edited after issue.
	ErrBadSignature = errors.New("license: signature verification failed")

	// ErrNoTrustedKeys means a token was presented but no verification key is
	// configured, so nothing can be checked. Refusing is the only honest
	// answer: accepting would make the signature decorative.
	ErrNoTrustedKeys = errors.New("license: a license token was supplied but no trusted signing key is configured (set LICENSE_PUBLIC_KEY)")

	// ErrInvalidClaims means the token verified but says something a licence
	// cannot say — a missing issuer or subject, an unparseable availability
	// date, an edition this software does not issue.
	ErrInvalidClaims = errors.New("license: invalid claim set")

	// ErrCompetingUse is the field-of-use refusal. See Load.
	ErrCompetingUse = errors.New("license: competing use is outside the grant")
)

var b64 = base64.RawURLEncoding

// KeyID returns the short, stable identifier for an Ed25519 public key: the
// first eight bytes of its SHA-256, in hex. It is a lookup handle, not a
// security boundary — the signature is what is trusted.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// EncodeKey renders a key for an environment variable or a operator's notes.
func EncodeKey(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

// ParsePublicKey decodes a base64 Ed25519 public key. Standard and URL
// alphabets are both accepted, padded or not, because this value is copied by
// hand between a key ceremony and a deployment manifest.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := decodeKeyBytes(s)
	if err != nil {
		return nil, fmt.Errorf("license: public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("license: public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey decodes a base64 Ed25519 private (seed-expanded) key. Used
// only by cmd/keygen and by tests that mint a token at test time; no private
// key is ever read by the server.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	raw, err := decodeKeyBytes(s)
	if err != nil {
		return nil, fmt.Errorf("license: private key: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	default:
		return nil, fmt.Errorf("license: private key is %d bytes, want %d or %d", len(raw), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

func decodeKeyBytes(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty")
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(s); err == nil {
			return raw, nil
		}
	}
	return nil, errors.New("not valid base64")
}

// SplitKeys parses the LICENSE_PUBLIC_KEY environment value: one or more base64
// keys separated by commas or whitespace. More than one is normal during a key
// rotation, when tokens signed by either key must verify.
func SplitKeys(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Mint signs a claim set into a token. It lives here (rather than in cmd/keygen)
// so the test suite can mint its own tokens from a keypair generated at test
// time, which is why no private key is committed to this repository.
func Mint(c Claims, priv ed25519.PrivateKey) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("license: private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("license: encode claims: %w", err)
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, payload)
	return strings.Join([]string{
		tokenPrefix,
		KeyID(pub),
		b64.EncodeToString(payload),
		b64.EncodeToString(sig),
	}, "."), nil
}

// verify parses a token, selects its signing key from the trusted set, and
// checks the detached signature. Every failure path returns an error: this
// function never downgrades a bad token into a lesser licence.
func verify(token string, trusted map[string]ed25519.PublicKey) (*Claims, string, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 4 || parts[0] != tokenPrefix {
		return nil, "", fmt.Errorf("%w: expected %s.<key-id>.<claims>.<signature>", ErrMalformedToken, tokenPrefix)
	}
	kid, encPayload, encSig := parts[1], parts[2], parts[3]

	payload, err := b64.DecodeString(encPayload)
	if err != nil {
		return nil, kid, fmt.Errorf("%w: claim set is not base64url: %v", ErrMalformedToken, err)
	}
	sig, err := b64.DecodeString(encSig)
	if err != nil {
		return nil, kid, fmt.Errorf("%w: signature is not base64url: %v", ErrMalformedToken, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, kid, fmt.Errorf("%w: signature is %d bytes, want %d", ErrMalformedToken, len(sig), ed25519.SignatureSize)
	}

	pub, ok := trusted[kid]
	if !ok {
		return nil, kid, fmt.Errorf("%w: token is signed by key %s, which this deployment does not trust", ErrUnknownSigningKey, kid)
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, kid, fmt.Errorf("%w: key %s did not sign this claim set (the token is forged, truncated, or was edited after issue)", ErrBadSignature, kid)
	}

	// Decoded only AFTER the signature check, so nothing unverified is ever
	// parsed into a decision. Unknown fields are rejected rather than dropped:
	// a token minted by a newer issuer carries a determination this binary
	// cannot represent, and reporting a licence it only partly understood is
	// worse than saying so at boot.
	var c Claims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, kid, fmt.Errorf("%w: claim set is not a license claim set this build understands: %v", ErrMalformedToken, err)
	}
	return &c, kid, nil
}
