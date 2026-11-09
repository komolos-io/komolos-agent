// Package inventory defines the provider-neutral document komolos-agent writes.
//
// The document holds configuration metadata only: no secrets, no file contents,
// no application data, no environment variables. The customer reviews the file
// before anything is uploaded. Anything the collector could not read is listed
// as a Gap and reported as unknown, never guessed.
package inventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is bumped whenever a field is added, removed or changes meaning.
const SchemaVersion = "0.2"

// MaxBytes is the largest document the collector writes or uploads, and the
// largest the control plane accepts.
const MaxBytes = 10 << 20

type Inventory struct {
	SchemaVersion    string    `json:"schema_version"`
	CollectorVersion string    `json:"collector_version"`
	Provider         string    `json:"provider"` // "aws" or "ssh"
	CollectedAt      time.Time `json:"collected_at"`
	AWS              *AWS      `json:"aws,omitempty"`
	Hosts            []Host    `json:"hosts,omitempty"`
	Gaps             []Gap     `json:"gaps"`
}

// Gap records an area that could not be collected (usually a missing permission
// or a tool that is not installed). Code is a short machine-readable reason.
type Gap struct {
	Scope string `json:"scope"` // e.g. "aws:eu-west-1", "host:10.0.0.5"
	Area  string `json:"area"`
	Code  string `json:"code"`
}

// ---------- AWS ----------

type AWS struct {
	AccountID string      `json:"account_id"`
	CallerARN string      `json:"caller_arn"`
	Regions   []AWSRegion `json:"regions"`
}

type AWSRegion struct {
	Region  string  `json:"region"`
	Network Network `json:"network"`
	Compute Compute `json:"compute"`
}

type Network struct {
	VPCs           []VPC           `json:"vpcs"`
	Subnets        []Subnet        `json:"subnets"`
	RouteTables    []RouteTable    `json:"route_tables"`
	SecurityGroups []SecurityGroup `json:"security_groups"`
	FlowLogs       []FlowLog       `json:"flow_logs"` // empty means no observed-traffic evidence is available
}

type VPC struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	CIDR      string `json:"cidr"`
	IsDefault bool   `json:"is_default"`
}

type Subnet struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	VPCID  string `json:"vpc_id"`
	CIDR   string `json:"cidr"`
	AZ     string `json:"az"`
	Public bool   `json:"maps_public_ip"`
}

type Route struct {
	Destination string `json:"destination"`
	Target      string `json:"target"`
	State       string `json:"state"`
}

type RouteTable struct {
	ID      string   `json:"id"`
	VPCID   string   `json:"vpc_id"`
	Main    bool     `json:"main"`
	Subnets []string `json:"subnets"`
	Routes  []Route  `json:"routes"`
}

// Rule is a declared permission. Source groups are declared dependencies
// between components, not observed traffic.
type Rule struct {
	Protocol     string   `json:"protocol"`
	FromPort     int32    `json:"from_port"`
	ToPort       int32    `json:"to_port"`
	CIDRs        []string `json:"cidrs,omitempty"`
	SourceGroups []string `json:"source_groups,omitempty"`
}

type SecurityGroup struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	VPCID   string `json:"vpc_id"`
	Ingress []Rule `json:"ingress"`
	Egress  []Rule `json:"egress"`
}

type FlowLog struct {
	ID              string `json:"id"`
	ResourceID      string `json:"resource_id"`
	TrafficType     string `json:"traffic_type"`
	DestinationType string `json:"destination_type"`
	Status          string `json:"status"`
}

type Compute struct {
	Instances   []Instance   `json:"instances"`
	Volumes     []Volume     `json:"volumes"`
	EKSClusters []EKSCluster `json:"eks_clusters"`
	ECSClusters []ECSCluster `json:"ecs_clusters"`
}

type Instance struct {
	ID             string    `json:"id"`
	Name           string    `json:"name,omitempty"`
	Type           string    `json:"type"`
	State          string    `json:"state"`
	Platform       string    `json:"platform"` // e.g. "Linux/UNIX", "Windows"
	Architecture   string    `json:"architecture"`
	ImageID        string    `json:"image_id"`
	LaunchTime     time.Time `json:"launch_time"`
	VPCID          string    `json:"vpc_id"`
	SubnetID       string    `json:"subnet_id"`
	AZ             string    `json:"az"`
	PrivateIP      string    `json:"private_ip,omitempty"`
	HasPublicIP    bool      `json:"has_public_ip"`
	SecurityGroups []string  `json:"security_groups"`
	RootDeviceType string    `json:"root_device_type"`
	VolumeIDs      []string  `json:"volume_ids"`
}

type Volume struct {
	ID          string   `json:"id"`
	SizeGiB     int32    `json:"size_gib"`
	Type        string   `json:"type"`
	IOPS        int32    `json:"iops,omitempty"`
	Encrypted   bool     `json:"encrypted"`
	AZ          string   `json:"az"`
	State       string   `json:"state"`
	InstanceIDs []string `json:"instance_ids"`
}

type EKSCluster struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Status    string   `json:"status"`
	VPCID     string   `json:"vpc_id"`
	SubnetIDs []string `json:"subnet_ids"`
}

type ECSCluster struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	RunningTasks   int32  `json:"running_tasks"`
	ActiveServices int32  `json:"active_services"`
}

// ---------- SSH hosts ----------

