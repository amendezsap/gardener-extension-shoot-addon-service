package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

const (
	// TagManagedBy is used to tag resources created by this controller.
	TagManagedBy = "managed-by"
	// TagManagedByValue identifies resources we own.
	TagManagedByValue = "gardener-extension-shoot-addon-service"
	// TagShoots tracks which shoot namespaces use a VPC endpoint.
	// Value is comma-separated: "shoot--proj--a,shoot--proj--b"
	TagShoots = "managed-resources-shoots"
	// CloudWatchLogsServiceTemplate is the service name for CloudWatch Logs VPC endpoint.
	CloudWatchLogsServiceTemplate = "com.amazonaws.%s.logs"
)

// Credentials holds AWS credentials extracted from the Gardener cloudprovider secret.
type Credentials struct {
	AccessKeyID    string
	SecretAccessKey string
	RoleARN        string
	Token          string
}

// Client wraps AWS SDK clients for IAM and EC2 operations.
type Client struct {
	iam    *iam.Client
	ec2    *ec2.Client
	region string
}

// VPCEndpointInfo holds details about a VPC endpoint returned by queries.
type VPCEndpointInfo struct {
	ID              string
	State           string
	SecurityGroups  []string
	CreatedByUs     bool     // true if our managed-by tag is present
	TrackedShoots   []string // shoot namespaces from our tracking tag
}

// EnsureResult holds the result of an EnsureCloudWatchVPCEndpoint call.
type EnsureResult struct {
	EndpointID  string
	CreatedByUs bool // true if we just created it (or previously created it)
}

// NewClient creates an AWS client from Gardener cloudprovider credentials.
func NewClient(creds *Credentials, region string) (*Client, error) {
	cfg := aws.Config{
		Region: region,
		Credentials: credentials.NewStaticCredentialsProvider(
			creds.AccessKeyID,
			creds.SecretAccessKey,
			creds.Token, // Empty string if not using STS
		),
	}

	return &Client{
		iam:    iam.NewFromConfig(cfg),
		ec2:    ec2.NewFromConfig(cfg),
		region: region,
	}, nil
}

// --------------------------------------------------------------------------
// IAM Operations
// --------------------------------------------------------------------------

// AttachRolePolicy attaches an IAM policy to a role. Idempotent — succeeds if already attached.
func (c *Client) AttachRolePolicy(ctx context.Context, roleName, policyARN string) error {
	_, err := c.iam.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName:  aws.String(roleName),
		PolicyArn: aws.String(policyARN),
	})
	if err != nil {
		if isNoSuchEntity(err) {
			return fmt.Errorf("role %s does not exist: %w", roleName, err)
		}
		return fmt.Errorf("attach policy %s to %s: %w", policyARN, roleName, err)
	}
	return nil
}

// DetachRolePolicy detaches an IAM policy from a role. Idempotent.
func (c *Client) DetachRolePolicy(ctx context.Context, roleName, policyARN string) error {
	_, err := c.iam.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
		RoleName:  aws.String(roleName),
		PolicyArn: aws.String(policyARN),
	})
	if err != nil {
		if isNoSuchEntity(err) {
			return nil
		}
		return fmt.Errorf("detach policy %s from %s: %w", policyARN, roleName, err)
	}
	return nil
}

// --------------------------------------------------------------------------
// VPC Endpoint Operations
// --------------------------------------------------------------------------

// GetWorkerSubnetIDs looks up subnet IDs in a VPC that match the given worker CIDRs.
func (c *Client) GetWorkerSubnetIDs(ctx context.Context, vpcID string, workerCIDRs []string) ([]string, error) {
	resp, err := c.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe subnets for vpc %s: %w", vpcID, err)
	}

	cidrSet := make(map[string]bool, len(workerCIDRs))
	for _, cidr := range workerCIDRs {
		cidrSet[cidr] = true
	}

	var subnetIDs []string
	for _, s := range resp.Subnets {
		if s.CidrBlock != nil && cidrSet[*s.CidrBlock] {
			subnetIDs = append(subnetIDs, *s.SubnetId)
		}
	}
	return subnetIDs, nil
}

