//go:build testnet

package renter

import "github.com/siacentral/apisdkgo/sia"

const duration = 10 * 144 // 10 days

func apiClient() *sia.APIClient {
	return &sia.APIClient{
		BaseAddress: "https://api.siacentral.com/v2/zen",
	}
}
