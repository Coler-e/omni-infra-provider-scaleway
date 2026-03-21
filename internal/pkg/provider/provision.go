// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements Scaleway infra provider core.
package provider

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	infraresources "github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-scaleway/internal/pkg/provider/data"
	"github.com/siderolabs/omni-infra-provider-scaleway/internal/pkg/provider/resources"
)

// Provisioner implements Scaleway infra provider.
type Provisioner struct {
	scwClient   *scw.Client
	defaultZone string
}

// NewProvisioner creates a new provisioner.
func NewProvisioner(scwClient *scw.Client, defaultZone string) *Provisioner {
	return &Provisioner{scwClient: scwClient, defaultZone: defaultZone}
}

// ProvisionSteps implements infra.Provisioner.
func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep("validateRequest", func(_ context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if len(pctx.GetRequestID()) > 63 {
				return fmt.Errorf("the machine request name cannot be longer than 63 characters")
			}

			var providerData data.Data

			if err := pctx.UnmarshalProviderData(&providerData); err != nil {
				return err
			}

			if providerData.ImageID == "" && providerData.ImageName == "" {
				return fmt.Errorf("either image_id or image_name must be set")
			}

			if providerData.Arch != "" && providerData.Arch != "amd64" && providerData.Arch != "arm64" {
				return fmt.Errorf("arch must be amd64 or arm64, got %q", providerData.Arch)
			}

			return nil
		}),
		provision.NewStep("createSchematic", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			schematic, err := pctx.GenerateSchematicID(ctx, logger,
				provision.WithoutConnectionParams(),
				provision.WithExtraExtensions("siderolabs/qemu-guest-agent"),
			)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.Schematic = schematic

			return nil
		}),
		provision.NewStep("createServer", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			pctx.State.TypedSpec().Value.TalosVersion = pctx.GetTalosVersion()

			// Idempotent: if server was already created, skip creation.
			if pctx.State.TypedSpec().Value.ServerId != "" {
				return nil
			}

			var providerData data.Data

			err := pctx.UnmarshalProviderData(&providerData)
			if err != nil {
				return err
			}

			var zoneName string
			if len(providerData.Zones) > 0 {
				zoneName, err = pickZone(ctx, instance.NewAPI(p.scwClient), providerData.Zones)
				if err != nil {
					return err
				}
			} else {
				zoneName = providerData.GetZone(p.defaultZone)
			}

			zone, err := scw.ParseZone(zoneName)
			if err != nil {
				return fmt.Errorf("invalid zone %q: %w", zoneName, err)
			}

			instanceAPI := instance.NewAPI(p.scwClient)

			imageID := providerData.ImageID
			if imageID == "" && providerData.ImageName != "" {
				scwArch, err := resolveArch(ctx, instanceAPI, zone, providerData.CommercialType, providerData.Arch)
				if err != nil {
					return err
				}

				imageID, err = resolveImageByName(ctx, instanceAPI, zone, providerData.ImageName, scwArch)
				if err != nil {
					return err
				}
			}

			bootType := instance.BootTypeLocal

			tags := append(providerData.Tags, "omni", "omni-request-id="+pctx.GetRequestID())

			req := &instance.CreateServerRequest{
				Zone:           zone,
				Name:           pctx.GetRequestID(),
				CommercialType: providerData.CommercialType,
				Image:          &imageID,
				Tags:           tags,
				BootType:       &bootType,
			}

			diskSizeGB := providerData.DiskSizeGB
			if diskSizeGB == 0 {
				diskSizeGB = 40
			}

			diskSize := scw.Size(diskSizeGB) * scw.GB
			req.Volumes = map[string]*instance.VolumeServerTemplate{
				"0": {Size: &diskSize},
			}

			createResp, err := instanceAPI.CreateServer(req, scw.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("failed to create Scaleway server: %w", err)
			}

			serverID := createResp.Server.ID

			pctx.State.TypedSpec().Value.ServerId = serverID
			pctx.State.TypedSpec().Value.Zone = zoneName
			pctx.SetMachineInfraID(serverID)

			logger.Info("created Scaleway server", zap.String("server_id", serverID))

			// Attach private NIC if a private network is configured for this region.
			if pnID := providerData.PrivateNetworkIDs[zoneToRegion(zoneName)]; pnID != "" {
				_, err = instanceAPI.CreatePrivateNIC(&instance.CreatePrivateNICRequest{
					Zone:             zone,
					ServerID:         serverID,
					PrivateNetworkID: pnID,
				}, scw.WithContext(ctx))
				if err != nil {
					return fmt.Errorf("failed to attach private NIC (network %s) to server %s: %w", pnID, serverID, err)
				}

				logger.Info("attached private NIC", zap.String("server_id", serverID), zap.String("private_network_id", pnID))
			}

			// Set Talos machine config as cloud-init user-data.
			err = instanceAPI.SetServerUserData(&instance.SetServerUserDataRequest{
				Zone:     zone,
				ServerID: serverID,
				Key:      "cloud-init",
				Content:  io.NopCloser(strings.NewReader(pctx.ConnectionParams.JoinConfig)),
			}, scw.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("failed to set user-data on server %s: %w", serverID, err)
			}

			// Power on the server.
			_, err = instanceAPI.ServerAction(&instance.ServerActionRequest{
				Zone:     zone,
				ServerID: serverID,
				Action:   instance.ServerActionPoweron,
			}, scw.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("failed to power on server %s: %w", serverID, err)
			}

			logger.Info("powered on Scaleway server", zap.String("server_id", serverID))

			return nil
		}),
		provision.NewStep("waitForRunning", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			serverID := pctx.State.TypedSpec().Value.ServerId
			if serverID == "" {
				return provision.NewRetryInterval(time.Second * 10)
			}

			zoneName := pctx.State.TypedSpec().Value.Zone
			if zoneName == "" {
				zoneName = p.defaultZone
			}

			zone, err := scw.ParseZone(zoneName)
			if err != nil {
				return fmt.Errorf("invalid zone %q: %w", zoneName, err)
			}

			instanceAPI := instance.NewAPI(p.scwClient)

			resp, err := instanceAPI.GetServer(&instance.GetServerRequest{
				Zone:     zone,
				ServerID: serverID,
			}, scw.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("failed to get server state for %s: %w", serverID, err)
			}

			state := resp.Server.State
			logger.Info("waiting for server to be running",
				zap.String("server_id", serverID),
				zap.String("state", state.String()),
			)

			switch state {
			case instance.ServerStateRunning:
				return nil
			case instance.ServerStateStopped, instance.ServerStateStoppedInPlace, instance.ServerStateLocked:
				return fmt.Errorf("server %s entered terminal state %q", serverID, state)
			}

			return provision.NewRetryInterval(time.Second * 10)
		}),
	}
}

