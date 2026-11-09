// Package upload sends a reviewed inventory file to the komolos control plane.
//
// Authentication is a short-lived, single-use upload token the customer creates
// in the dashboard and pastes in at run time. Tokens are never baked into the
// binary or written to disk by the collector.
package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/komolos-io/komolos-agent/internal/inventory"
)

// TokenPrefix marks komolos upload tokens so a pasted value of the wrong kind
// is rejected locally instead of being sent anywhere.
const TokenPrefix = "kup_"

const Path = "/api/v1/komolos-intake/inventory"

type Result struct {
	InventoryID string `json:"inventoryId"`
	SHA256      string `json:"sha256"`
}

// Send uploads doc (the exact bytes the customer reviewed) and checks that the
// server received the same digest.
func Send(ctx context.Context, endpoint, token string, doc []byte) (*Result, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, TokenPrefix) {
		return nil, errors.New("that is not a komolos upload token (they start with " + TokenPrefix + ")")
	}
	if _, err := inventory.Decode(doc); err != nil {
		return nil, fmt.Errorf("refusing to upload an invalid file: %w", err)
	}
	u, err := url.Parse(strings.TrimRight(endpoint, "/") + Path)
	if err != nil {
		return nil, fmt.Errorf("bad endpoint: %w", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocal(u.Hostname())) {
		return nil, errors.New("endpoint must use https")
	}
	digest := inventory.SHA256(doc)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(doc))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Inventory-SHA256", digest)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("control plane returned %d: %s", resp.StatusCode, msg)
	}
	var res Result
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	if res.SHA256 != digest {
		return nil, fmt.Errorf("server recorded digest %s but the file is %s", res.SHA256, digest)
	}
	return &res, nil
}

func isLocal(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
