// Package psp is the HTTP client for the payment processor. The PSP is treated
// as a real external dependency: it can be slow, fail, or answer garbage.
//
// Every outcome is mapped to one of four statuses. Anything we can't be sure
// about is Unknown, never Failed, so we never tell a customer "declined" for a
// charge that may have gone through.
package psp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Status string

const (
	Succeeded Status = "succeeded" // PSP confirmed the charge
	Failed    Status = "failed"    // PSP definitively declined
	Unknown   Status = "unknown"   // timeout, network error, 5xx, bad response, still processing
	NotFound  Status = "not_found" // Lookup only: the PSP has never seen this reference
)

type Result struct {
	Status Status
	PSPRef string
	Code   string
	Err    error // why the result is Unknown; for logs only
}

type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a client with a hard per-request timeout. The timeout is what
// stops a 30s PSP hang from pinning our connections.
func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Charge asks the PSP to charge. The reference makes the call idempotent at the
// PSP: the same reference never charges twice.
func (c *Client) Charge(ctx context.Context, reference, cardToken string, amountCents int64) Result {
	body, _ := json.Marshal(map[string]any{
		"reference":    reference,
		"card_token":   cardToken,
		"amount_cents": amountCents,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/charges", bytes.NewReader(body))
	if err != nil {
		return Result{Status: Unknown, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, false)
}

// Lookup asks what happened to an earlier charge (used by the reconciler).
// 200 gives the result, 404 gives NotFound, "still processing" gives Unknown.
func (c *Client) Lookup(ctx context.Context, reference string) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/charges/"+url.PathEscape(reference), nil)
	if err != nil {
		return Result{Status: Unknown, Err: err}
	}
	return c.do(req, true)
}

func (c *Client) do(req *http.Request, lookup bool) Result {
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{Status: Unknown, Err: err} // timeout, refused, connection dropped
	}
	defer resp.Body.Close()

	if lookup && resp.StatusCode == http.StatusNotFound {
		return Result{Status: NotFound}
	}
	if resp.StatusCode != http.StatusOK {
		return Result{Status: Unknown, Err: fmt.Errorf("psp returned HTTP %d", resp.StatusCode)}
	}

	var out struct {
		Status string `json:"status"`
		PSPRef string `json:"psp_ref"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return Result{Status: Unknown, Err: fmt.Errorf("psp response not valid JSON: %w", err)}
	}

	switch {
	case out.Status == "succeeded" && out.PSPRef != "":
		return Result{Status: Succeeded, PSPRef: out.PSPRef}
	case out.Status == "failed" && out.Code != "":
		return Result{Status: Failed, Code: out.Code}
	default:
		return Result{Status: Unknown, Err: fmt.Errorf("unexpected psp response: status=%q", out.Status)}
	}
}
