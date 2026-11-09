package awsinv

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeAWS answers the handful of calls the collector makes, and denies the
// rest, so the test checks both parsing and gap handling.
func fakeAWS(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		action := form.Get("Action")
		switch {
		case action == "GetCallerIdentity":
			w.Header().Set("Content-Type", "text/xml")
			io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/reader</Arn><UserId>AIDA</UserId><Account>123456789012</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
		case action == "DescribeVpcs":
			io.WriteString(w, `<DescribeVpcsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>1</requestId><vpcSet><item><vpcId>vpc-1</vpcId><cidrBlock>10.0.0.0/16</cidrBlock><isDefault>false</isDefault><tagSet><item><key>Name</key><value>prod</value></item></tagSet></item></vpcSet></DescribeVpcsResponse>`)
		case action == "DescribeInstances":
			io.WriteString(w, `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>1</requestId><reservationSet><item><reservationId>r-1</reservationId><instancesSet><item><instanceId>i-1</instanceId><instanceType>t3.medium</instanceType><instanceState><code>16</code><name>running</name></instanceState><platformDetails>Linux/UNIX</platformDetails><architecture>x86_64</architecture><imageId>ami-1</imageId><vpcId>vpc-1</vpcId><subnetId>subnet-1</subnetId><privateIpAddress>10.0.1.5</privateIpAddress><ipAddress>3.3.3.3</ipAddress><placement><availabilityZone>eu-west-1a</availabilityZone></placement><groupSet><item><groupId>sg-1</groupId><groupName>web</groupName></item></groupSet><blockDeviceMapping><item><deviceName>/dev/xvda</deviceName><ebs><volumeId>vol-1</volumeId></ebs></item></blockDeviceMapping><rootDeviceType>ebs</rootDeviceType></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
		case action != "": // any other EC2 call: missing permission
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>1</RequestID></Response>`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/clusters"): // EKS
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"clusters":[]}`)
		case strings.Contains(r.Header.Get("X-Amz-Target"), "ListClusters"): // ECS
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"__type":"AccessDeniedException","message":"denied"}`)
		default:
			t.Errorf("unexpected request %s %s %q", r.Method, r.URL.Path, r.Header.Get("X-Amz-Target"))
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestCollect(t *testing.T) {
	srv := fakeAWS(t)
	defer srv.Close()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	inv, gaps, err := Collect(context.Background(), Options{Regions: []string{"eu-west-1"}, endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if inv.AccountID != "123456789012" || len(inv.Regions) != 1 {
		t.Fatalf("identity/regions: %+v", inv)
	}
	r := inv.Regions[0]
	if len(r.Network.VPCs) != 1 || r.Network.VPCs[0].Name != "prod" {
		t.Errorf("vpcs: %+v", r.Network.VPCs)
	}
	if len(r.Compute.Instances) != 1 {
		t.Fatalf("instances: %+v", r.Compute.Instances)
	}
	in := r.Compute.Instances[0]
	if in.State != "running" || !in.HasPublicIP || in.AZ != "eu-west-1a" || len(in.VolumeIDs) != 1 || in.SecurityGroups[0] != "sg-1" {
		t.Errorf("instance: %+v", in)
	}
	codes := map[string]string{}
	for _, g := range gaps {
		codes[g.Area] = g.Code
		if g.Scope != "aws:eu-west-1" {
			t.Errorf("scope: %+v", g)
		}
	}
	for _, area := range []string{"subnets", "route_tables", "security_groups", "flow_logs", "volumes"} {
		if codes[area] != "UnauthorizedOperation" {
			t.Errorf("gap for %s = %q", area, codes[area])
		}
	}
	if codes["ecs_clusters"] != "AccessDeniedException" {
		t.Errorf("ecs gap = %q", codes["ecs_clusters"])
	}
	if _, ok := codes["eks_clusters"]; ok {
		t.Error("eks succeeded with zero clusters; no gap expected")
	}
}
