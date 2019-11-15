package huaweicloud

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/bootfromvolume"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/hashicorp/packer/helper/multistep"
	"github.com/hashicorp/packer/packer"
)

type StepRunSourceServer struct {
	Name                  string
	SecurityGroups        []string
	Networks              []string
	Ports                 []string
	AvailabilityZone      string
	UserData              string
	UserDataFile          string
	ConfigDrive           bool
	InstanceMetadata      map[string]string
	UseBlockStorageVolume bool
	ForceDelete           bool
	server                *servers.Server
}

func (s *StepRunSourceServer) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(*Config)
	flavor := state.Get("flavor_id").(string)
	sourceImage := state.Get("source_image").(string)
	ui := state.Get("ui").(packer.Ui)

	// We need the v2 compute client
	computeClient, err := config.computeV2Client()
	if err != nil {
		err = fmt.Errorf("Error initializing compute client: %s", err)
		state.Put("error", err)
		return multistep.ActionHalt
	}

	networks := make([]servers.Network, len(s.Networks)+len(s.Ports))
	i := 0
	for ; i < len(s.Ports); i++ {
		networks[i].Port = s.Ports[i]
	}
	for ; i < len(networks); i++ {
		networks[i].UUID = s.Networks[i-len(s.Ports)]
	}

	userData := []byte(s.UserData)
	if s.UserDataFile != "" {
		userData, err = ioutil.ReadFile(s.UserDataFile)
		if err != nil {
			err = fmt.Errorf("Error reading user data file: %s", err)
			state.Put("error", err)
			return multistep.ActionHalt
		}
	}

	serverOpts := servers.CreateOpts{
		Name:             s.Name,
		ImageRef:         sourceImage,
		FlavorRef:        flavor,
		SecurityGroups:   s.SecurityGroups,
		Networks:         networks,
		AvailabilityZone: s.AvailabilityZone,
		UserData:         userData,
		ConfigDrive:      &s.ConfigDrive,
		ServiceClient:    computeClient,
		Metadata:         s.InstanceMetadata,
	}

	var serverOptsExt servers.CreateOptsBuilder

	// Create root volume in the Block Storage service if required.
	// Add block device mapping v2 to the server create options if required.
	if s.UseBlockStorageVolume {
		volume := state.Get("volume_id").(string)
		blockDeviceMappingV2 := []bootfromvolume.BlockDevice{
			{
				BootIndex:       0,
				DestinationType: bootfromvolume.DestinationVolume,
				SourceType:      bootfromvolume.SourceVolume,
				UUID:            volume,
			},
		}
		// ImageRef and block device mapping is an invalid options combination.
		serverOpts.ImageRef = ""
		serverOptsExt = bootfromvolume.CreateOptsExt{
			CreateOptsBuilder: &serverOpts, // must pass pointer, because it will be changed later
			BlockDevice:       blockDeviceMappingV2,
		}
	} else {
		serverOptsExt = &serverOpts // must pass pointer
	}

	// Add keypair to the server create options.
	keyName := config.Comm.SSHKeyPairName
	if keyName != "" {
		serverOptsExt = keypairs.CreateOptsExt{
			CreateOptsBuilder: serverOptsExt,
			KeyName:           keyName,
		}
	}

	azs := state.Get("azs").([]string)
	if s.AvailabilityZone != "" {
		for i, az := range azs {
			if az == s.AvailabilityZone {
				az = azs[0]
				azs[0] = s.AvailabilityZone
				azs[i] = az
			}
		}
	}
	var server *servers.Server
	for _, az := range azs {
		ui.Say(fmt.Sprintf("Launching server in az:%s ...", az))
		serverOpts.AvailabilityZone = az
		server, err = createServer(ui, state, computeClient, serverOptsExt)
		if err == nil {
			break
		}
	}
	if err != nil {
		state.Put("error", err)
		return multistep.ActionHalt
	}

	s.server = server
	state.Put("server", server)

	return multistep.ActionContinue
}

func (s *StepRunSourceServer) Cleanup(state multistep.StateBag) {
	if s.server == nil {
		return
	}

	config := state.Get("config").(*Config)
	ui := state.Get("ui").(packer.Ui)

	// We need the v2 compute client
	computeClient, err := config.computeV2Client()
	if err != nil {
		ui.Error(fmt.Sprintf("Error terminating server, may still be around: %s", err))
		return
	}

	ui.Say(fmt.Sprintf("Terminating the source server: %s ...", s.server.ID))
	if config.ForceDelete {
		if err := servers.ForceDelete(computeClient, s.server.ID).ExtractErr(); err != nil {
			ui.Error(fmt.Sprintf("Error terminating server, may still be around: %s", err))
			return
		}
	} else {
		if err := servers.Delete(computeClient, s.server.ID).ExtractErr(); err != nil {
			ui.Error(fmt.Sprintf("Error terminating server, may still be around: %s", err))
			return
		}
	}

	stateChange := StateChangeConf{
		Pending: []string{"ACTIVE", "BUILD", "REBUILD", "SUSPENDED", "SHUTOFF", "STOPPED"},
		Refresh: ServerStateRefreshFunc(computeClient, s.server),
		Target:  []string{"DELETED"},
	}

	WaitForState(&stateChange)
}

func createServer(ui packer.Ui, state multistep.StateBag, client *gophercloud.ServiceClient, opts servers.CreateOptsBuilder) (*servers.Server, error) {
	server, err := servers.Create(client, opts).Extract()
	if err != nil {
		err = fmt.Errorf("Error launching source server: %s", err)
		ui.Error(err.Error())
		return nil, err
	}

	ui.Message(fmt.Sprintf("Server ID: %s", server.ID))
	log.Printf("server id: %s", server.ID)

	ui.Say("Waiting for server to become ready...")
	stateChange := StateChangeConf{
		Pending:   []string{"BUILD"},
		Target:    []string{"ACTIVE"},
		Refresh:   ServerStateRefreshFunc(client, server),
		StepState: state,
	}
	latestServer, err := WaitForState(&stateChange)
	if err != nil {
		err = fmt.Errorf("Error waiting for server (%s) to become ready: %s", server.ID, err)
		ui.Error(err.Error())
		return nil, err
	}

	return latestServer.(*servers.Server), nil
}
