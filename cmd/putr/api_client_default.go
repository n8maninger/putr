//go:build !testnet

package main

import "github.com/siacentral/apisdkgo/sia"

const duration = 144 * 30 * 3 // 3 months

func apiClient() *sia.APIClient {
	return &sia.APIClient{
		BaseAddress: "https://api.siacentral.com/v2",
	}
}