type Host struct {
	Address        string           `json:"address"`
	WrapperVersion string           `json:"wrapper_version"`
	Hostname       string           `json:"hostname,omitempty"`
	OS             OSInfo           `json:"os"`
	CPUs           int              `json:"cpus,omitempty"`
	MemoryBytes    int64            `json:"memory_bytes,omitempty"`
	Virtualization string           `json:"virtualization,omitempty"`
	Vendor         string           `json:"vendor,omitempty"`
	Product        string           `json:"product,omitempty"`
	Packages       []Package        `json:"packages"`
	Listeners      []Listener       `json:"listeners"`
	Connections    []PeerCount      `json:"connections"`
	Processes      []ProcessSummary `json:"processes"`
	Services       []string         `json:"services"`
	Mounts         []Mount          `json:"mounts"`
	Disks          []Disk           `json:"disks"`
	Containers     []Container      `json:"containers"`
}

type OSInfo struct {
	ID         string `json:"id,omitempty"`
	VersionID  string `json:"version_id,omitempty"`
	PrettyName string `json:"pretty_name,omitempty"`
	Kernel     string `json:"kernel,omitempty"`
	Arch       string `json:"arch,omitempty"`
}

type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Listener is a listening socket. Process is empty when the discovery account
// cannot see who owns the socket (it is not root): unknown, not guessed.
type Listener struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Process  string `json:"process,omitempty"`
}

// PeerCount aggregates established TCP connections by remote endpoint, which is
// the evidence for "this host talks to that one".
type PeerCount struct {
	Peer  string `json:"peer"`
	Port  int    `json:"port"`
	Count int    `json:"count"`
}

// ProcessSummary counts processes by name (comm) and user. Command lines are
// never collected because they can contain secrets.
type ProcessSummary struct {
	Name  string `json:"name"`
	User  string `json:"user"`
	Count int    `json:"count"`
}

type Mount struct {
	Source     string `json:"source"`
	FSType     string `json:"fs_type"`
	SizeBytes  int64  `json:"size_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
	MountPoint string `json:"mount_point"`
}

type Disk struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	Type       string `json:"type"`
	Rotational bool   `json:"rotational"`
}

type Container struct {
	ID    string `json:"id"`
	Image string `json:"image"`
	Name  string `json:"name"`
	Ports string `json:"ports,omitempty"`
}

// ---------- reading, writing, checking ----------

// Validate checks the fields the control plane relies on.
func (inv *Inventory) Validate() error {
	if inv.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version %q is not supported (want %q)", inv.SchemaVersion, SchemaVersion)
	}
	switch inv.Provider {
	case "aws":
		if inv.AWS == nil {
			return errors.New("provider is aws but the aws section is missing")
		}
	case "ssh":
		if len(inv.Hosts) == 0 && len(inv.Gaps) == 0 {
			return errors.New("provider is ssh but no hosts or gaps were recorded")
		}
	default:
		return fmt.Errorf("unknown provider %q", inv.Provider)
	}
	if inv.CollectedAt.IsZero() {
		return errors.New("collected_at is missing")
	}
	return nil
}

// Marshal returns the indented document, ending in a newline.
func Marshal(inv *Inventory) ([]byte, error) {
	b, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if len(b) > MaxBytes {
		return nil, fmt.Errorf("inventory is %d bytes, over the %d byte limit", len(b), MaxBytes)
	}
	return b, nil
}

// WriteFile writes the document atomically with owner-only permissions.
func WriteFile(path string, inv *Inventory) error {
	b, err := Marshal(inv)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".inventory-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Decode parses and validates a document, rejecting unknown fields so a
// tampered or newer file is not silently accepted.
func Decode(b []byte) (*Inventory, error) {
	if len(b) > MaxBytes {
		return nil, fmt.Errorf("inventory is over the %d byte limit", MaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var inv Inventory
	if err := dec.Decode(&inv); err != nil {
		return nil, fmt.Errorf("parse inventory: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse inventory: trailing data after the document")
	}
	if err := inv.Validate(); err != nil {
		return nil, err
	}
	return &inv, nil
}

// SHA256 is the hex digest shown to the customer and sent with the upload.
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Summary writes a short human-readable overview for the review step.
func Summary(w io.Writer, inv *Inventory) {
	fmt.Fprintf(w, "provider:     %s\n", inv.Provider)
	fmt.Fprintf(w, "collected at: %s\n", inv.CollectedAt.Format(time.RFC3339))
	if inv.AWS != nil {
		fmt.Fprintf(w, "account:      %s\n", inv.AWS.AccountID)
		for _, r := range inv.AWS.Regions {
			fmt.Fprintf(w, "  %-14s vpcs=%d subnets=%d security_groups=%d instances=%d volumes=%d eks=%d ecs=%d\n",
				r.Region, len(r.Network.VPCs), len(r.Network.Subnets), len(r.Network.SecurityGroups),
				len(r.Compute.Instances), len(r.Compute.Volumes), len(r.Compute.EKSClusters), len(r.Compute.ECSClusters))
		}
	}
	for _, h := range inv.Hosts {
		fmt.Fprintf(w, "  host %-20s %s, listeners=%d services=%d packages=%d containers=%d\n",
			h.Address, h.OS.PrettyName, len(h.Listeners), len(h.Services), len(h.Packages), len(h.Containers))
	}
	if len(inv.Gaps) == 0 {
		fmt.Fprintln(w, "gaps:         none")
		return
	}
	fmt.Fprintf(w, "gaps:         %d (reported as unknown, not guessed)\n", len(inv.Gaps))
	for _, g := range inv.Gaps {
		fmt.Fprintf(w, "  %s %s: %s\n", g.Scope, g.Area, g.Code)
	}
}
