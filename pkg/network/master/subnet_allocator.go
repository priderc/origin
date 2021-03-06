package master

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

var ErrSubnetAllocatorFull = fmt.Errorf("No subnets available.")

type SubnetAllocator struct {
	network    *net.IPNet
	hostBits   uint32
	leftShift  uint32
	leftMask   uint32
	rightShift uint32
	rightMask  uint32
	next       uint32
	allocMap   map[string]bool
	mutex      sync.Mutex
}

func newSubnetAllocator(network string, hostBits uint32) (*SubnetAllocator, error) {
	_, netIP, err := net.ParseCIDR(network)
	if err != nil {
		return nil, fmt.Errorf("failed to parse network address: %q", network)
	}

	netMaskSize, _ := netIP.Mask.Size()
	if hostBits == 0 {
		return nil, fmt.Errorf("host capacity cannot be zero.")
	} else if hostBits > (32 - uint32(netMaskSize)) {
		return nil, fmt.Errorf("subnet capacity cannot be larger than number of networks available.")
	}
	subnetBits := 32 - uint32(netMaskSize) - hostBits

	// In the simple case, the subnet part of the 32-bit IP address is just the subnet
	// number shifted hostBits to the left. However, if hostBits isn't a multiple of
	// 8, then it can be difficult to distinguish the subnet part and the host part
	// visually. (Eg, given network="10.1.0.0/16" and hostBits=6, then "10.1.0.50" and
	// "10.1.0.70" are on different networks.)
	//
	// To try to avoid this confusion, if the subnet extends into the next higher
	// octet, we rotate the bits of the subnet number so that we use the subnets with
	// all 0s in the shared octet first. So again given network="10.1.0.0/16",
	// hostBits=6, we first allocate 10.1.0.0/26, 10.1.1.0/26, etc, through
	// 10.1.255.0/26 (just like we would with /24s in the hostBits=8 case), and only
	// if we use up all of those subnets do we start allocating 10.1.0.64/26,
	// 10.1.1.64/26, etc.
	var leftShift, rightShift uint32
	var leftMask, rightMask uint32
	if hostBits%8 != 0 && ((hostBits-1)/8 != (hostBits+subnetBits-1)/8) {
		leftShift = 8 - (hostBits % 8)
		leftMask = uint32(1)<<(32-uint32(netMaskSize)) - 1
		rightShift = subnetBits - leftShift
		rightMask = (uint32(1)<<leftShift - 1) << hostBits
	} else {
		leftShift = 0
		leftMask = 0xFFFFFFFF
		rightShift = 0
		rightMask = 0
	}

	return &SubnetAllocator{
		network:    netIP,
		hostBits:   hostBits,
		leftShift:  leftShift,
		leftMask:   leftMask,
		rightShift: rightShift,
		rightMask:  rightMask,
		next:       0,
		allocMap:   make(map[string]bool),
	}, nil
}

func (sna *SubnetAllocator) markAllocatedNetwork(ipNet *net.IPNet) error {
	sna.mutex.Lock()
	defer sna.mutex.Unlock()

	if !sna.network.Contains(ipNet.IP) {
		return fmt.Errorf("provided subnet doesn't belong to network: %v", ipNet)
	}
	if !sna.allocMap[ipNet.String()] {
		sna.allocMap[ipNet.String()] = true
	}
	return nil
}

func (sna *SubnetAllocator) allocateNetwork() (*net.IPNet, error) {
	var (
		numSubnets    uint32
		numSubnetBits uint32
	)
	sna.mutex.Lock()
	defer sna.mutex.Unlock()

	baseipu := IPToUint32(sna.network.IP)
	netMaskSize, _ := sna.network.Mask.Size()
	numSubnetBits = 32 - uint32(netMaskSize) - sna.hostBits
	numSubnets = 1 << numSubnetBits

	var i uint32
	for i = 0; i < numSubnets; i++ {
		n := (i + sna.next) % numSubnets
		shifted := n << sna.hostBits
		ipu := baseipu | ((shifted << sna.leftShift) & sna.leftMask) | ((shifted >> sna.rightShift) & sna.rightMask)
		genIp := Uint32ToIP(ipu)
		genSubnet := &net.IPNet{IP: genIp, Mask: net.CIDRMask(int(numSubnetBits)+netMaskSize, 32)}
		if !sna.allocMap[genSubnet.String()] {
			sna.allocMap[genSubnet.String()] = true
			sna.next = n + 1
			return genSubnet, nil
		}
	}

	sna.next = 0
	return nil, ErrSubnetAllocatorFull
}

