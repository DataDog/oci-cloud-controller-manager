// Copyright 2024 Oracle and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipam

import (
	"net"
)

// SubnetCIDRAllocator manages CIDR allocations within a specific OCI subnet.
// It tracks which CIDR ranges have been allocated to nodes to prevent overlaps.
type SubnetCIDRAllocator struct {
	// SubnetID is the OCID of the OCI subnet
	SubnetID string

	// SubnetCIDR is the CIDR block of the subnet
	SubnetCIDR *net.IPNet

	// NodeMask is the mask size for individual node CIDRs (e.g., 24 for /24)
	NodeMask int
}

// NodeCIDRAllocation represents a CIDR allocation for a specific node.
type NodeCIDRAllocation struct {
	// NodeName is the Kubernetes node name
	NodeName string

	// InstanceID is the OCI instance OCID
	InstanceID string

	// PodCIDR is the allocated pod CIDR for this node
	PodCIDR *net.IPNet

	// SubnetID is the pod subnet OCID where the node's pod VNIC is attached
	SubnetID string

	// VnicID is the OCID of the pod VNIC attached to this node
	VnicID string
}
