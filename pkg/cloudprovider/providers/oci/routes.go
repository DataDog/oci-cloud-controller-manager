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

package oci

import (
	"context"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
)

// routes implements the cloudprovider.Routes interface for OCI.
// Note: OCI uses native subnet routing via VNICs, so this implementation
// primarily validates that routing is correctly configured rather than
// managing explicit route table entries. When a secondary VNIC is attached
// to a pod subnet, OCI automatically routes traffic destined for that
// subnet to the instance.
type routes struct {
	cp *CloudProvider
}

// ListRoutes lists all managed routes for the specified cluster.
// In the OCI implementation, routes are implicitly created through
// secondary VNIC attachments in pod subnets. This method returns
// Route objects representing those VNIC-based routes.
func (r *routes) ListRoutes(ctx context.Context, clusterName string) ([]*cloudprovider.Route, error) {
	r.cp.logger.With("clusterName", clusterName).Debug("Listing routes")

	// Get all nodes in the cluster
	nodes, err := r.cp.NodeLister.List(labels.Everything())
	if err != nil {
		return nil, errors.Wrap(err, "failed to list nodes")
	}

	var routeList []*cloudprovider.Route

	// For each node with a PodCIDR, verify secondary VNIC exists
	for _, node := range nodes {
		if node.Spec.PodCIDR == "" {
			// Node doesn't have PodCIDR yet, skip
			continue
		}

		// Get the instance ID from provider ID
		instanceID, err := MapProviderIDToResourceID(node.Spec.ProviderID)
		if err != nil {
			r.cp.logger.With("node", node.Name, "error", err).Warn("Failed to map provider ID to instance ID")
			continue
		}

		// Get compartment ID for the node
		compartmentID, err := r.cp.getCompartmentIDByInstanceID(instanceID)
		if err != nil {
			r.cp.logger.With("node", node.Name, "error", err).Warn("Failed to get compartment ID")
			continue
		}

		// Check if secondary VNIC exists in pod subnet
		podVNIC, err := r.getPodVNICForNode(ctx, instanceID, compartmentID, node)
		if err != nil {
			r.cp.logger.With("node", node.Name, "error", err).Warn("Failed to get pod VNIC for node")
			continue
		}

		if podVNIC == nil {
			r.cp.logger.With("node", node.Name).Warn("No pod VNIC found for node with PodCIDR")
			continue
		}

		// Create a Route object representing the VNIC-based route
		route := &cloudprovider.Route{
			Name:            node.Name,
			TargetNode:      types.NodeName(node.Name),
			DestinationCIDR: node.Spec.PodCIDR,
		}
		routeList = append(routeList, route)
	}

	r.cp.logger.With("routeCount", len(routeList)).Debug("Listed routes")
	return routeList, nil
}

// CreateRoute creates a new route for the specified cluster.
// In the OCI implementation, routes are created implicitly when secondary
// VNICs are attached to pod subnets. This method validates that the
// target node has a secondary VNIC in the correct pod subnet. If
// autoAttachPodVNIC is enabled and the VNIC doesn't exist, it will
// attach one.
func (r *routes) CreateRoute(ctx context.Context, clusterName string, nameHint string, route *cloudprovider.Route) error {
	r.cp.logger.With(
		"clusterName", clusterName,
		"nameHint", nameHint,
		"targetNode", route.TargetNode,
		"destinationCIDR", route.DestinationCIDR,
	).Debug("Creating route")

	// Get the target node
	node, err := r.cp.NodeLister.Get(string(route.TargetNode))
	if err != nil {
		return errors.Wrapf(err, "failed to get node %s", route.TargetNode)
	}

	// Get the instance ID from provider ID
	instanceID, err := MapProviderIDToResourceID(node.Spec.ProviderID)
	if err != nil {
		return errors.Wrap(err, "failed to map provider ID to instance ID")
	}

	// Get compartment ID for the node
	compartmentID, err := r.cp.getCompartmentIDByInstanceID(instanceID)
	if err != nil {
		return errors.Wrap(err, "failed to get compartment ID")
	}

	// Check if secondary VNIC exists in pod subnet
	podVNIC, err := r.getPodVNICForNode(ctx, instanceID, compartmentID, node)
	if err != nil {
		return errors.Wrap(err, "failed to get pod VNIC for node")
	}

	if podVNIC != nil {
		// VNIC already exists, routing is automatically handled by OCI
		r.cp.logger.With(
			"node", node.Name,
			"vnicID", *podVNIC.Id,
		).Info("Pod VNIC already exists, routing is handled natively by OCI VCN")
		return nil
	}

	// VNIC doesn't exist
	if !r.cp.config.IPAM.AutoAttachPodVNIC {
		// Auto-attach is disabled, return error
		return errors.Errorf("no pod VNIC found for node %s and autoAttachPodVNIC is disabled", node.Name)
	}

	// Auto-attach is enabled, attach a secondary VNIC
	r.cp.logger.With("node", node.Name).Info("Attaching pod VNIC to node")

	// Get availability domain from node labels
	availabilityDomain, ok := node.Labels["topology.kubernetes.io/zone"]
	if !ok {
		// Fall back to deprecated label
		availabilityDomain, ok = node.Labels["failure-domain.beta.kubernetes.io/zone"]
		if !ok {
			return errors.New("node does not have availability domain label (topology.kubernetes.io/zone)")
		}
	}

	// Get the pod subnet ID for this availability domain
	// Try exact match first
	podSubnetID, ok := r.cp.config.IPAM.PodSubnetIDs[availabilityDomain]
	if !ok {
		// Try with realm prefix
		for configAD, configSubnetID := range r.cp.config.IPAM.PodSubnetIDs {
			if parts := strings.Split(configAD, ":"); len(parts) == 2 && parts[1] == availabilityDomain {
				podSubnetID = configSubnetID
				ok = true
				break
			}
		}
		if !ok {
			return errors.Errorf("no pod subnet configured for availability domain %s", availabilityDomain)
		}
	}

	// Attach the VNIC
	_, err = r.cp.client.Compute().AttachVnic(ctx, instanceID, podSubnetID, r.cp.config.IPAM.PodVNICDisplayName)
	if err != nil {
		return errors.Wrap(err, "failed to attach pod VNIC")
	}

	r.cp.logger.With("node", node.Name, "subnetID", podSubnetID).Info("Successfully attached pod VNIC to node")
	return nil
}

