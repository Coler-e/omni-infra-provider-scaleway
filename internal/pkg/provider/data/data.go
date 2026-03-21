// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package data defines the structures and interfaces for custom provider data.
// Each provider can have its own machine configuration schema.
// When a provider starts, it reports its data schema back to Omni.
// Omni then uses this schema to render the appropriate UI and validate MachineRequests.
package data

import (
	_ "embed"
)

//go:embed schema.json
var Schema []byte

// Data and schema.json should be in sync.
type Data struct {
	Zone              string            `yaml:"zone"`
	Zones             []string          `yaml:"zones,omitempty"`
	CommercialType    string            `yaml:"commercial_type"`
	ImageID           string            `yaml:"image_id,omitempty"`
	ImageName         string            `yaml:"image_name,omitempty"`
	Arch              string            `yaml:"arch,omitempty"` // amd64 (default) or arm64
	DiskSizeGB        uint64            `yaml:"disk_size_gb,omitempty"`
	Tags              []string          `yaml:"tags,omitempty"`
	PrivateNetworkIDs map[string]string `yaml:"private_network_ids,omitempty"` // region (e.g. "fr-par") → private network UUID
}

// GetZone returns the zone to use for single-zone configs.
// Falls back to Zone, then to the provider-level default.
func (d *Data) GetZone(defaultZone string) string {
	if d.Zone != "" {
		return d.Zone
	}

	return defaultZone
}
