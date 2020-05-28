/*
Copyright SecureKey Technologies Inc. All Rights Reserved.
SPDX-License-Identifier: Apache-2.0
*/

package operation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/coreos/go-oidc"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ory/hydra-client-go/client/admin"
	"github.com/ory/hydra-client-go/models"
	"github.com/stretchr/testify/require"

	"github.com/trustbloc/edge-adapter/pkg/db"
	"github.com/trustbloc/edge-adapter/pkg/presentationex"
)

func TestGetRESTHandlers(t *testing.T) {
	c, err := New(&Config{})
	require.NoError(t, err)

	require.Equal(t, 6, len(c.GetRESTHandlers()))
}

func TestHydraLoginHandler(t *testing.T) {
	t.Run("TODO - implement redirect to OIDC provider", func(t *testing.T) {
		o, err := New(&Config{
			OAuth2Config: &stubOAuth2Config{},
			Hydra: &stubHydra{
				loginRequestFunc: func(*admin.GetLoginRequestParams) (*admin.GetLoginRequestOK, error) {
					return &admin.GetLoginRequestOK{
						Payload: &models.LoginRequest{
							Skip: false,
						},
					}, nil
				},
				acceptLoginFunc: func(*admin.AcceptLoginRequestParams) (*admin.AcceptLoginRequestOK, error) {
					return &admin.AcceptLoginRequestOK{
						Payload: &models.CompletedRequest{
							RedirectTo: "http://test.hydra.com",
						},
					}, nil
				},
			},
		})
		require.NoError(t, err)

		r := &httptest.ResponseRecorder{}
		o.hydraLoginHandler(r, newHydraRequest(t))

		require.Equal(t, http.StatusFound, r.Code)
	})
	t.Run("redirects back to hydra when skipping", func(t *testing.T) {
		const redirectURL = "http://redirect.com"
		o, err := New(&Config{
			Hydra: &stubHydra{
				loginRequestFunc: func(*admin.GetLoginRequestParams) (*admin.GetLoginRequestOK, error) {
					return &admin.GetLoginRequestOK{
						Payload: &models.LoginRequest{
							Skip: true,
						},
					}, nil
				},
				acceptLoginFunc: func(*admin.AcceptLoginRequestParams) (*admin.AcceptLoginRequestOK, error) {
					return &admin.AcceptLoginRequestOK{
						Payload: &models.CompletedRequest{
							RedirectTo: redirectURL,
						},
					}, nil
				},
			},
		})
		require.NoError(t, err)
		w := &httptest.ResponseRecorder{}
		o.hydraLoginHandler(w, newHydraRequest(t))
		require.Equal(t, http.StatusFound, w.Code)
		require.Equal(t, w.Header().Get("Location"), redirectURL)
	})
	t.Run("fails on missing login_challenge", func(t *testing.T) {
		o, err := New(&Config{})
		require.NoError(t, err)
		r := newHydraRequestNoChallenge(t)
		r.URL.Query().Del("login_challenge")
		w := &httptest.ResponseRecorder{}
		o.hydraLoginHandler(w, r)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})
	t.Run("error while fetching hydra login request", func(t *testing.T) {
		o, err := New(&Config{
			Hydra: &stubHydra{
				loginRequestFunc: func(*admin.GetLoginRequestParams) (*admin.GetLoginRequestOK, error) {
					return nil, errors.New("test")
				},
			},
		})
		require.NoError(t, err)
		w := &httptest.ResponseRecorder{}
		o.hydraLoginHandler(w, newHydraRequest(t))
		require.Equal(t, http.StatusInternalServerError, w.Code)
	})
	t.Run("error while accepting login request at hydra", func(t *testing.T) {
		o, err := New(&Config{
			Hydra: &stubHydra{
				loginRequestFunc: func(*admin.GetLoginRequestParams) (*admin.GetLoginRequestOK, error) {
					return &admin.GetLoginRequestOK{
						Payload: &models.LoginRequest{
							Skip: true,
						},
					}, nil
				},
				acceptLoginFunc: func(*admin.AcceptLoginRequestParams) (*admin.AcceptLoginRequestOK, error) {
					return nil, errors.New("test")
				},
			},
		})
		require.NoError(t, err)
		w := &httptest.ResponseRecorder{}
		o.hydraLoginHandler(w, newHydraRequest(t))
		require.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

func TestOidcCallbackHandler(t *testing.T) {
	t.Run("redirects to hydra", func(t *testing.T) {
		const redirectURL = "http://hydra.example.com"
		const code = "test_code"
		const clientID = "test_client_id"

		c, err := New(&Config{
			OAuth2Config: &stubOAuth2Config{
				clientID: clientID,
			},
			OIDC: func(c string, _ context.Context) (*oidc.IDToken, error) {
				require.Equal(t, code, c)
				return &oidc.IDToken{Subject: "test"}, nil
			},
			Hydra: &stubHydra{
				acceptLoginFunc: func(*admin.AcceptLoginRequestParams) (*admin.AcceptLoginRequestOK, error) {
					return &admin.AcceptLoginRequestOK{
						Payload: &models.CompletedRequest{RedirectTo: redirectURL},
					}, nil
				},
			},
			TrxProvider: func(context.Context, *sql.TxOptions) (Trx, error) {
				return &stubTrx{}, nil
			},
			UsersDAO:        &stubUsersDAO{},
			OIDCrequestsDAO: &stubOidcRequestsDAO{},
		})
		require.NoError(t, err)

		const state = "123"

		c.setLoginRequestForState(state, &models.LoginRequest{})

		r := &httptest.ResponseRecorder{}
		c.oidcCallbackHandler(r, newOidcCallbackRequest(t, state, code))

		require.Equal(t, http.StatusFound, r.Code)
		require.Equal(t, redirectURL, r.Header().Get("Location"))
	})

	t.Run("bad request on invalid state", func(t *testing.T) {
		c, err := New(&Config{})
		require.NoError(t, err)

		r := &httptest.ResponseRecorder{}
		c.oidcCallbackHandler(r, newOidcCallbackRequest(t, "invalid_state", "code"))

		require.Equal(t, http.StatusBadRequest, r.Code)
	})

	t.Run("internal error if exchanging code for id_token fails", func(t *testing.T) {
		c, err := New(&Config{
			OAuth2Config: &stubOAuth2Config{},
			OIDC: func(string, context.Context) (*oidc.IDToken, error) {
				return nil, errors.New("test")
			},
		})
		require.NoError(t, err)

		const state = "123"

		c.setLoginRequestForState(state, &models.LoginRequest{})

		r := &httptest.ResponseRecorder{}
		c.oidcCallbackHandler(r, newOidcCallbackRequest(t, state, "code"))

		require.Equal(t, http.StatusInternalServerError, r.Code)
	})

	t.Run("internal error if cannot open DB transaction", func(t *testing.T) {
		c, err := New(&Config{
			OAuth2Config: &stubOAuth2Config{},
			OIDC: func(string, context.Context) (*oidc.IDToken, error) {
				return &oidc.IDToken{Subject: "test"}, nil
			},
			TrxProvider: func(context.Context, *sql.TxOptions) (Trx, error) {
				return nil, errors.New("test")
			},
		})
		require.NoError(t, err)

		const state = "123"

		c.setLoginRequestForState(state, &models.LoginRequest{})

		r := &httptest.ResponseRecorder{}
		c.oidcCallbackHandler(r, newOidcCallbackRequest(t, state, "code"))

		require.Equal(t, http.StatusInternalServerError, r.Code)
	})

	t.Run("internal server error if hydra fails to accept login", func(t *testing.T) {
		c, err := New(&Config{
			OAuth2Config: &stubOAuth2Config{},
			OIDC: func(c string, _ context.Context) (*oidc.IDToken, error) {
				return &oidc.IDToken{Subject: "test"}, nil
			},
			Hydra: &stubHydra{
				acceptLoginFunc: func(*admin.AcceptLoginRequestParams) (*admin.AcceptLoginRequestOK, error) {
					return nil, errors.New("test")
				},
			},
			TrxProvider:     func(context.Context, *sql.TxOptions) (Trx, error) { return &stubTrx{}, nil },
			UsersDAO:        &stubUsersDAO{},
			OIDCrequestsDAO: &stubOidcRequestsDAO{},
		})
		require.NoError(t, err)

		const state = "123"

		c.setLoginRequestForState(state, &models.LoginRequest{})

		r := &httptest.ResponseRecorder{}
		c.oidcCallbackHandler(r, newOidcCallbackRequest(t, state, "code"))

		require.Equal(t, http.StatusInternalServerError, r.Code)
	})
}

func TestSaveUserAndRequest(t *testing.T) {
	t.Run("error when inserting user", func(t *testing.T) {
		c, err := New(&Config{
			OAuth2Config: &stubOAuth2Config{},
			OIDC: func(c string, _ context.Context) (*oidc.IDToken, error) {
				return &oidc.IDToken{Subject: "test"}, nil
			},
			TrxProvider: func(context.Context, *sql.TxOptions) (Trx, error) { return &stubTrx{}, nil },
			UsersDAO: &stubUsersDAO{
				insertErr: errors.New("test"),
			},
		})
		require.NoError(t, err)

		err = c.saveUserAndRequest(
			context.Background(),
			&models.LoginRequest{},
			"sub",
		)
		require.Error(t, err)
	})

	t.Run("error when inserting oidc request", func(t *testing.T) {
		c, err := New(&Config{
			OAuth2Config: &stubOAuth2Config{},
			OIDC: func(c string, _ context.Context) (*oidc.IDToken, error) {
				return &oidc.IDToken{Subject: "test"}, nil
			},
			TrxProvider: func(context.Context, *sql.TxOptions) (Trx, error) { return &stubTrx{}, nil },
			UsersDAO:    &stubUsersDAO{},
			OIDCrequestsDAO: &stubOidcRequestsDAO{
				insertErr: errors.New("test"),
			},
		})
		require.NoError(t, err)

		err = c.saveUserAndRequest(
			context.Background(),
			&models.LoginRequest{},
			"sub",
		)
		require.Error(t, err)
	})
}

func TestHydraConsentHandler(t *testing.T) {
	c, err := New(&Config{})
	require.NoError(t, err)

	r := &httptest.ResponseRecorder{}
	c.hydraConsentHandler(r, nil)

	require.Equal(t, http.StatusOK, r.Code)
}

func TestCreatePresentationDefinition(t *testing.T) {
	t.Run("test success", func(t *testing.T) {
		c, err := New(&Config{PresentationExProvider: &mockPresentationExProvider{
			createValue: &presentationex.PresentationDefinitions{
				InputDescriptors: []presentationex.InputDescriptors{{ID: "1"}}}}})
		require.NoError(t, err)

		reqBytes, err := json.Marshal(CreatePresentationDefinitionReq{Scopes: []string{"scope1", "scope2"}})
		require.NoError(t, err)

		r := httptest.NewRecorder()
		c.createPresentationDefinition(r, &http.Request{Body: ioutil.NopCloser(bytes.NewReader(reqBytes))})

		require.Equal(t, http.StatusOK, r.Code)

		var resp presentationex.PresentationDefinitions
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &resp))

		require.Equal(t, "1", resp.InputDescriptors[0].ID)
	})

	t.Run("test failure from create presentation definition request", func(t *testing.T) {
		c, err := New(&Config{PresentationExProvider: &mockPresentationExProvider{
			createErr: fmt.Errorf("failed to create presentation definition request")}})
		require.NoError(t, err)

		reqBytes, err := json.Marshal(CreatePresentationDefinitionReq{Scopes: []string{"scope1", "scope2"}})
		require.NoError(t, err)

		r := httptest.NewRecorder()
		c.createPresentationDefinition(r, &http.Request{Body: ioutil.NopCloser(bytes.NewReader(reqBytes))})

		require.Equal(t, http.StatusBadRequest, r.Code)
		require.Contains(t, r.Body.String(), "failed to create presentation definition request")
	})

	t.Run("test failure from decode request", func(t *testing.T) {
		c, err := New(&Config{})
		require.NoError(t, err)

		r := httptest.NewRecorder()
		c.createPresentationDefinition(r, &http.Request{Body: ioutil.NopCloser(bytes.NewReader([]byte("w")))})

		require.Equal(t, http.StatusBadRequest, r.Code)
		require.Contains(t, r.Body.String(), "invalid request")
	})
}

func TestPresentationResponseHandler(t *testing.T) {
	c, err := New(&Config{})
	require.NoError(t, err)

	r := &httptest.ResponseRecorder{}
	c.presentationResponseHandler(r, nil)

	require.Equal(t, http.StatusOK, r.Code)
}

func TestUserInfoHandler(t *testing.T) {
	c, err := New(&Config{})
	require.NoError(t, err)

	r := &httptest.ResponseRecorder{}
	c.userInfoHandler(r, nil)

	require.Equal(t, http.StatusOK, r.Code)
}

func TestTestResponse(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		testResponse(&stubWriter{})
	})
}

