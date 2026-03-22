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
	"sync"
	"time"

	block "github.com/scaleway/scaleway-sdk-go/api/block/v1alpha1"
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

	zoneMu     sync.Mutex
	zoneCounts map[string]int // in-memory zone counts; initialized lazily from Scaleway API
}

// NewProvisioner creates a new provisioner.
func NewProvisioner(scwClient *scw.Client, defaultZone string) *Provisioner {
	return &Provisioner{
		scwClient:   scwClient,
		defaultZone: defaultZone,
		zoneCounts:  make(map[string]int),
	}
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

			for _, r := range providerData.Regions {
				if len(r.Zones) == 0 {
					return fmt.Errorf("region %q has no zones defined", r.Region)
				}
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
				// Scaleway kernel args must be included so that schematic upgrades
				// don't overwrite the bootloader and drop platform support.
				provision.WithExtraKernelArgs(
					"-console",
					"console=ttyS0,115200",
					"talos.dashboard.disabled=0",
					"talos.platform=scaleway",
				),
			)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.Schematic = schematic

			return nil
		}),
		// createServer allocates the Scaleway instance and immediately persists its ID.
		// Keeping this step self-contained ensures the ServerId is written to COSI state
		// before any post-creation operation (user-data, NIC, poweron) is attempted.
		// Without this split, a failure in any of those operations would cause a retry
		// that re-enters with ServerId == "" and create a duplicate (orphaned) server.
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
			if allZones := providerData.AllZones(); len(allZones) > 0 {
				zoneName, err = p.pickZone(ctx, instance.NewAPI(p.scwClient), allZones)
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

			commercialType := providerData.CommercialTypeForZone(zoneName)

			// Query server type constraints once; used for both arch inference and volume logic.
			serverTypes, err := instanceAPI.ListServersTypes(&instance.ListServersTypesRequest{
				Zone: zone,
			}, scw.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("failed to list server types in zone %s: %w", zone, err)
			}

			// maxLocalSSD == 0 when volumes_constraint is nil (block-storage-only types
			// such as POP2 and BASIC2 that cannot boot from l_ssd snapshots).
			var maxLocalSSD scw.Size
			if st, ok := serverTypes.Servers[commercialType]; ok && st.VolumesConstraint != nil {
				maxLocalSSD = st.VolumesConstraint.MaxSize
			}

			imageID := providerData.ImageID
			if imageID == "" && providerData.ImageName != "" {
				scwArch, err := resolveArchFromTypes(serverTypes.Servers, commercialType, providerData.Arch)
				if err != nil {
					return err
				}

				// Block-storage instance types (POP2, BASIC2, …) require SBS snapshots and
				// cannot boot from l_ssd images.  Resolve "<image_name>-sbs" for these.
				imageName := providerData.ImageName
				if maxLocalSSD == 0 {
					imageName += "-sbs"
				}

				imageID, err = resolveImageByName(ctx, instanceAPI, zone, imageName, scwArch)
				if err != nil {
					if maxLocalSSD == 0 {
						return fmt.Errorf(
							"%w\n\nHint: %q (commercial_type %s) is a block-storage instance type that "+
								"requires an SBS-backed image. Run hack/upload-talos-image.sh to create "+
								"both l_ssd and SBS images automatically. The SBS image must be named %q "+
								"and be available in zone %s.",
							err, commercialType, commercialType, imageName, zone,
						)
					}

					return err
				}
			}

			bootType := instance.BootTypeLocal

			tags := append(providerData.Tags, "omni", "omni-request-id="+pctx.GetRequestID())

			req := &instance.CreateServerRequest{
				Zone:           zone,
				Name:           pctx.GetRequestID(),
				CommercialType: commercialType,
				Image:          &imageID,
				Tags:           tags,
				BootType:       &bootType,
			}

			if maxLocalSSD > 0 {
				// Traditional instance type with explicit local SSD sizing (GP1, DEV1, PRO2, POP2, …).
				diskSizeGB := providerData.DiskSizeGBForZone(zoneName)
				if diskSizeGB == 0 {
					diskSizeGB = 40
				}

				diskSize := scw.Size(diskSizeGB) * scw.GB
				volName := pctx.GetRequestID() + "-root-volume"
				req.Volumes = map[string]*instance.VolumeServerTemplate{
					"0": {Size: &diskSize, Name: &volName},
				}
			}
			// When volumes_constraint is nil (e.g. BASIC2) the instance type manages its
			// own root volume: the image snapshot is l_ssd but must not have an explicit
			// size in the request.  Omitting the volumes field entirely lets Scaleway
			// auto-size the root volume from the snapshot.

			createResp, err := instanceAPI.CreateServer(req, scw.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("failed to create Scaleway server: %w", err)
			}

			serverID := createResp.Server.ID

			// Persist immediately — the controller writes state on step success.
			// Subsequent steps read ServerId from this saved state.
			pctx.State.TypedSpec().Value.ServerId = serverID
			pctx.State.TypedSpec().Value.Zone = zoneName
			pctx.SetMachineInfraID(serverID)

			logger.Info("created Scaleway server", zap.String("server_id", serverID), zap.String("zone", zoneName))

			return nil
		}),
		// configureServer attaches the private NIC (if any), writes the Talos join config
		// as cloud-init user-data, and powers the server on.  All operations are idempotent
		// so the step can be safely retried without creating duplicate resources.
		provision.NewStep("configureServer", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			serverID := pctx.State.TypedSpec().Value.ServerId
			zoneName := pctx.State.TypedSpec().Value.Zone

			zone, err := scw.ParseZone(zoneName)
			if err != nil {
				return fmt.Errorf("invalid zone %q: %w", zoneName, err)
			}

			instanceAPI := instance.NewAPI(p.scwClient)

			var providerData data.Data
			if err = pctx.UnmarshalProviderData(&providerData); err != nil {
				return err
			}

			// Attach private NIC if configured (idempotent: skip if already attached).
			if pnID := providerData.NetworkIDForZone(zoneName); pnID != "" {
				nics, err := instanceAPI.ListPrivateNICs(&instance.ListPrivateNICsRequest{
					Zone:     zone,
					ServerID: serverID,
				}, scw.WithContext(ctx))
				if err != nil {
					return fmt.Errorf("failed to list NICs on server %s: %w", serverID, err)
				}

				attached := false

				for _, nic := range nics.PrivateNics {
					if nic.PrivateNetworkID == pnID {
						attached = true

						break
					}
				}

				if !attached {
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
			}

			// Set Talos machine config as cloud-init user-data (idempotent PUT).
			if err = instanceAPI.SetServerUserData(&instance.SetServerUserDataRequest{
				Zone:     zone,
				ServerID: serverID,
				Key:      "cloud-init",
				Content:  io.NopCloser(strings.NewReader(pctx.ConnectionParams.JoinConfig)),
			}, scw.WithContext(ctx)); err != nil {
				return fmt.Errorf("failed to set user-data on server %s: %w", serverID, err)
			}

			// Get server to inspect its current state and volumes.
			srv, err := instanceAPI.GetServer(&instance.GetServerRequest{
				Zone:     zone,
				ServerID: serverID,
			}, scw.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("failed to get server %s state: %w", serverID, err)
			}

			// Resize SBS block volumes to the configured disk size.
			// Instance types that don't support explicit LSSD sizing (e.g. BASIC2) receive
			// an auto-sized root volume from the snapshot (~image size, typically 2-5 GB).
			// We expand it to disk_size_gb (default 40 GB) via the block API while the
			// server is still stopped.
			diskSizeGB := providerData.DiskSizeGBForZone(zoneName)
			if diskSizeGB == 0 {
				diskSizeGB = 40
			}

			targetSize := scw.Size(diskSizeGB) * scw.GB
			blockAPI := block.NewAPI(p.scwClient)

			for _, vol := range srv.Server.Volumes {
				if vol.VolumeType != instance.VolumeServerVolumeTypeSbsVolume {
					continue
				}

				if vol.Size != nil && *vol.Size >= targetSize {
					break
				}

				fromSize := "unknown"
				if vol.Size != nil {
					fromSize = vol.Size.String()
				}

				logger.Info("resizing SBS root volume",
					zap.String("volume_id", vol.ID),
					zap.String("from", fromSize),
					zap.String("to", targetSize.String()),
				)

				volName := pctx.GetRequestID() + "-root-volume"
				if _, err = blockAPI.UpdateVolume(&block.UpdateVolumeRequest{
					Zone:     zone,
					VolumeID: vol.ID,
					Size:     &targetSize,
					Name:     &volName,
				}, scw.WithContext(ctx)); err != nil {
					return fmt.Errorf("failed to resize SBS volume %s: %w", vol.ID, err)
				}

				break
			}

			if srv.Server.State == instance.ServerStateStopped || srv.Server.State == instance.ServerStateStoppedInPlace {
				_, err = instanceAPI.ServerAction(&instance.ServerActionRequest{
					Zone:     zone,
					ServerID: serverID,
					Action:   instance.ServerActionPoweron,
				}, scw.WithContext(ctx))
				if err != nil {
					return fmt.Errorf("failed to power on server %s: %w", serverID, err)
				}

				logger.Info("powered on Scaleway server", zap.String("server_id", serverID))
			}

			return nil
		}),
		provision.NewStep("waitForRunning", func(ctx context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			serverID := pctx.State.TypedSpec().Value.ServerId
			if serverID == "" {
				return provision.NewRetryInterval(time.Second * 5)
			}

			zoneName := pctx.State.TypedSpec().Value.Zone
			if zoneName == "" {
				zoneName = p.defaultZone
			}

			zone, err := scw.ParseZone(zoneName)
			if err != nil {
				return fmt.Errorf("invalid zone %q: %w", zoneName, err)
			}

			resp, err := instance.NewAPI(p.scwClient).GetServer(&instance.GetServerRequest{
				Zone:     zone,
				ServerID: serverID,
			}, scw.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("failed to get server %s: %w", serverID, err)
			}

			switch resp.Server.State {
			case instance.ServerStateRunning:
				return nil
			case instance.ServerStateLocked:
				return fmt.Errorf("server %s is locked", serverID)
			default:
				return provision.NewRetryInterval(time.Second * 5)
			}
		}),
	}
}


