package awsinv

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/komolos-io/komolos-agent/internal/inventory"
	"github.com/komolos-io/komolos-agent/internal/redact"
)

// collectNetwork reads the network layer. Each area is independent: a failure
// in one (usually a missing permission) becomes a Gap and collection continues,
// keeping whatever was read before the error.
func collectNetwork(ctx context.Context, c *ec2.Client, scope string, n *inventory.Network) []inventory.Gap {
	var gaps []inventory.Gap
	steps := []struct {
		area string
		fn   func() error
	}{
		{"vpcs", func() error { return collectVPCs(ctx, c, n) }},
		{"subnets", func() error { return collectSubnets(ctx, c, n) }},
		{"route_tables", func() error { return collectRouteTables(ctx, c, n) }},
		{"security_groups", func() error { return collectSecurityGroups(ctx, c, n) }},
		{"flow_logs", func() error { return collectFlowLogs(ctx, c, n) }},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			gaps = append(gaps, gapFrom(scope, s.area, err))
		}
	}
	return gaps
}

// gapFrom keeps only the API error code, not the message, so no extra detail leaks.
func gapFrom(scope, area string, err error) inventory.Gap {
	g := inventory.Gap{Scope: scope, Area: area, Code: "error"}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		g.Code = ae.ErrorCode() // EC2 uses UnauthorizedOperation for missing permissions
	}
	return g
}

func tagName(tags []ec2types.Tag) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == "Name" {
			return redact.String(aws.ToString(t.Value))
		}
	}
	return ""
}

func firstNonEmpty(vals ...*string) string {
	for _, v := range vals {
		if s := aws.ToString(v); s != "" {
			return s
		}
	}
	return ""
}

func collectVPCs(ctx context.Context, c *ec2.Client, n *inventory.Network) error {
	p := ec2.NewDescribeVpcsPaginator(c, &ec2.DescribeVpcsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, v := range page.Vpcs {
			n.VPCs = append(n.VPCs, inventory.VPC{
				ID: aws.ToString(v.VpcId), Name: tagName(v.Tags),
				CIDR: aws.ToString(v.CidrBlock), IsDefault: aws.ToBool(v.IsDefault),
			})
		}
	}
	return nil
}

func collectSubnets(ctx context.Context, c *ec2.Client, n *inventory.Network) error {
	p := ec2.NewDescribeSubnetsPaginator(c, &ec2.DescribeSubnetsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, s := range page.Subnets {
			n.Subnets = append(n.Subnets, inventory.Subnet{
				ID: aws.ToString(s.SubnetId), Name: tagName(s.Tags), VPCID: aws.ToString(s.VpcId),
				CIDR: aws.ToString(s.CidrBlock), AZ: aws.ToString(s.AvailabilityZone),
				Public: aws.ToBool(s.MapPublicIpOnLaunch),
			})
		}
	}
	return nil
}

func collectRouteTables(ctx context.Context, c *ec2.Client, n *inventory.Network) error {
	p := ec2.NewDescribeRouteTablesPaginator(c, &ec2.DescribeRouteTablesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, rt := range page.RouteTables {
			out := inventory.RouteTable{ID: aws.ToString(rt.RouteTableId), VPCID: aws.ToString(rt.VpcId)}
			for _, a := range rt.Associations {
				if aws.ToBool(a.Main) {
					out.Main = true
				}
				if id := aws.ToString(a.SubnetId); id != "" {
					out.Subnets = append(out.Subnets, id)
				}
			}
			for _, r := range rt.Routes {
				out.Routes = append(out.Routes, inventory.Route{
					Destination: firstNonEmpty(r.DestinationCidrBlock, r.DestinationIpv6CidrBlock, r.DestinationPrefixListId),
					Target: firstNonEmpty(r.GatewayId, r.NatGatewayId, r.TransitGatewayId,
						r.VpcPeeringConnectionId, r.InstanceId, r.NetworkInterfaceId),
					State: string(r.State),
				})
			}
			n.RouteTables = append(n.RouteTables, out)
		}
	}
	return nil
}

func rules(perms []ec2types.IpPermission) []inventory.Rule {
	var out []inventory.Rule
	for _, p := range perms {
		r := inventory.Rule{Protocol: aws.ToString(p.IpProtocol), FromPort: aws.ToInt32(p.FromPort), ToPort: aws.ToInt32(p.ToPort)}
		for _, rg := range p.IpRanges {
			r.CIDRs = append(r.CIDRs, aws.ToString(rg.CidrIp))
		}
		for _, rg := range p.Ipv6Ranges {
			r.CIDRs = append(r.CIDRs, aws.ToString(rg.CidrIpv6))
		}
		for _, pl := range p.PrefixListIds {
			r.CIDRs = append(r.CIDRs, aws.ToString(pl.PrefixListId))
		}
		for _, g := range p.UserIdGroupPairs {
			r.SourceGroups = append(r.SourceGroups, aws.ToString(g.GroupId))
		}
		out = append(out, r)
	}
	return out
}

func collectSecurityGroups(ctx context.Context, c *ec2.Client, n *inventory.Network) error {
	p := ec2.NewDescribeSecurityGroupsPaginator(c, &ec2.DescribeSecurityGroupsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, g := range page.SecurityGroups {
			n.SecurityGroups = append(n.SecurityGroups, inventory.SecurityGroup{
				ID: aws.ToString(g.GroupId), Name: aws.ToString(g.GroupName), VPCID: aws.ToString(g.VpcId),
				Ingress: rules(g.IpPermissions), Egress: rules(g.IpPermissionsEgress),
			})
		}
	}
	return nil
}

func collectFlowLogs(ctx context.Context, c *ec2.Client, n *inventory.Network) error {
	p := ec2.NewDescribeFlowLogsPaginator(c, &ec2.DescribeFlowLogsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, f := range page.FlowLogs {
			n.FlowLogs = append(n.FlowLogs, inventory.FlowLog{
				ID: aws.ToString(f.FlowLogId), ResourceID: aws.ToString(f.ResourceId),
				TrafficType: string(f.TrafficType), DestinationType: string(f.LogDestinationType),
				Status: aws.ToString(f.FlowLogStatus),
			})
		}
	}
	return nil
}
