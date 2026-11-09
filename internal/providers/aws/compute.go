package awsinv

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/eks"

	"github.com/komolos-io/komolos-agent/internal/inventory"
)

// collectCompute reads what runs where: EC2 instances and their volumes (the
// VM path) and EKS/ECS clusters (the container path). Which one a source uses
// decides how it is migrated, so both are recorded.
func collectCompute(ctx context.Context, e *ec2.Client, k *eks.Client, s *ecs.Client, scope string, c *inventory.Compute) []inventory.Gap {
	var gaps []inventory.Gap
	steps := []struct {
		area string
		fn   func() error
	}{
		{"instances", func() error { return collectInstances(ctx, e, c) }},
		{"volumes", func() error { return collectVolumes(ctx, e, c) }},
		{"eks_clusters", func() error { return collectEKS(ctx, k, c) }},
		{"ecs_clusters", func() error { return collectECS(ctx, s, c) }},
	}
	for _, st := range steps {
		if err := st.fn(); err != nil {
			gaps = append(gaps, gapFrom(scope, st.area, err))
		}
	}
	return gaps
}

func collectInstances(ctx context.Context, c *ec2.Client, out *inventory.Compute) error {
	p := ec2.NewDescribeInstancesPaginator(c, &ec2.DescribeInstancesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, res := range page.Reservations {
			for _, in := range res.Instances {
				i := inventory.Instance{
					ID: aws.ToString(in.InstanceId), Name: tagName(in.Tags), Type: string(in.InstanceType),
					Platform: aws.ToString(in.PlatformDetails), Architecture: string(in.Architecture),
					ImageID: aws.ToString(in.ImageId), LaunchTime: aws.ToTime(in.LaunchTime),
					VPCID: aws.ToString(in.VpcId), SubnetID: aws.ToString(in.SubnetId),
					PrivateIP: aws.ToString(in.PrivateIpAddress), HasPublicIP: aws.ToString(in.PublicIpAddress) != "",
					RootDeviceType: string(in.RootDeviceType),
				}
				if in.State != nil {
					i.State = string(in.State.Name)
				}
				if in.Placement != nil {
					i.AZ = aws.ToString(in.Placement.AvailabilityZone)
				}
				for _, g := range in.SecurityGroups {
					i.SecurityGroups = append(i.SecurityGroups, aws.ToString(g.GroupId))
				}
				for _, m := range in.BlockDeviceMappings {
					if m.Ebs != nil {
						i.VolumeIDs = append(i.VolumeIDs, aws.ToString(m.Ebs.VolumeId))
					}
				}
				out.Instances = append(out.Instances, i)
			}
		}
	}
	return nil
}

func collectVolumes(ctx context.Context, c *ec2.Client, out *inventory.Compute) error {
	p := ec2.NewDescribeVolumesPaginator(c, &ec2.DescribeVolumesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, v := range page.Volumes {
			vol := inventory.Volume{
				ID: aws.ToString(v.VolumeId), SizeGiB: aws.ToInt32(v.Size), Type: string(v.VolumeType),
				IOPS: aws.ToInt32(v.Iops), Encrypted: aws.ToBool(v.Encrypted),
				AZ: aws.ToString(v.AvailabilityZone), State: string(v.State),
			}
			for _, a := range v.Attachments {
				vol.InstanceIDs = append(vol.InstanceIDs, aws.ToString(a.InstanceId))
			}
			out.Volumes = append(out.Volumes, vol)
		}
	}
	return nil
}

func collectEKS(ctx context.Context, c *eks.Client, out *inventory.Compute) error {
	p := eks.NewListClustersPaginator(c, &eks.ListClustersInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, name := range page.Clusters {
			d, err := c.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(name)})
			if err != nil {
				return err
			}
			cl := inventory.EKSCluster{Name: name}
			if d.Cluster != nil {
				cl.Version = aws.ToString(d.Cluster.Version)
				cl.Status = string(d.Cluster.Status)
				if v := d.Cluster.ResourcesVpcConfig; v != nil {
					cl.VPCID = aws.ToString(v.VpcId)
					cl.SubnetIDs = v.SubnetIds
				}
			}
			out.EKSClusters = append(out.EKSClusters, cl)
		}
	}
	return nil
}

func collectECS(ctx context.Context, c *ecs.Client, out *inventory.Compute) error {
	var arns []string
	p := ecs.NewListClustersPaginator(c, &ecs.ListClustersInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		arns = append(arns, page.ClusterArns...)
	}
	// DescribeClusters accepts at most 100 clusters per call.
	for start := 0; start < len(arns); start += 100 {
		end := min(start+100, len(arns))
		d, err := c.DescribeClusters(ctx, &ecs.DescribeClustersInput{Clusters: arns[start:end]})
		if err != nil {
			return err
		}
		for _, cl := range d.Clusters {
			out.ECSClusters = append(out.ECSClusters, inventory.ECSCluster{
				Name: aws.ToString(cl.ClusterName), Status: aws.ToString(cl.Status),
				RunningTasks: cl.RunningTasksCount, ActiveServices: cl.ActiveServicesCount,
			})
		}
	}
	return nil
}
