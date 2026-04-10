// token.go — GCP token refresh (replaces gcp-token-refresh binary).
//
// Reads ~/.config/gcloud/application_default_credentials.json, uses the
// refresh token to get a fresh access token. Falls back to SA key from
// macOS Keychain when RAPT expires.

package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const gcpTokenEndpoint = "https://oauth2.googleapis.com/token"

type gcpADC struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	QuotaProject string `json:"quota_project_id,omitempty"`
}

type gcpTokenResp struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

func cmdToken(args []string) {
	var (
		check    bool
		identity bool
		audience string
		jsonOut  bool
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--check":
			check = true
		case "--identity":
			identity = true
		case "--audience":
			if i+1 < len(args) {
				i++
				audience = args[i]
			}
		case "--json":
			jsonOut = true
		}
	}

	creds, err := loadGCPADC()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai token: %v\n", err)
		os.Exit(1)
	}

	if check {
		tok, err := gcpRefreshToken(creds)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ADC invalid: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "ADC valid — token expires in %ds\n", tok.ExpiresIn)
		return
	}

	if identity {
		if audience == "" {
			fmt.Fprintln(os.Stderr, "usage: qai token --identity --audience <url>")
			os.Exit(1)
		}
		tok, err := gcpIdentityToken(creds, audience)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai token: %v\n", err)
			os.Exit(1)
		}
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(tok)
		} else {
			fmt.Print(tok.IDToken)
		}
		return
	}

	tok, err := gcpRefreshToken(creds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai token: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		out := map[string]any{
			"access_token": tok.AccessToken,
			"token_type":   tok.TokenType,
			"expires_in":   tok.ExpiresIn,
			"expires_at":   time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	} else {
		fmt.Print(tok.AccessToken)
	}
}

func loadGCPADC() (*gcpADC, error) {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
	if v := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); v != "" {
		path = v
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ADC: %w (run: gcloud auth application-default login)", err)
	}

	var creds gcpADC
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse ADC: %w", err)
	}
	return &creds, nil
}

func gcpRefreshToken(creds *gcpADC) (*gcpTokenResp, error) {
	params := url.Values{
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"refresh_token": {creds.RefreshToken},
		"grant_type":    {"refresh_token"},
	}

	resp, err := http.PostForm(gcpTokenEndpoint, params)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		// RAPT expired — try SA key from Keychain
		if strings.Contains(string(body), "invalid_rapt") || strings.Contains(string(body), "invalid_grant") {
			fmt.Fprintln(os.Stderr, "ADC RAPT expired — falling back to SA key from Keychain")
			return gcpRefreshViaSA()
		}
		return nil, fmt.Errorf("token refresh %d: %s (run: gcloud auth application-default login)", resp.StatusCode, truncateStr(string(body), 200))
	}

	var tok gcpTokenResp
	json.Unmarshal(body, &tok)
	return &tok, nil
}

func gcpRefreshViaSA() (*gcpTokenResp, error) {
	out, err := readCredentialFromKeystore()
	if err != nil {
		return nil, err
	}

	keyJSON, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, fmt.Errorf("decode SA key: %w", err)
	}

	var saKey struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	json.Unmarshal(keyJSON, &saKey)

	fmt.Fprintf(os.Stderr, "Using SA: %s\n", saKey.ClientEmail)

	// Sign JWT
	now := time.Now()
	claims := map[string]any{
		"iss":   saKey.ClientEmail,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   saKey.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}

	jwt, err := gcpSignJWT(claims, saKey.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("sign JWT: %w", err)
	}

	// Exchange JWT for token
	params := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {jwt},
	}
	resp, err := http.PostForm(saKey.TokenURI, params)
	if err != nil {
		return nil, fmt.Errorf("SA token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("SA token exchange %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	// Activate SA in gcloud CLI too
	gcpActivateSA(keyJSON)

	var tok gcpTokenResp
	json.Unmarshal(body, &tok)
	return &tok, nil
}

func gcpActivateSA(keyJSON []byte) {
	tmpFile, err := os.CreateTemp("", "sa-key-*.json")
	if err != nil {
		return
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Write(keyJSON)
	tmpFile.Close()

	cmd := exec.Command("gcloud", "auth", "activate-service-account", "--key-file="+tmpFile.Name())
	cmd.Stderr = os.Stderr
	cmd.Run()
	fmt.Fprintln(os.Stderr, "gcloud CLI now using SA")
}

func gcpIdentityToken(creds *gcpADC, audience string) (*gcpTokenResp, error) {
	// Validate ADC first
	_, err := gcpRefreshToken(creds)
	if err != nil {
		return nil, err
	}

	params := url.Values{
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"refresh_token": {creds.RefreshToken},
		"grant_type":    {"refresh_token"},
		"audience":      {audience},
	}

	resp, err := http.PostForm(gcpTokenEndpoint, params)
	if err != nil {
		return nil, fmt.Errorf("identity token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("identity token %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	var tok gcpTokenResp
	json.Unmarshal(body, &tok)
	return &tok, nil
}

func gcpSignJWT(claims map[string]any, privateKeyPEM string) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("decode PEM block")
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("not RSA key")
	}

	header := gcpB64(mustJSONBytes(map[string]string{"alg": "RS256", "typ": "JWT"}))
	payload := gcpB64(mustJSONBytes(claims))
	signingInput := header + "." + payload

	hash := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + gcpB64(sig), nil
}

func gcpB64(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func mustJSONBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