// zoneToRegion strips the zone suffix to get the region, e.g. "fr-par-1" → "fr-par".
func zoneToRegion(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}

	return zone
}

// pickZone selects the zone with the fewest omni-managed servers from the candidate list.
// With concurrency=1 there is no race, so this guarantees even spread.
func pickZone(ctx context.Context, instanceAPI *instance.API, zones []string) (string, error) {
	counts := make(map[string]int, len(zones))
	for _, z := range zones {
		counts[z] = 0
	}

	for _, z := range zones {
		zone, err := scw.ParseZone(z)
		if err != nil {
			return "", fmt.Errorf("invalid zone %q: %w", z, err)
		}

		resp, err := instanceAPI.ListServers(&instance.ListServersRequest{
			Zone: zone,
			Tags: []string{"omni"},
		}, scw.WithContext(ctx))
		if err != nil {
			return "", fmt.Errorf("failed to list servers in zone %s: %w", z, err)
		}

		counts[z] = int(resp.TotalCount)
	}

	picked := zones[0]
	for _, z := range zones[1:] {
		if counts[z] < counts[picked] {
			picked = z
		}
	}

	return picked, nil
}

func resolveImageByName(ctx context.Context, instanceAPI *instance.API, zone scw.Zone, name string, arch instance.Arch) (string, error) {
	archStr := string(arch)
	resp, err := instanceAPI.ListImages(&instance.ListImagesRequest{
		Zone: zone,
		Name: &name,
		Arch: &archStr,
	}, scw.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to list images in zone %s: %w", zone, err)
	}

	for _, img := range resp.Images {
		if img.Name == name {
			return img.ID, nil
		}
	}

	return "", fmt.Errorf("no image named %q (arch %s) found in zone %s", name, arch, zone)
}