type stubWriter struct {
}

func (s *stubWriter) Write(p []byte) (n int, err error) {
	return -1, errors.New("test")
}

type mockPresentationExProvider struct {
	createValue *presentationex.PresentationDefinitions
	createErr   error
}

func (m *mockPresentationExProvider) Create(scopes []string) (*presentationex.PresentationDefinitions, error) {
	return m.createValue, m.createErr
}

func newHydraRequest(t *testing.T) *http.Request {
	u, err := url.Parse("http://example.com?login_challenge=" + uuid.New().String())
	require.NoError(t, err)

	return &http.Request{URL: u}
}

func newOidcCallbackRequest(t *testing.T, state, code string) *http.Request {
	u, err := url.Parse(fmt.Sprintf("http://example.com?state=%s&code=%s", state, code))
	require.NoError(t, err)

	return &http.Request{URL: u}
}

func newHydraRequestNoChallenge(t *testing.T) *http.Request {
	u, err := url.Parse("http://example.com")
	require.NoError(t, err)

	return &http.Request{
		URL: u,
	}
}

type stubHydra struct {
	loginRequestFunc func(*admin.GetLoginRequestParams) (*admin.GetLoginRequestOK, error)
	acceptLoginFunc  func(*admin.AcceptLoginRequestParams) (*admin.AcceptLoginRequestOK, error)
}

