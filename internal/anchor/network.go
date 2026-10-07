package anchor

import "github.com/stellar/go/network"

// NetworkPassphrase resolves Nexora's configured network name ("testnet",
// "mainnet"/"public") to the Stellar network passphrase used to validate and
// sign SEP-10 challenges.
func NetworkPassphrase(stellarNetwork string) string {
	if stellarNetwork == "mainnet" || stellarNetwork == "public" {
		return network.PublicNetworkPassphrase
	}
	return network.TestNetworkPassphrase
}
