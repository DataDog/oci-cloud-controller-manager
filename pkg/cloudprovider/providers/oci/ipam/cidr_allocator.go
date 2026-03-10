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
	"math/big"
	"net"
	"sync"

	"github.com/pkg/errors"
)

// cidrSet implements a bitmap-based CIDR range allocator.
// It tracks which CIDR ranges have been allocated from a larger subnet CIDR.
type cidrSet struct {
	sync.Mutex

	// clusterCIDR is the overall CIDR block to allocate from
	clusterCIDR *net.IPNet

	// nodeMask is the mask size for individual node allocations (e.g., 24 for /24)
	nodeMask int

	// allocatedCIDRs tracks which CIDRs have been allocated
	// Key is the CIDR string (e.g., "10.244.1.0/24")
	allocatedCIDRs map[string]bool

	// maxCIDRs is the maximum number of CIDRs that can be allocated
	maxCIDRs int

	// nextCandidate is a hint for where to start searching for the next available CIDR
	nextCandidate int
}

// newCIDRSet creates a new CIDR allocator for the given cluster CIDR and node mask size.
func newCIDRSet(clusterCIDR *net.IPNet, nodeMask int) (*cidrSet, error) {
	// Validate that node mask is larger than (more specific than) cluster mask
	clusterMask, _ := clusterCIDR.Mask.Size()
	if nodeMask <= clusterMask {
		return nil, errors.Errorf("node mask (%d) must be larger than cluster mask (%d)", nodeMask, clusterMask)
	}

	// Calculate maximum number of node CIDRs that can fit in the cluster CIDR
	maxCIDRs := 1 << uint(nodeMask-clusterMask)

	return &cidrSet{
		clusterCIDR:    clusterCIDR,
		nodeMask:       nodeMask,
		allocatedCIDRs: make(map[string]bool),
		maxCIDRs:       maxCIDRs,
		nextCandidate:  0,
	}, nil
}

// AllocateNext allocates the next available CIDR range.
// Returns the allocated CIDR or an error if no CIDRs are available.
func (s *cidrSet) AllocateNext() (*net.IPNet, error) {
	s.Lock()
	defer s.Unlock()

	if len(s.allocatedCIDRs) >= s.maxCIDRs {
		return nil, errors.New("CIDR allocation exhausted: no more CIDRs available")
	}

	// Search for an available CIDR starting from nextCandidate
	for i := 0; i < s.maxCIDRs; i++ {
		candidate := (s.nextCandidate + i) % s.maxCIDRs
		cidr := s.indexToCIDR(candidate)

		if !s.allocatedCIDRs[cidr.String()] {
			// Found an available CIDR
			s.allocatedCIDRs[cidr.String()] = true
			s.nextCandidate = (candidate + 1) % s.maxCIDRs
			return cidr, nil
		}
	}

	return nil, errors.New("failed to find available CIDR (unexpected)")
}

// Occupy marks a CIDR as occupied.
// This is used to register existing CIDR allocations (e.g., from nodes that already have CIDRs).
// Returns an error if the CIDR is outside the cluster CIDR or already occupied.
func (s *cidrSet) Occupy(cidr *net.IPNet) error {
	s.Lock()
	defer s.Unlock()

	// Verify the CIDR is within the cluster CIDR
	if !s.clusterCIDR.Contains(cidr.IP) {
		return errors.Errorf("CIDR %s is not within cluster CIDR %s", cidr.String(), s.clusterCIDR.String())
	}

	// Verify the CIDR has the correct mask size
	maskSize, _ := cidr.Mask.Size()
	if maskSize != s.nodeMask {
		return errors.Errorf("CIDR %s has mask size %d, expected %d", cidr.String(), maskSize, s.nodeMask)
	}

	cidrStr := cidr.String()
	if s.allocatedCIDRs[cidrStr] {
		return errors.Errorf("CIDR %s is already occupied", cidrStr)
	}

	s.allocatedCIDRs[cidrStr] = true
	return nil
}

// Release marks a CIDR as available for reallocation.
// Returns an error if the CIDR was not previously allocated.
func (s *cidrSet) Release(cidr *net.IPNet) error {
	s.Lock()
	defer s.Unlock()

	cidrStr := cidr.String()
	if !s.allocatedCIDRs[cidrStr] {
		return errors.Errorf("CIDR %s is not currently allocated", cidrStr)
	}

	delete(s.allocatedCIDRs, cidrStr)
	return nil
}

// indexToCIDR converts an index (0 to maxCIDRs-1) to a CIDR.
func (s *cidrSet) indexToCIDR(index int) *net.IPNet {
	// Get the base IP of the cluster CIDR
	baseIP := s.clusterCIDR.IP

	// Calculate the offset for this index
	// Each index represents a block of IPs of size 2^(32-nodeMask)
	clusterMask, _ := s.clusterCIDR.Mask.Size()
	blockSize := 1 << uint(s.nodeMask-clusterMask)
	offset := uint64(index) * uint64(blockSize)

	// Convert base IP to integer, add offset, convert back to IP
	baseInt := ipToInt(baseIP)
	newInt := big.NewInt(0).Add(baseInt, big.NewInt(int64(offset)))
	newIP := intToIP(newInt)

	// Create the CIDR with the node mask
	return &net.IPNet{
		IP:   newIP,
		Mask: net.CIDRMask(s.nodeMask, 32), // Assuming IPv4 for now
	}
}

// GetAllocatedCIDRs returns a list of all currently allocated CIDRs.
func (s *cidrSet) GetAllocatedCIDRs() []string {
	s.Lock()
	defer s.Unlock()

	cidrs := make([]string, 0, len(s.allocatedCIDRs))
	for cidr := range s.allocatedCIDRs {
		cidrs = append(cidrs, cidr)
	}
	return cidrs
}

// GetUsage returns allocation statistics.
func (s *cidrSet) GetUsage() (allocated, available, max int) {
	s.Lock()
	defer s.Unlock()

	allocated = len(s.allocatedCIDRs)
	max = s.maxCIDRs
	available = max - allocated
	return
}

// ipToInt converts an IP address to a big integer.
func ipToInt(ip net.IP) *big.Int {
	// Ensure we're working with IPv4
	ip = ip.To4()
	if ip == nil {
		return big.NewInt(0)
	}

	val := big.NewInt(0)
	val.SetBytes(ip)
	return val
}

// intToIP converts a big integer to an IP address.
func intToIP(i *big.Int) net.IP {
	// Convert to 4-byte representation (IPv4)
	bytes := i.Bytes()

	// Pad to 4 bytes if necessary
	if len(bytes) < 4 {
		padded := make([]byte, 4)
		copy(padded[4-len(bytes):], bytes)
		bytes = padded
	}

	return net.IP(bytes)
}

// ParseCIDR is a helper function that parses a CIDR string and returns the IPNet.
// It's similar to net.ParseCIDR but returns only the IPNet for convenience.
func ParseCIDR(cidr string) (*net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse CIDR %s", cidr)
	}
	return ipNet, nil
}

// IsCIDRAllocated checks if a specific CIDR is already allocated.
func (s *cidrSet) IsCIDRAllocated(cidr *net.IPNet) bool {
	s.Lock()
	defer s.Unlock()

	return s.allocatedCIDRs[cidr.String()]
}

// GetUtilizationPercent returns the utilization percentage (0-100).
func (s *cidrSet) GetUtilizationPercent() float64 {
	s.Lock()
	defer s.Unlock()

	if s.maxCIDRs == 0 {
		return 0
	}

	return (float64(len(s.allocatedCIDRs)) / float64(s.maxCIDRs)) * 100.0
}
