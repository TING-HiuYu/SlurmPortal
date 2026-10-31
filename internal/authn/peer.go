package authn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
)

// TLSPeer authenticates machines by a client certificate verified by the TLS stack.
// Fingerprints, when set, pin the accepted leaf certificates (SHA-256 of DER, lowercase hex).
type TLSPeer struct {
	Fingerprints []string
}

// Authenticate accepts a verified client certificate whose fingerprint is configured (native mTLS).
func (t TLSPeer) Authenticate(r *http.Request) (*Principal, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return nil, nil
	}
	leaf := r.TLS.VerifiedChains[0][0]
	sum := sha256.Sum256(leaf.Raw)
	fp := hex.EncodeToString(sum[:])
	if len(t.Fingerprints) > 0 && !slices.Contains(t.Fingerprints, fp) {
		return nil, ErrInvalidToken
	}
	return &Principal{Username: "peer:" + leaf.Subject.CommonName, Source: SourcePeer, TokenID: fp}, nil
}

type peerKey struct{}

// WithBridgePeer marks a request as coming from a peer the Worker edge already verified (CF mTLS).
// Only the private JS bridge may call this; it is never derived from headers.
func WithBridgePeer(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, peerKey{}, id)
}

// BridgePeer trusts only identities attached via WithBridgePeer.
type BridgePeer struct{}

func (BridgePeer) Authenticate(r *http.Request) (*Principal, error) {
	id, _ := r.Context().Value(peerKey{}).(string)
	if id == "" {
		return nil, nil
	}
	return &Principal{Username: "peer:" + id, Source: SourcePeer, TokenID: id}, nil
}

// AnyPeer accepts the first authenticator that recognises the request.
type AnyPeer []Authenticator

func (a AnyPeer) Authenticate(r *http.Request) (*Principal, error) {
	for _, x := range a {
		p, err := x.Authenticate(r)
		if err != nil || p != nil {
			return p, err
		}
	}
	return nil, nil
}
