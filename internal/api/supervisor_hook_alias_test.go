package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

func standaloneHookHandler(t *testing.T, state *fakeState, verifier *citywriteauth.Verifier) http.Handler {
	t.Helper()
	resolver := &fakeCityResolver{cities: map[string]*fakeState{state.CityName(): state}}
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now()).WithStandaloneCityHookAlias(state.CityName())
	sm.WithAllowedHosts([]string{"example.com"})
	if verifier != nil {
		sm.WithWriteAuth(verifier)
	}
	return sm.Handler()
}

func TestStandaloneCityHookAliasMatchesCanonicalWebhook(t *testing.T) {
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "alias-hmac-secret-0001")
	secret := []byte("alias-hmac-secret-0001")
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "alias", path: "/hook/github?source=alias"},
		{name: "canonical", path: "/v0/city/test-city/hook/github?source=canonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := firedDispatcher()
			state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
			h := standaloneHookHandler(t, state, nil)
			body := []byte(prLabeledPayload)
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
			req.RemoteAddr = "203.0.113.7:443"
			req.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("X-GitHub-Delivery", tc.name)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("valid signature status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
			}
			if disp.count() != 1 {
				t.Fatalf("valid signature dispatch count = %d, want 1", disp.count())
			}
		})
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "alias", path: "/hook/github"},
		{name: "canonical", path: "/v0/city/test-city/hook/github"},
	} {
		t.Run("invalid-hmac-"+tc.name, func(t *testing.T) {
			disp := firedDispatcher()
			state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
			h := standaloneHookHandler(t, state, nil)
			body := []byte(prLabeledPayload)
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
			req.RemoteAddr = "203.0.113.7:443"
			req.Header.Set("X-Hub-Signature-256", githubSignature([]byte("wrong-secret-000000"), body))
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("X-GitHub-Delivery", "invalid-"+tc.name)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("invalid signature status = %d, want 401", rec.Code)
			}
			if disp.count() != 0 {
				t.Fatalf("invalid signature dispatched %d times, want 0", disp.count())
			}
		})
	}
}

func TestStandaloneCityHookAliasPreservesRequestAndCanonicalizesBeforeWriteAuth(t *testing.T) {
	const city = "test-city"
	t.Setenv("GC_WEBHOOK_GITHUB_SECRET", "alias-writeauth-secret")
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	verifier := newTestWriteVerifier(t, pub, now)
	disp := firedDispatcher()
	state := newWebhookState(t, githubWebhook("public"), prReviewOrder(), disp)
	h := standaloneHookHandler(t, state, verifier)
	body := []byte(prLabeledPayload)
	canonicalPath := "/v0/city/" + city + "/hook/github"
	query := "delivery=with-query"

	// The alias is subject to the same write-auth gate as the canonical route.
	missing := httptest.NewRequest(http.MethodPost, "/hook/github?"+query, bytes.NewReader(body))
	missing.Header.Set("X-Hub-Signature-256", githubSignature([]byte("alias-writeauth-secret"), body))
	missing.Header.Set("X-GitHub-Event", "pull_request")
	missingRec := httptest.NewRecorder()
	h.ServeHTTP(missingRec, missing)
	if missingRec.Code != http.StatusUnauthorized {
		t.Fatalf("alias without write grant status = %d, want 401", missingRec.Code)
	}
	if disp.count() != 0 {
		t.Fatalf("alias without write grant dispatched %d times, want 0", disp.count())
	}

	for i, path := range []string{"/hook/github?" + query, canonicalPath + "?" + query} {
		requestBody := body
		if i == 1 {
			// The webhook deduplicates signature-covered bodies across both URLs.
			// A trailing JSON space preserves the event while making this a fresh delivery.
			requestBody = append(append([]byte(nil), body...), ' ')
		}
		tok := mintToken(t, priv, grantForQuery(now, city, http.MethodPost, canonicalPath, query, requestBody, "alias-jti-"+string(rune('1'+i))))
		reqCtx := context.WithValue(context.Background(), hookAliasContextKey{}, "context-preserved")
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(requestBody)).WithContext(reqCtx)
		req.Header.Set(writeAuthHeader, tok)
		req.Header.Set(csrfHeaderName, "1")
		req.Header.Set("X-Hub-Signature-256", githubSignature([]byte("alias-writeauth-secret"), requestBody))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", "writeauth-"+string(rune('1'+i)))
		req.RemoteAddr = "203.0.113.7:443"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %q with valid write and HMAC grants status = %d, want 202 (body %s)", path, rec.Code, rec.Body.String())
		}
	}
	if disp.count() != 2 {
		t.Fatalf("valid alias and canonical requests dispatched %d times, want 2", disp.count())
	}
}

type hookAliasContextKey struct{}

func TestStandaloneCityHookAliasCopiesContextHeadersBodyAndQuery(t *testing.T) {
	type observed struct {
		method, path, rawPath, query, header, context string
		body                                          []byte
	}
	var got observed
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.rawPath, got.query = r.Method, r.URL.Path, r.URL.RawPath, r.URL.RawQuery
		got.header = r.Header.Get("X-Preserve")
		got.context, _ = r.Context().Value(hookAliasContextKey{}).(string)
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	h := standaloneCityHookAlias("city name", next)
	body := []byte("raw signed bytes\x00")
	req := httptest.NewRequest(http.MethodPut, "/hook/receiver?x=1", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), hookAliasContextKey{}, "kept"))
	req.Header.Set("X-Preserve", "header")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got.method != http.MethodPut || got.path != "/v0/city/city name/hook/receiver" || got.rawPath != "/v0/city/city%20name/hook/receiver" || got.query != "x=1" || got.header != "header" || got.context != "kept" || !bytes.Equal(got.body, body) {
		t.Fatalf("request changed during alias rewrite: %+v", got)
	}
}

func TestStandaloneCityHookAliasIsAbsentForMultiCitySupervisor(t *testing.T) {
	alpha := newFakeState(t)
	alpha.cityName = "alpha"
	beta := newFakeState(t)
	beta.cityName = "beta"
	resolver := &fakeCityResolver{cities: map[string]*fakeState{"alpha": alpha, "beta": beta}}
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now()).WithStandaloneCityHookAlias("alpha")
	h := wrapTestSupervisorMiddleware(sm)
	req := httptest.NewRequest(http.MethodPost, "/hook/github", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("multi-city alias status = %d, want 404", rec.Code)
	}
	if sm.standaloneCityHookAlias != "" {
		t.Fatalf("multi-city supervisor installed alias for %q", sm.standaloneCityHookAlias)
	}
}
