package eks

import (
	"math"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/gruntwork-io/gruntwork-cli/errors"

	"github.com/gruntwork-io/kubergrunt/eksawshelper"
	"github.com/gruntwork-io/kubergrunt/logging"
)

// CleanupSecurityGroup deletes the AWS EKS managed security group, which otherwise doesn't get cleaned up when
// destroying the EKS cluster. It also attempts to delete the security group left by ALB ingress controller, if applicable.
func CleanupSecurityGroup(
	clusterArn string,
	securityGroupID string,
	vpcID string,
) error {
	logger := logging.GetProjectLogger()

	// Set wait variables for NetworkInterface detaching and deleting
	waitSleepBetweenRetries := 1 * time.Second
	waitMaxRetries := int(math.Trunc(30 / waitSleepBetweenRetries.Seconds()))

	// Get Region from ARN
	region, err := eksawshelper.GetRegionFromArn(clusterArn)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	// Get Cluster Name from ARN
	clusterID, err := eksawshelper.GetClusterNameFromArn(clusterArn)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	// Start new AWS session
	sess, err := eksawshelper.NewAuthenticatedSession(region)
	if err != nil {
		return errors.WithStackTrace(err)
	}
	ec2Svc := ec2.New(sess)
	logger.Infof("Successfully authenticated with AWS")

	// Find network interfaces for AWS-managed security group
	describeNetworkInterfacesInput := &ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("group-id"),
				Values: []*string{aws.String(securityGroupID)},
			},
		},
	}
	networkInterfacesResult, err := ec2Svc.DescribeNetworkInterfaces(describeNetworkInterfacesInput)
	if err != nil {
		return errors.WithStackTrace(err)
	}
	for _, ni := range networkInterfacesResult.NetworkInterfaces {
		logger.Infof("Found network interface %s", ni.NetworkInterfaceId)
	}

	// Detach network interfaces
	for _, ni := range networkInterfacesResult.NetworkInterfaces {
		detachInput := &ec2.DetachNetworkInterfaceInput{
			AttachmentId: ni.Attachment.AttachmentId,
		}
		_, err := ec2Svc.DetachNetworkInterface(detachInput)
		if err != nil {
			return errors.WithStackTrace(err)
		}
		logger.Infof("Requested to detach network interface %s for security group %s", aws.StringValue(ni.NetworkInterfaceId), securityGroupID)
	}

	// Wait for network interfaces to be detached
	if len(networkInterfacesResult.NetworkInterfaces) > 0 {
		err = waitForNetworkInterfacesToBeDetached(ec2Svc, networkInterfacesResult.NetworkInterfaces, waitMaxRetries, waitSleepBetweenRetries)
		if err != nil {
			return err
		}
		logger.Info("Verified network interfaces are detached.")
	}

	// Delete network interfaces
	for _, ni := range networkInterfacesResult.NetworkInterfaces {
		deleteNetworkInterfacesInput := &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: ni.NetworkInterfaceId,
		}
		_, err := ec2Svc.DeleteNetworkInterface(deleteNetworkInterfacesInput)

		if err != nil {
			return errors.WithStackTrace(err)
		}
		logger.Infof("Requested to delete network interface %s for security group %s", *ni.NetworkInterfaceId, securityGroupID)
	}

	// Wait for network interfaces to be deleted
	err = waitForNetworkInterfacesToBeDeleted(ec2Svc, networkInterfacesResult.NetworkInterfaces, waitMaxRetries, waitSleepBetweenRetries)
	if err != nil {
		return err
	}
	logger.Info("Verified network interfaces are deleted.")

	// Delete security group
	logger.Infof("Deleting security group %s", securityGroupID)
	delSGInput := &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(securityGroupID),
	}
	_, err = ec2Svc.DeleteSecurityGroup(delSGInput)
	if err != nil {
		if awsErr, isAwsErr := err.(awserr.Error); isAwsErr && awsErr.Code() == "InvalidGroup.NotFound" {
			logger.Infof("Security group %s already deleted.", securityGroupID)
			return nil
		}
		return errors.WithStackTrace(err)
	}
	logger.Infof("Successfully deleted security group %s", securityGroupID)

	// Now delete ALB Ingress Controller's security group, if it exists
	logger.Infof("Looking up security group containing tag for EKS cluster %s", clusterID)
	sgInput := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(vpcID)},
			},
			{
				Name:   aws.String("tag:kubernetes.io/cluster-name"),
				Values: []*string{aws.String(clusterID)},
			},
		}}

	sgResult, err := ec2Svc.DescribeSecurityGroups(sgInput)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	for _, result := range sgResult.SecurityGroups {
		groupID := *result.GroupId
		groupName := *result.GroupName
		input := &ec2.DeleteSecurityGroupInput{
			GroupId:   aws.String(groupID),
			GroupName: aws.String(groupName),
		}
		// DeleteSecurityGroup returns a struct with only private fields
		// so we can ignore the result.
		_, err := ec2Svc.DeleteSecurityGroup(input)
		if err != nil {
			return errors.WithStackTrace(err)
		}

		logger.Infof("Successfully deleted security group with name=%s, id=%s", groupID, groupName)
	}

	return nil
}

