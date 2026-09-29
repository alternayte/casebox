package api

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// DeviceCode is the start of a device login.
type DeviceCode struct {
	DeviceCode      string `json:"deviceCode"`
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationUri"`
	ExpiresIn       int    `json:"expiresIn"`
	Interval        int    `json:"interval"`
}

// DeviceToken is what an approved device login returns, once.
type DeviceToken struct {
	AccessToken string `json:"accessToken"`
	OrgID       string `json:"orgId"`
}

// ErrDeviceCodeExpired means nobody approved the code in time.
var ErrDeviceCodeExpired = errors.New("the code expired before it was approved; run the command again")

// StartDeviceLogin asks the server for a code to show the person.
func (c *Client) StartDeviceLogin(ctx context.Context) (DeviceCode, error) {
	var code DeviceCode
	err := c.Do(ctx, http.MethodPost, "/api/v1/auth/device/code", struct{}{}, &code)
	return code, err
}

// WaitForDeviceToken polls until the person approves the code, the code expires, or ctx ends.
func (c *Client) WaitForDeviceToken(ctx context.Context, code DeviceCode) (DeviceToken, error) {
	interval := time.Duration(code.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		var token DeviceToken
		err := c.Do(ctx, http.MethodPost, "/api/v1/auth/device/token", map[string]string{"deviceCode": code.DeviceCode}, &token)
		switch StatusOf(err) {
		case 0:
			if err != nil {
				return DeviceToken{}, err
			}
			return token, nil
		case http.StatusPreconditionRequired:
		case http.StatusGone:
			return DeviceToken{}, ErrDeviceCodeExpired
		default:
			return DeviceToken{}, err
		}
		select {
		case <-ctx.Done():
			return DeviceToken{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}