func (s *stubHydra) GetLoginRequest(params *admin.GetLoginRequestParams) (*admin.GetLoginRequestOK, error) {
	return s.loginRequestFunc(params)
}

func (s *stubHydra) AcceptLoginRequest(params *admin.AcceptLoginRequestParams) (*admin.AcceptLoginRequestOK, error) {
	return s.acceptLoginFunc(params)
}

type stubOAuth2Config struct {
	clientID    string
	authCodeURL string
}

func (s *stubOAuth2Config) ClientID() string {
	return s.clientID
}

func (s *stubOAuth2Config) AuthCodeURL(_ string) string {
	return s.authCodeURL
}

type stubTrx struct {
	commitErr   error
	rollbackErr error
}

func (s *stubTrx) Commit() error {
	return s.commitErr
}

func (s stubTrx) Rollback() error {
	return s.rollbackErr
}

type stubUsersDAO struct {
	insertErr  error
	insertFunc func(*db.EndUser) error
}

func (s *stubUsersDAO) Insert(u *db.EndUser) error {
	if s.insertErr != nil {
		return s.insertErr
	}

	if s.insertFunc != nil {
		return s.insertFunc(u)
	}

	return nil
}

type stubOidcRequestsDAO struct {
	insertErr  error
	insertFunc func(*db.OIDCRequest) error
}

func (s *stubOidcRequestsDAO) Insert(r *db.OIDCRequest) error {
	if s.insertErr != nil {
		return s.insertErr
	}

	if s.insertFunc != nil {
		return s.insertFunc(r)
	}

	return nil
}