func waitForNetworkInterfacesToBeDetached(
	ec2Svc *ec2.EC2,
	networkInterfaces []*ec2.NetworkInterface,
	maxRetries int,
	sleepBetweenRetries time.Duration,
) error {
	logger := logging.GetProjectLogger()
	for _, ni := range networkInterfaces {
		for i := 0; i < maxRetries; i++ {
			logger.Infof("Waiting for network interface %s to reach detached state.", aws.StringValue(ni.NetworkInterfaceId))
			logger.Info("Checking network interface attachment status.")

			// Poll for the new status
			describeNetworkInterfacesInput := &ec2.DescribeNetworkInterfaceAttributeInput{
				Attribute:          aws.String("attachment"),
				NetworkInterfaceId: ni.NetworkInterfaceId,
			}

			niResult, err := ec2Svc.DescribeNetworkInterfaceAttribute(describeNetworkInterfacesInput)
			if err != nil {
				logger.Errorf("Error polling network interface attribute: attachment for %s", aws.StringValue(ni.NetworkInterfaceId))
				return errors.WithStackTrace(err)
			}

			// There should only be one interface in this result
			if aws.StringValue(niResult.Attachment.Status) == "detached" {
				logger.Infof("Network interface %s is detached.", aws.StringValue(ni.NetworkInterfaceId))
				return nil
			}

			logger.Warnf("Network interface %s is not detached yet. Status: %s", aws.StringValue(ni.NetworkInterfaceId), aws.StringValue(niResult.Attachment.Status))
			logger.Infof("Waiting for %s...", sleepBetweenRetries)
			time.Sleep(sleepBetweenRetries)
		}

		return errors.WithStackTrace(NetworkInterfaceDetachedTimeoutError{aws.StringValue(ni.NetworkInterfaceId)})
	}
	return nil
}

func waitForNetworkInterfacesToBeDeleted(
	ec2Svc *ec2.EC2,
	networkInterfaces []*ec2.NetworkInterface,
	maxRetries int,
	sleepBetweenRetries time.Duration,
) error {
	logger := logging.GetProjectLogger()
	for _, ni := range networkInterfaces {
		for i := 0; i < maxRetries; i++ {
			logger.Infof("Waiting for network interface %s to be deleted.", aws.StringValue(ni.NetworkInterfaceId))
			logger.Info("Checking for network interface not found.")

			// Poll for the new status
			describeNetworkInterfacesInput := &ec2.DescribeNetworkInterfacesInput{
				NetworkInterfaceIds: []*string{ni.NetworkInterfaceId},
			}
			_, err := ec2Svc.DescribeNetworkInterfaces(describeNetworkInterfacesInput)
			if err != nil {
				if awsErr, isAwsErr := err.(awserr.Error); isAwsErr && awsErr.Code() == "InvalidNetworkInterfaceID.NotFound" {
					logger.Infof("Network interface %s is deleted.", aws.StringValue(ni.NetworkInterfaceId))
					return nil
				}

				return errors.WithStackTrace(err)
			}

			logger.Warnf("Network interface %s is not deleted yet.", aws.StringValue(ni.NetworkInterfaceId))
			logger.Infof("Waiting for %s...", sleepBetweenRetries)
			time.Sleep(sleepBetweenRetries)
		}

		return errors.WithStackTrace(NetworkInterfaceDetachedTimeoutError{aws.StringValue(ni.NetworkInterfaceId)})
	}
	return nil
}