package oauth2cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/browser"
	"golang.org/x/oauth2"
)

// AuthCodeFlow provides flow with OAuth 2.0 Authorization Code Grant.
// See https://tools.ietf.org/html/rfc6749#section-4.1
type AuthCodeFlow struct {
	Config          oauth2.Config           // OAuth2 config.
	AuthCodeOptions []oauth2.AuthCodeOption // Options passed to AuthCodeURL().
	LocalServerPort int                     // Local server port. Default to a random port.
	SkipOpenBrowser bool                    // Skip opening browser if it is true.

	ShowLocalServerURL func(url string) // Called when the local server is started. Default to show a message via the logger.
}

// GetToken performs Authorization Grant Flow and returns a token got from the provider.
//
// This does the following steps:
//
// 1. Start a local server at the port.
// 2. Open browser and navigate to the local server.
// 3. Wait for user authorization.
// 4. Receive a code via an authorization response (HTTP redirect).
// 5. Exchange the code and a token.
// 6. Return the code.
//
// Note that this will change Config.RedirectURL to "http://localhost:port" if it is empty.
//
func (f *AuthCodeFlow) GetToken(ctx context.Context) (*TokenJSON, error) {
	listener, err := newLocalhostListener(f.LocalServerPort)
	if err != nil {
		return nil, fmt.Errorf("Could not listen to port: %s", err)
	}
	defer listener.Close()
	if f.Config.RedirectURL == "" {
		f.Config.RedirectURL = listener.URL
	}
	code, err := f.getCode(ctx, listener)
	if err != nil {
		return nil, fmt.Errorf("Could not get an auth code: %s", err)
	}
	// token, err := f.Config.Exchange(ctx, code)
	token, err := exchangeWithBasicAuth(f.Config, code, f.Config.RedirectURL)
	if err != nil {
		return nil, fmt.Errorf("Could not exchange token: %s", err)
	}
	return token, nil
}

func exchangeWithBasicAuth(config oauth2.Config, code string, redirectURL string) (*TokenJSON, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", redirectURL)

	// urlStr := config.Endpoint.Token + "/token"

	log.Printf("Token URL is ", config.Endpoint.TokenURL)

	client := &http.Client{}
	request, err := http.NewRequest("POST", config.Endpoint.TokenURL, strings.NewReader(data.Encode())) // URL-encoded payload
	if err != nil {
		return nil, err
	}

	clientIDSecret := []byte(config.ClientID + ":" + config.ClientSecret)
	basicAuth := base64.StdEncoding.EncodeToString(clientIDSecret)

	request.Header.Add("Authorization", "Basic "+basicAuth)
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	r, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()

	fmt.Println(r.Status)

	body, err := ioutil.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("oauth2: cannot fetch token: %v", err)
	}
	if code := r.StatusCode; code < 200 || code > 299 {
		return nil, &RetrieveError{
			Response: r,
			Body:     body,
		}
	}

	var tj TokenJSON
	if err = json.Unmarshal(body, &tj); err != nil {
		return nil, err
	}
	return &tj, nil

}

func (f *AuthCodeFlow) getCode(ctx context.Context, listener *localhostListener) (string, error) {
	state, err := newOAuth2State()
	if err != nil {
		return "", fmt.Errorf("Could not generate state parameter: %s", err)
	}
	codeCh := make(chan string)
	defer close(codeCh)
	errCh := make(chan error)
	defer close(errCh)
	server := http.Server{
		Handler: &authCodeFlowHandler{
			authCodeURL: f.Config.AuthCodeURL(string(state), f.AuthCodeOptions...),
			gotCode: func(code string, gotState string) {
				if gotState == state {
					codeCh <- code
				} else {
					errCh <- fmt.Errorf("State does not match, wants %s but %s", state, gotState)
				}
			},
			gotError: func(err error) {
				errCh <- err
			},
		},
	}
	defer server.Shutdown(ctx)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	go func() {
		time.Sleep(500 * time.Millisecond)
		if f.ShowLocalServerURL != nil {
			f.ShowLocalServerURL(listener.URL)
		} else {
			log.Printf("Open %s for authorization", listener.URL)
		}
		if !f.SkipOpenBrowser {
			browser.OpenURL(listener.URL)
		}
	}()
	select {
	case err := <-errCh:
		return "", err
	case code := <-codeCh:
		return code, nil
	case <-ctx.Done():
		return "", fmt.Errorf("Context done while waiting for authorization response: %s", ctx.Err())
	}
}

type authCodeFlowHandler struct {
	authCodeURL string
	gotCode     func(code string, state string)
	gotError    func(err error)
}

func (h *authCodeFlowHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && r.URL.Path == "/" && q.Get("error") != "":
		h.gotError(fmt.Errorf("OAuth Error: %s %s", q.Get("error"), q.Get("error_description")))
		http.Error(w, "OAuth Error", 500)

	case r.Method == "GET" && r.URL.Path == "/" && q.Get("code") != "":
		h.gotCode(q.Get("code"), q.Get("state"))
		w.Header().Add("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body>OK<script>window.close()</script></body></html>`)

	case r.Method == "GET" && r.URL.Path == "/":
		http.Redirect(w, r, h.authCodeURL, 302)

	default:
		http.Error(w, "Not Found", 404)
	}
}

type RetrieveError struct {
	Response *http.Response
	Body     []byte
}

func (r *RetrieveError) Error() string {
	return fmt.Sprintf("oauth2: cannot fetch token: %v\nResponse: %s", r.Response.Status, r.Body)
}

type TokenJSON struct {
	AccessToken  string         `json:"access_token"`
	TokenType    string         `json:"token_type"`
	IdToken      string         `json:"id_token"`
	RefreshToken string         `json:"refresh_token"`
	ExpiresIn    expirationTime `json:"expires_in"` // at least PayPal returns string, while most return number
	Expires      expirationTime `json:"expires"`    // broken Facebook spelling of expires_in
}

type expirationTime int32

func (e *TokenJSON) expiry() (t time.Time) {
	if v := e.ExpiresIn; v != 0 {
		return time.Now().Add(time.Duration(v) * time.Second)
	}
	if v := e.Expires; v != 0 {
		return time.Now().Add(time.Duration(v) * time.Second)
	}
	return
}
