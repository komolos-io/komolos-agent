// Package awsinv inventories an AWS account through read-only API calls.
//
// It uses the standard AWS credential chain (environment, shared config and
// SSO profiles, instance or container roles), so it runs from CloudShell, a
// jump host or an admin laptop with the customer's own credentials. Nothing is
// written to the account. Every call is a Describe/List/Get; see
// aws-iam-policy.json at the repository root for the exact permissions.
package awsinv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/komolos-io/komolos-agent/internal/inventory"
)

type Options struct {
	Profile string   // optional shared-config profile
	Regions []string // empty means the profile's default region
	Log     io.Writer

	// endpoint overrides every service endpoint; tests only.
	endpoint string
}

// Collect returns the AWS section plus any gaps. A missing permission in one
// area never stops the others.
func Collect(ctx context.Context, opt Options) (*inventory.AWS, []inventory.Gap, error) {
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	loadOpts := []func(*config.LoadOptions) error{}
	if opt.Profile != "" {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(opt.Profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if opt.endpoint != "" {
		cfg.BaseEndpoint = aws.String(opt.endpoint)
	}
	regions := slices.Clone(opt.Regions)
	if len(regions) == 0 {
		if cfg.Region == "" {
			return nil, nil, errors.New("no region: pass --region or set AWS_REGION / a profile region")
		}
		regions = []string{cfg.Region}
	}

	out := &inventory.AWS{}
	var gaps []inventory.Gap

	// Identity first: it confirms the credentials work before anything else runs.
	idCfg := cfg.Copy()
	idCfg.Region = regions[0]
	id, err := sts.NewFromConfig(idCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, nil, fmt.Errorf("check AWS credentials (sts:GetCallerIdentity): %w", err)
	}
	out.AccountID, out.CallerARN = aws.ToString(id.Account), aws.ToString(id.Arn)
	fmt.Fprintf(opt.Log, "  account %s as %s\n", out.AccountID, out.CallerARN)

	for _, region := range regions {
		rc := cfg.Copy()
		rc.Region = region
		scope := "aws:" + region
		r := inventory.AWSRegion{Region: region}

		ec2c := ec2.NewFromConfig(rc)
		gaps = append(gaps, collectNetwork(ctx, ec2c, scope, &r.Network)...)
		gaps = append(gaps, collectCompute(ctx, ec2c, eks.NewFromConfig(rc), ecs.NewFromConfig(rc), scope, &r.Compute)...)

		fmt.Fprintf(opt.Log, "  %-14s instances=%d volumes=%d vpcs=%d\n",
			region, len(r.Compute.Instances), len(r.Compute.Volumes), len(r.Network.VPCs))
		out.Regions = append(out.Regions, r)
	}
	return out, gaps, nil
}