// resolveArch returns the Scaleway Arch for the given commercial type.
// If userArch is set ("amd64" or "arm64"), it is converted directly.
// Otherwise the Scaleway server-types API is queried.
func resolveArch(ctx context.Context, instanceAPI *instance.API, zone scw.Zone, commercialType, userArch string) (instance.Arch, error) {
	switch userArch {
	case "amd64":
		return instance.ArchX86_64, nil
	case "arm64":
		return instance.ArchArm64, nil
	case "arm":
		return instance.ArchArm, nil
	case "":
		return inferArch(ctx, instanceAPI, zone, commercialType)
	default:
		return "", fmt.Errorf("unsupported arch %q: must be amd64, arm64, or arm", userArch)
	}
}

func inferArch(ctx context.Context, instanceAPI *instance.API, zone scw.Zone, commercialType string) (instance.Arch, error) {
	resp, err := instanceAPI.ListServersTypes(&instance.ListServersTypesRequest{
		Zone: zone,
	}, scw.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to list server types in zone %s: %w", zone, err)
	}

	serverType, ok := resp.Servers[commercialType]
	if !ok {
		return "", fmt.Errorf("commercial type %q not found in zone %s", commercialType, zone)
	}

	switch serverType.Arch {
	case instance.ArchX86_64, instance.ArchArm64, instance.ArchArm:
		return serverType.Arch, nil
	default:
		return "", fmt.Errorf("unknown architecture %q for commercial type %q", serverType.Arch, commercialType)
	}
}

// Deprovision implements infra.Provisioner.
func (p *Provisioner) Deprovision(ctx context.Context, logger *zap.Logger, machine *resources.Machine, _ *infraresources.MachineRequest) error {
	serverID := machine.TypedSpec().Value.ServerId
	if serverID == "" {
		logger.Info("no server to deprovision")

		return nil
	}

	deprovisionZone := machine.TypedSpec().Value.Zone
	if deprovisionZone == "" {
		deprovisionZone = p.defaultZone
	}

	zone, err := scw.ParseZone(deprovisionZone)
	if err != nil {
		return fmt.Errorf("invalid zone %q in machine state: %w", deprovisionZone, err)
	}

	instanceAPI := instance.NewAPI(p.scwClient)

	resp, err := instanceAPI.GetServer(&instance.GetServerRequest{
		Zone:     zone,
		ServerID: serverID,
	}, scw.WithContext(ctx))
	if err != nil {
		// Server already gone — deprovision complete.
		logger.Info("server already gone, deprovision complete", zap.String("server_id", serverID))

		return nil
	}

	switch resp.Server.State {
	case instance.ServerStateStopped, instance.ServerStateStoppedInPlace:
		// ready to delete — fall through
	case instance.ServerStateStopping:
		// already powering off, just wait
		return provision.NewRetryInterval(time.Second * 5)
	default:
		_, err = instanceAPI.ServerAction(&instance.ServerActionRequest{
			Zone:     zone,
			ServerID: serverID,
			Action:   instance.ServerActionPoweroff,
		}, scw.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("failed to power off server %s: %w", serverID, err)
		}

		return provision.NewRetryInterval(time.Second * 5)
	}

	// Collect volume IDs before deleting the server.
	var volumeIDs []string

	for _, vol := range resp.Server.Volumes {
		volumeIDs = append(volumeIDs, vol.ID)
	}

	if err = instanceAPI.DeleteServer(&instance.DeleteServerRequest{
		Zone:     zone,
		ServerID: serverID,
	}, scw.WithContext(ctx)); err != nil {
		return fmt.Errorf("failed to delete server %s: %w", serverID, err)
	}

	logger.Info("server deleted", zap.String("server_id", serverID))

	for _, volID := range volumeIDs {
		if err = instanceAPI.DeleteVolume(&instance.DeleteVolumeRequest{
			Zone:     zone,
			VolumeID: volID,
		}, scw.WithContext(ctx)); err != nil {
			logger.Warn("failed to delete volume", zap.String("volume_id", volID), zap.Error(err))
		}
	}

	logger.Info("server deprovisioned", zap.String("server_id", serverID))

	return nil
}