// DeleteRoute deletes the specified route.
// In the OCI implementation, we don't actually delete routes since they are
// managed implicitly through VNIC attachments. Detaching a VNIC could
// disrupt networking, so this is a no-op. The IPAM controller is responsible
// for managing VNIC lifecycle.
func (r *routes) DeleteRoute(ctx context.Context, clusterName string, route *cloudprovider.Route) error {
	r.cp.logger.With(
		"clusterName", clusterName,
		"targetNode", route.TargetNode,
		"destinationCIDR", route.DestinationCIDR,
	).Debug("Delete route requested (no-op for OCI)")

	// Note: We don't detach VNICs as it could disrupt pod networking.
	// VNIC cleanup should be handled separately if needed.
	return nil
}

// getPodVNICForNode finds the secondary VNIC in a pod subnet for the given node.
// Returns nil if no pod VNIC is found.
func (r *routes) getPodVNICForNode(ctx context.Context, instanceID, compartmentID string, node *v1.Node) (*core.Vnic, error) {
	// Get availability domain from node labels (set by kubelet/cloud provider)
	// Standard Kubernetes topology label
	availabilityDomain, ok := node.Labels["topology.kubernetes.io/zone"]
	if !ok {
		// Fall back to deprecated label
		availabilityDomain, ok = node.Labels["failure-domain.beta.kubernetes.io/zone"]
		if !ok {
			return nil, errors.New("node does not have availability domain label (topology.kubernetes.io/zone)")
		}
	}

	// Get the pod subnet ID for this availability domain
	// Try exact match first
	podSubnetID, ok := r.cp.config.IPAM.PodSubnetIDs[availabilityDomain]
	if !ok {
		// If not found, try with realm prefix (format: "realm:AD-NAME")
		// This handles cases where config uses full format but label has short format
		for configAD, configSubnetID := range r.cp.config.IPAM.PodSubnetIDs {
			// Check if the config AD ends with the node's AD (after the colon)
			if parts := strings.Split(configAD, ":"); len(parts) == 2 && parts[1] == availabilityDomain {
				podSubnetID = configSubnetID
				ok = true
				break
			}
		}
		if !ok {
			// No pod subnet configured for this AD
			return nil, errors.Errorf("no pod subnet configured for availability domain %s", availabilityDomain)
		}
	}

	// Get secondary VNICs for the instance
	secondaryVNICs, err := r.cp.client.Compute().GetSecondaryVNICsForInstance(ctx, compartmentID, instanceID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get secondary VNICs")
	}

	// Look for a VNIC in the pod subnet
	for _, vnic := range secondaryVNICs {
		if vnic.SubnetId != nil && *vnic.SubnetId == podSubnetID {
			return vnic, nil
		}
	}

	// No pod VNIC found
	return nil, nil
}