// pickZone selects the zone with the fewest omni-managed servers.
// Unknown zones are initialized from the Scaleway API once, then tracked in-memory.
// The mutex makes zone selection atomic so concurrent provisioning still guarantees even spread.
func (p *Provisioner) pickZone(ctx context.Context, instanceAPI *instance.API, zones []string) (string, error) {
	p.zoneMu.Lock()
	defer p.zoneMu.Unlock()

	// Initialize any zones not yet seen from the Scaleway API.
	for _, z := range zones {
		if _, ok := p.zoneCounts[z]; ok {
			continue
		}

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

		p.zoneCounts[z] = int(resp.TotalCount)
	}

	picked := zones[0]
	for _, z := range zones[1:] {
		if p.zoneCounts[z] < p.zoneCounts[picked] {
			picked = z
		}
	}

	// Reserve the slot immediately so concurrent picks see an up-to-date count.
	p.zoneCounts[picked]++

	return picked, nil
}

// releaseZone decrements the in-memory count for a zone after a server is deprovisioned.
func (p *Provisioner) releaseZone(zone string) {
	p.zoneMu.Lock()
	defer p.zoneMu.Unlock()

	if p.zoneCounts[zone] > 0 {
		p.zoneCounts[zone]--
	}
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

// resolveArchFromTypes returns the Scaleway Arch for the given commercial type.
// If userArch is set ("amd64" or "arm64"), it is converted directly.
// Otherwise the arch is inferred from the already-fetched server types map.
func resolveArchFromTypes(servers map[string]*instance.ServerType, commercialType, userArch string) (instance.Arch, error) {
	switch userArch {
	case "amd64":
		return instance.ArchX86_64, nil
	case "arm64":
		return instance.ArchArm64, nil
	case "arm":
		return instance.ArchArm, nil
	case "":
		serverType, ok := servers[commercialType]
		if !ok {
			return "", fmt.Errorf("commercial type %q not found in server types response", commercialType)
		}

		switch serverType.Arch {
		case instance.ArchX86_64, instance.ArchArm64, instance.ArchArm:
			return serverType.Arch, nil
		default:
			return "", fmt.Errorf("unknown architecture %q for commercial type %q", serverType.Arch, commercialType)
		}
	default:
		return "", fmt.Errorf("unsupported arch %q: must be amd64, arm64, or arm", userArch)
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

	// Collect volume IDs and types before deleting the server.
	type volEntry struct {
		id       string
		volType  instance.VolumeServerVolumeType
	}

	var volumes []volEntry

	for _, vol := range resp.Server.Volumes {
		volumes = append(volumes, volEntry{id: vol.ID, volType: vol.VolumeType})
	}

	if err = instanceAPI.DeleteServer(&instance.DeleteServerRequest{
		Zone:     zone,
		ServerID: serverID,
	}, scw.WithContext(ctx)); err != nil {
		return fmt.Errorf("failed to delete server %s: %w", serverID, err)
	}

	logger.Info("server deleted", zap.String("server_id", serverID))

	p.releaseZone(deprovisionZone)

	blockAPI := block.NewAPI(p.scwClient)

	for _, vol := range volumes {
		switch vol.volType {
		case instance.VolumeServerVolumeTypeLSSD:
			// Local SSD volumes are deleted automatically with the server.
		case instance.VolumeServerVolumeTypeSbsVolume:
			// SBS volumes are detached asynchronously after server deletion.
			// Wait for the volume to reach "available" before deleting it.
			availStatus := block.VolumeStatusAvailable
			if _, err = blockAPI.WaitForVolume(&block.WaitForVolumeRequest{
				Zone:           zone,
				VolumeID:       vol.id,
				TerminalStatus: &availStatus,
			}, scw.WithContext(ctx)); err != nil {
				logger.Warn("timed out waiting for sbs volume to detach", zap.String("volume_id", vol.id), zap.Error(err))

				break
			}

			if err = blockAPI.DeleteVolume(&block.DeleteVolumeRequest{
				Zone:     zone,
				VolumeID: vol.id,
			}, scw.WithContext(ctx)); err != nil {
				logger.Warn("failed to delete sbs volume", zap.String("volume_id", vol.id), zap.Error(err))
			} else {
				logger.Info("deleted sbs volume", zap.String("volume_id", vol.id))
			}
		default:
			// b_ssd and other types managed by the instance API.
			if err = instanceAPI.DeleteVolume(&instance.DeleteVolumeRequest{
				Zone:     zone,
				VolumeID: vol.id,
			}, scw.WithContext(ctx)); err != nil {
				logger.Warn("failed to delete volume", zap.String("volume_id", vol.id), zap.String("type", string(vol.volType)), zap.Error(err))
			} else {
				logger.Info("deleted volume", zap.String("volume_id", vol.id), zap.String("type", string(vol.volType)))
			}
		}
	}

	logger.Info("server deprovisioned", zap.String("server_id", serverID))

	return nil
}