// FindCloudWatchVPCEndpoint finds a CloudWatch Logs VPC endpoint in the given VPC.
// Returns nil if no endpoint exists (available or pending).
func (c *Client) FindCloudWatchVPCEndpoint(ctx context.Context, vpcID, region string) (*VPCEndpointInfo, error) {
	serviceName := fmt.Sprintf(CloudWatchLogsServiceTemplate, region)

	resp, err := c.ec2.DescribeVpcEndpoints(ctx, &ec2.DescribeVpcEndpointsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("service-name"), Values: []string{serviceName}},
			{Name: aws.String("vpc-endpoint-state"), Values: []string{"available", "pending"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe vpc endpoints: %w", err)
	}

	if len(resp.VpcEndpoints) == 0 {
		return nil, nil
	}

	ep := resp.VpcEndpoints[0]
	info := &VPCEndpointInfo{
		ID:    *ep.VpcEndpointId,
		State: string(ep.State),
	}

	for _, sg := range ep.Groups {
		info.SecurityGroups = append(info.SecurityGroups, *sg.GroupId)
	}

	for _, tag := range ep.Tags {
		if tag.Key == nil {
			continue
		}
		switch *tag.Key {
		case TagManagedBy:
			if tag.Value != nil && *tag.Value == TagManagedByValue {
				info.CreatedByUs = true
			}
		case TagShoots:
			if tag.Value != nil && *tag.Value != "" {
				info.TrackedShoots = parseShoots(*tag.Value)
			}
		}
	}

	return info, nil
}

// EnsureCloudWatchVPCEndpoint creates a CloudWatch Logs Interface VPC endpoint
// if one doesn't exist. If an endpoint already exists (ours or external), it
// returns its info without creating a new one.
//
// Unlike the old version, this passes the node security group to the endpoint
// at creation time, and tags it with our managed-by and shoot tracking tags.
func (c *Client) EnsureCloudWatchVPCEndpoint(
	ctx context.Context,
	vpcID, region string,
	subnetIDs []string,
	nodeSecurityGroupID string,
	shootNamespace string,
) (*EnsureResult, error) {
	// Check if endpoint already exists
	existing, err := c.FindCloudWatchVPCEndpoint(ctx, vpcID, region)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		// Endpoint exists — add our SG and register our shoot
		if err := c.addSecurityGroupToEndpoint(ctx, existing.ID, nodeSecurityGroupID, existing.SecurityGroups); err != nil {
			return nil, fmt.Errorf("add SG to existing endpoint: %w", err)
		}
		if err := c.addShootToEndpointTag(ctx, existing.ID, shootNamespace, existing.TrackedShoots); err != nil {
			return nil, fmt.Errorf("add shoot to endpoint tag: %w", err)
		}
		return &EnsureResult{
			EndpointID:  existing.ID,
			CreatedByUs: existing.CreatedByUs,
		}, nil
	}

	// No endpoint exists — create one
	if len(subnetIDs) == 0 {
		return nil, fmt.Errorf("no worker subnets found for VPC %s, cannot create endpoint", vpcID)
	}

	serviceName := fmt.Sprintf(CloudWatchLogsServiceTemplate, region)
	securityGroupIDs := []string{nodeSecurityGroupID}

	createResp, err := c.ec2.CreateVpcEndpoint(ctx, &ec2.CreateVpcEndpointInput{
		VpcEndpointType:   ec2types.VpcEndpointTypeInterface,
		VpcId:             aws.String(vpcID),
		ServiceName:       aws.String(serviceName),
		SubnetIds:         subnetIDs,
		SecurityGroupIds:  securityGroupIDs,
		PrivateDnsEnabled: aws.Bool(true),
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeVpcEndpoint,
				Tags: []ec2types.Tag{
					{Key: aws.String("Name"), Value: aws.String("cloudwatch-logs")},
					{Key: aws.String(TagManagedBy), Value: aws.String(TagManagedByValue)},
					{Key: aws.String(TagShoots), Value: aws.String(shootNamespace)},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create vpc endpoint in %s: %w", vpcID, err)
	}

	return &EnsureResult{
		EndpointID:  *createResp.VpcEndpoint.VpcEndpointId,
		CreatedByUs: true,
	}, nil
}

// CleanupVPCEndpoint removes our shoot's node SG and tracking tag entry from
// the VPC endpoint. If we created the endpoint and no other shoots use it,
// the endpoint is deleted. If someone else created it, we never delete it.
//
// Returns true if the endpoint was deleted, false if it was kept.
func (c *Client) CleanupVPCEndpoint(
	ctx context.Context,
	vpcID, region string,
	nodeSecurityGroupID string,
	shootNamespace string,
) (deleted bool, err error) {
	existing, err := c.FindCloudWatchVPCEndpoint(ctx, vpcID, region)
	if err != nil {
		return false, err
	}
	if existing == nil {
		// Already gone — nothing to do (scenario D4)
		return false, nil
	}

	// Always remove our SG from the endpoint
	if err := c.removeSecurityGroupFromEndpoint(ctx, existing.ID, nodeSecurityGroupID, existing.SecurityGroups); err != nil {
		return false, fmt.Errorf("remove SG from endpoint: %w", err)
	}

	// Remove our shoot from the tracking tag
	remainingShoots, err := c.removeShootFromEndpointTag(ctx, existing.ID, shootNamespace, existing.TrackedShoots)
	if err != nil {
		return false, fmt.Errorf("remove shoot from endpoint tag: %w", err)
	}

	// Decision: should we delete the endpoint?
	// Only if: (1) we created it AND (2) no other shoots remain
	if !existing.CreatedByUs {
		// Scenario D3: someone else created it — never delete
		return false, nil
	}

	if len(remainingShoots) > 0 {
		// Scenario D2: other shoots still use it — keep it
		return false, nil
	}

	// Scenario D1: we created it and we're the last shoot — delete it
	_, err = c.ec2.DeleteVpcEndpoints(ctx, &ec2.DeleteVpcEndpointsInput{
		VpcEndpointIds: []string{existing.ID},
	})
	if err != nil {
		return false, fmt.Errorf("delete vpc endpoint %s: %w", existing.ID, err)
	}

	return true, nil
}

// WaitForVPCEndpointDeletion polls until the VPC endpoint is deleted or timeout.
func (c *Client) WaitForVPCEndpointDeletion(ctx context.Context, endpointID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		resp, err := c.ec2.DescribeVpcEndpoints(ctx, &ec2.DescribeVpcEndpointsInput{
			VpcEndpointIds: []string{endpointID},
		})
		if err != nil {
			// If the endpoint is not found, it's deleted
			if strings.Contains(err.Error(), "InvalidVpcEndpointId.NotFound") {
				return nil
			}
			return fmt.Errorf("check endpoint deletion status: %w", err)
		}

		if len(resp.VpcEndpoints) == 0 {
			return nil
		}

		if resp.VpcEndpoints[0].State == ec2types.StateDeleted {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}

	return fmt.Errorf("timeout waiting for VPC endpoint %s deletion after %v", endpointID, timeout)
}

// --------------------------------------------------------------------------
// Security Group helpers
// --------------------------------------------------------------------------

// addSecurityGroupToEndpoint adds a SG to the endpoint if not already present.
func (c *Client) addSecurityGroupToEndpoint(ctx context.Context, endpointID, sgID string, currentSGs []string) error {
	for _, existing := range currentSGs {
		if existing == sgID {
			return nil // already present
		}
	}

	newSGs := append(currentSGs, sgID)
	_, err := c.ec2.ModifyVpcEndpoint(ctx, &ec2.ModifyVpcEndpointInput{
		VpcEndpointId:       aws.String(endpointID),
		AddSecurityGroupIds: []string{sgID},
	})
	if err != nil {
		return fmt.Errorf("add SG %s to endpoint %s (current: %v, desired: %v): %w", sgID, endpointID, currentSGs, newSGs, err)
	}
	return nil
}

// removeSecurityGroupFromEndpoint removes a SG from the endpoint if present.
// Will not remove the last SG (AWS requires at least one).
func (c *Client) removeSecurityGroupFromEndpoint(ctx context.Context, endpointID, sgID string, currentSGs []string) error {
	found := false
	for _, existing := range currentSGs {
		if existing == sgID {
			found = true
			break
		}
	}
	if !found {
		return nil // not present, nothing to do
	}

	// AWS requires at least one SG on an endpoint. If removing ours would
	// leave zero, skip the removal — the endpoint will be deleted anyway.
	if len(currentSGs) <= 1 {
		return nil
	}

	_, err := c.ec2.ModifyVpcEndpoint(ctx, &ec2.ModifyVpcEndpointInput{
		VpcEndpointId:          aws.String(endpointID),
		RemoveSecurityGroupIds: []string{sgID},
	})
	if err != nil {
		return fmt.Errorf("remove SG %s from endpoint %s: %w", sgID, endpointID, err)
	}
	return nil
}

// --------------------------------------------------------------------------
// Tag-based shoot tracking
// --------------------------------------------------------------------------

// addShootToEndpointTag adds a shoot namespace to the tracking tag on the endpoint.
func (c *Client) addShootToEndpointTag(ctx context.Context, endpointID, shootNamespace string, currentShoots []string) error {
	for _, s := range currentShoots {
		if s == shootNamespace {
			return nil // already tracked
		}
	}

	newShoots := append(currentShoots, shootNamespace)
	_, err := c.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{endpointID},
		Tags: []ec2types.Tag{
			{Key: aws.String(TagShoots), Value: aws.String(joinShoots(newShoots))},
		},
	})
	if err != nil {
		return fmt.Errorf("update shoot tracking tag on %s: %w", endpointID, err)
	}
	return nil
}

// removeShootFromEndpointTag removes a shoot namespace from the tracking tag.
// Returns the remaining shoots after removal.
func (c *Client) removeShootFromEndpointTag(ctx context.Context, endpointID, shootNamespace string, currentShoots []string) ([]string, error) {
	remaining := make([]string, 0, len(currentShoots))
	for _, s := range currentShoots {
		if s != shootNamespace {
			remaining = append(remaining, s)
		}
	}

	tagValue := joinShoots(remaining)
	_, err := c.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{endpointID},
		Tags: []ec2types.Tag{
			{Key: aws.String(TagShoots), Value: aws.String(tagValue)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("update shoot tracking tag on %s: %w", endpointID, err)
	}
	return remaining, nil
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func parseShoots(val string) []string {
	var shoots []string
	for _, s := range strings.Split(val, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			shoots = append(shoots, s)
		}
	}
	return shoots
}

func joinShoots(shoots []string) string {
	return strings.Join(shoots, ",")
}

func isNoSuchEntity(err error) bool {
	return err != nil && strings.Contains(fmt.Sprintf("%v", err), "NoSuchEntity")
}
