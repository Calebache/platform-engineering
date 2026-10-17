package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// tokenReviewVerifier authenticates callers by their Kubernetes
// ServiceAccount token. The API server does the cryptographic check
// (TokenReview); the token must have been issued for our audience, so a token
// meant for anything else can't be replayed here.
type tokenReviewVerifier struct {
	client   kubernetes.Interface
	audience string

	mu    sync.Mutex
	cache map[[32]byte]cachedIdentity
}

type cachedIdentity struct {
	info    *auth.TokenInfo
	expires time.Time
}

func newTokenReviewVerifier(client kubernetes.Interface, audience string) *tokenReviewVerifier {
	return &tokenReviewVerifier{client: client, audience: audience, cache: map[[32]byte]cachedIdentity{}}
}

// verify matches auth.TokenVerifier. Results are cached for up to a minute
// (never past the token's own expiry) to avoid a TokenReview per request.
func (v *tokenReviewVerifier) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	key := sha256.Sum256([]byte(token))
	now := time.Now()

	v.mu.Lock()
	if c, ok := v.cache[key]; ok && now.Before(c.expires) {
		v.mu.Unlock()
		return c.info, nil
	}
	v.mu.Unlock()

	review, err := v.client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{v.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("token review: %w", err)
	}
	if !review.Status.Authenticated {
		return nil, fmt.Errorf("%w: %s", auth.ErrInvalidToken, review.Status.Error)
	}

	exp := jwtExpiry(token)
	if exp.IsZero() {
		return nil, fmt.Errorf("%w: token has no expiry", auth.ErrInvalidToken)
	}
	info := &auth.TokenInfo{
		UserID:     review.Status.User.Username,
		Expiration: exp,
		Extra:      map[string]any{"groups": review.Status.User.Groups},
	}

	cacheUntil := now.Add(time.Minute)
	if exp.Before(cacheUntil) {
		cacheUntil = exp
	}
	v.mu.Lock()
	v.cache[key] = cachedIdentity{info: info, expires: cacheUntil}
	for k, c := range v.cache { // keep the cache bounded
		if now.After(c.expires) {
			delete(v.cache, k)
		}
	}
	v.mu.Unlock()
	return info, nil
}

// jwtExpiry reads the exp claim. Safe without signature verification because
// TokenReview has already authenticated this exact token.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

var errNoCaller = errors.New("no authenticated caller")

// callerFrom returns the Kubernetes identity the HTTP auth layer attached.
func callerFrom(req mcp.Request) (string, error) {
	extra := req.GetExtra()
	if extra == nil || extra.TokenInfo == nil || extra.TokenInfo.UserID == "" {
		return "", errNoCaller
	}
	return extra.TokenInfo.UserID, nil
}
