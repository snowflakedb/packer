package ebs

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awscommon "github.com/hashicorp/packer/builder/amazon/common"
	"github.com/hashicorp/packer/common/random"
	"github.com/hashicorp/packer/helper/multistep"
	"github.com/hashicorp/packer/packer"
)

type stepCreateAMI struct {
	image              *ec2.Image
	AMISkipBuildRegion bool
}

func (s *stepCreateAMI) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(*Config)
	ec2conn := state.Get("ec2").(*ec2.EC2)
	instance := state.Get("instance").(*ec2.Instance)
	ui := state.Get("ui").(packer.Ui)

	// Create the image
	amiName := config.AMIName
	state.Put("intermediary_image", false)
	if config.AMIEncryptBootVolume.True() || s.AMISkipBuildRegion {
		state.Put("intermediary_image", true)

		// From AWS SDK docs: You can encrypt a copy of an unencrypted snapshot,
		// but you cannot use it to create an unencrypted copy of an encrypted
		// snapshot. Your default CMK for EBS is used unless you specify a
		// non-default key using KmsKeyId.

		// If encrypt_boot is nil or true, we need to create a temporary image
		// so that in step_region_copy, we can copy it with the correct
		// encryption
		amiName = random.AlphaNum(7)
	}

	ui.Say(fmt.Sprintf("Creating AMI %s from instance %s", amiName, *instance.InstanceId))
	createOpts := &ec2.CreateImageInput{
		InstanceId:          instance.InstanceId,
		Name:                &amiName,
		BlockDeviceMappings: config.AMIMappings.BuildEC2BlockDeviceMappings(),
	}

	createReq, createResp := ec2conn.CreateImageRequest(createOpts)
	createReq.RetryCount = 11
	if err := createReq.Send(); err != nil {
		err := fmt.Errorf("Error creating AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	// Set the AMI ID in the state
	ui.Message(fmt.Sprintf("AMI: %s", *createResp.ImageId))
	amis := make(map[string]string)
	amis[*ec2conn.Config.Region] = *createResp.ImageId
	state.Put("amis", amis)

	// Wait for the image to become ready
	ui.Say("Waiting for AMI to become ready...")
	if err := awscommon.WaitUntilAMIAvailable(ctx, ec2conn, *createResp.ImageId); err != nil {
		log.Printf("Error waiting for AMI: %s", err)
		imResp, imerr := ec2conn.DescribeImages(&ec2.DescribeImagesInput{ImageIds: []*string{createResp.ImageId}})
		if imerr != nil {
			log.Printf("Unable to determine reason waiting for AMI failed: %s", err)
			err = fmt.Errorf("Unknown error waiting for AMI; %s", err)
		}
		if imResp != nil && len(imResp.Images) > 0 {
			image := imResp.Images[0]
			if image != nil {
				stateReason := image.StateReason
				err = fmt.Errorf("Error waiting for AMI. Reason: %s", stateReason)
			}
		}
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	imagesReg, imagesResp := ec2conn.DescribeImagesRequest(&ec2.DescribeImagesInput{ImageIds: []*string{createResp.ImageId}})
	imagesReg.RetryCount = 11

	if err := imagesReg.Send(); err != nil {
		err := fmt.Errorf("Error searching for AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	s.image = imagesResp.Images[0]

	snapshots := make(map[string][]string)
	for _, blockDeviceMapping := range imagesResp.Images[0].BlockDeviceMappings {
		if blockDeviceMapping.Ebs != nil && blockDeviceMapping.Ebs.SnapshotId != nil {

			snapshots[*ec2conn.Config.Region] = append(snapshots[*ec2conn.Config.Region], *blockDeviceMapping.Ebs.SnapshotId)
		}
	}
	state.Put("snapshots", snapshots)

	return multistep.ActionContinue
}

func (s *stepCreateAMI) Cleanup(state multistep.StateBag) {
	if s.image == nil {
		return
	}

	_, cancelled := state.GetOk(multistep.StateCancelled)
	_, halted := state.GetOk(multistep.StateHalted)
	if !cancelled && !halted {
		return
	}

	ec2conn := state.Get("ec2").(*ec2.EC2)
	ui := state.Get("ui").(packer.Ui)

	ui.Say("Deregistering the AMI and deleting associated snapshots because " +
		"of cancellation, or error...")

	resp, err := ec2conn.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{s.image.ImageId},
	})

	if err != nil {
		err := fmt.Errorf("Error describing AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return
	}

	// Deregister image by name.
	for _, i := range resp.Images {
		_, err := ec2conn.DeregisterImage(&ec2.DeregisterImageInput{
			ImageId: i.ImageId,
		})

		if err != nil {
			err := fmt.Errorf("Error deregistering existing AMI: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return
		}
		ui.Say(fmt.Sprintf("Deregistered AMI id: %s", *i.ImageId))

		// Delete snapshot(s) by image
		for _, b := range i.BlockDeviceMappings {
			if b.Ebs != nil && aws.StringValue(b.Ebs.SnapshotId) != "" {
				_, err := ec2conn.DeleteSnapshot(&ec2.DeleteSnapshotInput{
					SnapshotId: b.Ebs.SnapshotId,
				})

				if err != nil {
					err := fmt.Errorf("Error deleting existing snapshot: %s", err)
					state.Put("error", err)
					ui.Error(err.Error())
					return
				}
				ui.Say(fmt.Sprintf("Deleted snapshot: %s", *b.Ebs.SnapshotId))
			}
		}
	}
}
