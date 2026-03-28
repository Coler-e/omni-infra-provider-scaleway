// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package meta contains meta information about the provider.
package meta

import "os"

// ProviderID is the ID of the provider.
var ProviderID = getEnvOrDefault("PROVIDER_ID", "scaleway")

func getEnvOrDefault(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}

	return fallback
}
