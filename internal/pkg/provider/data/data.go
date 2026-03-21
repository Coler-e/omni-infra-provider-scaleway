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

// ZoneConfig declares a zone and an optional instance type override.
type ZoneConfig struct {
	Zone           string `yaml:"zone"`
	CommercialType string `yaml:"commercial_type,omitempty"`
}

// Region declares zones and an optional private network for a Scaleway region.
type Region struct {
	Region         string       `yaml:"region"`
	Zones          []ZoneConfig `yaml:"zones"`
	CommercialType string       `yaml:"commercial_type,omitempty"`
	NetworkID      string       `yaml:"network_id,omitempty"`
}

// Data and schema.json should be in sync.
type Data struct {
	Zone           string   `yaml:"zone"`
	Regions        []Region `yaml:"regions,omitempty"`
	CommercialType string   `yaml:"commercial_type"`
	ImageID        string   `yaml:"image_id,omitempty"`
	ImageName      string   `yaml:"image_name,omitempty"`
	Arch           string   `yaml:"arch,omitempty"` // amd64 (default) or arm64
	DiskSizeGB     uint64   `yaml:"disk_size_gb,omitempty"`
	Tags           []string `yaml:"tags,omitempty"`
}

// GetZone returns the zone to use for single-zone configs.
// Falls back to Zone, then to the provider-level default.
func (d *Data) GetZone(defaultZone string) string {
	if d.Zone != "" {
		return d.Zone
	}

	return defaultZone
}

// AllZones returns the flat list of all zone names across all declared regions.
func (d *Data) AllZones() []string {
	var zones []string
	for _, r := range d.Regions {
		for _, z := range r.Zones {
			zones = append(zones, z.Zone)
		}
	}

	return zones
}

// CommercialTypeForZone returns the most specific commercial type for the given zone.
// Precedence: zone-level > region-level > global.
func (d *Data) CommercialTypeForZone(zone string) string {
	region := zoneToRegion(zone)

	for _, r := range d.Regions {
		if r.Region != region {
			continue
		}

		for _, z := range r.Zones {
			if z.Zone == zone && z.CommercialType != "" {
				return z.CommercialType
			}
		}

		if r.CommercialType != "" {
			return r.CommercialType
		}
	}

	return d.CommercialType
}

// NetworkIDForZone returns the private network ID for the region containing the given zone, if any.
func (d *Data) NetworkIDForZone(zone string) string {
	region := zoneToRegion(zone)
	for _, r := range d.Regions {
		if r.Region == region {
			return r.NetworkID
		}
	}

	return ""
}

// zoneToRegion strips the zone suffix to get the region, e.g. "fr-par-1" → "fr-par".
func zoneToRegion(zone string) string {
	for i := len(zone) - 1; i >= 0; i-- {
		if zone[i] == '-' {
			return zone[:i]
		}
	}

	return zone
}