func (sna *SubnetAllocator) releaseNetwork(ipnet *net.IPNet) error {
	sna.mutex.Lock()
	defer sna.mutex.Unlock()

	if !sna.network.Contains(ipnet.IP) {
		return fmt.Errorf("provided subnet %v doesn't belong to the network %v.", ipnet, sna.network)
	}

	ipnetStr := ipnet.String()
	if !sna.allocMap[ipnetStr] {
		return fmt.Errorf("provided subnet %v is already available.", ipnet)
	} else {
		sna.allocMap[ipnetStr] = false
	}
	return nil
}

func IPToUint32(ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip.To4())
}

func Uint32ToIP(u uint32) net.IP {
	ip := make([]byte, 4)
	binary.BigEndian.PutUint32(ip, u)
	return net.IPv4(ip[0], ip[1], ip[2], ip[3])
}

//--------------------- Master methods ----------------------

func (master *OsdnMaster) initSubnetAllocators() error {
	for _, cn := range master.networkInfo.ClusterNetworks {
		sa, err := newSubnetAllocator(cn.ClusterCIDR.String(), cn.HostSubnetLength)
		if err != nil {
			return err
		}
		master.subnetAllocatorList = append(master.subnetAllocatorList, sa)
		master.subnetAllocatorMap[cn] = sa
	}

	// Populate subnet allocator
	subnets, err := master.networkClient.NetworkV1().HostSubnets().List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, sn := range subnets.Items {
		if err := master.markAllocatedNetwork(sn.Subnet); err != nil {
			utilruntime.HandleError(err)
		}
	}

	return nil
}

func (master *OsdnMaster) markAllocatedNetwork(subnet string) error {
	sa, ipnet, err := master.getSubnetAllocator(subnet)
	if err != nil {
		return err
	}
	if err = sa.markAllocatedNetwork(ipnet); err != nil {
		return err
	}
	return nil
}

func (master *OsdnMaster) allocateNetwork(nodeName string) (string, error) {
	var sn *net.IPNet
	var err error

	for _, possibleSubnet := range master.subnetAllocatorList {
		sn, err = possibleSubnet.allocateNetwork()
		if err == ErrSubnetAllocatorFull {
			// Current subnet exhausted, check the next one
			continue
		} else if err != nil {
			utilruntime.HandleError(fmt.Errorf("Error allocating network from subnet: %v", possibleSubnet))
			continue
		} else {
			return sn.String(), nil
		}
	}
	return "", fmt.Errorf("error allocating network for node %s: %v", nodeName, err)
}

func (master *OsdnMaster) releaseNetwork(subnet string) error {
	sa, ipnet, err := master.getSubnetAllocator(subnet)
	if err != nil {
		return err
	}
	if err = sa.releaseNetwork(ipnet); err != nil {
		return err
	}
	return nil
}

func (master *OsdnMaster) getSubnetAllocator(subnet string) (*SubnetAllocator, *net.IPNet, error) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, nil, fmt.Errorf("error parsing subnet %q: %v", subnet, err)
	}

	for _, cn := range master.networkInfo.ClusterNetworks {
		if cn.ClusterCIDR.Contains(ipnet.IP) {
			sa, ok := master.subnetAllocatorMap[cn]
			if !ok || sa == nil {
				return nil, nil, fmt.Errorf("subnet allocator not found for cluster network: %v", cn)
			}
			return sa, ipnet, nil
		}
	}
	return nil, nil, fmt.Errorf("subnet %q not found in the cluster networks: %v", subnet, master.networkInfo.ClusterNetworks)
}
