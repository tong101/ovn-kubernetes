package cluster

import (
	"fmt"
	"net"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/sirupsen/logrus"
)

// RebuildClusterNodes rebuilds db for cluster nodes
func (cluster *OvnClusterController) RebuildClusterNodes(
	masterNodeName string, oc *ovn.Controller) error {
	var err error

	// If OvnHA options is enabled, make sure default namespace has the
	// annotation of current cluster master node's overlay IP address
	if cluster.OvnHA && !cluster.OvnDbExist {
		// Rebuild OVN DB for the k8s nodes
		err = cluster.UpdateKubeNodes(masterNodeName)
		if err != nil {
			return err
		}
		// Rebuild OVN DB for the k8s namespaces and all the resource
		// objects inside the namespace including pods and network
		// policies
		err = cluster.UpdateKubeNsObjects(oc)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateKubeNodes rebuilds ovn db for k8s nodes
func (cluster *OvnClusterController) UpdateKubeNodes(
	masterNodeName string) error {
	nodes, err := cluster.Kube.GetNodes()
	if err != nil {
		logrus.Errorf("Failed to obtain k8s nodes: %v", err)
		return err
	}
	for _, node := range nodes.Items {
		sub, ok := node.Annotations[OvnHostSubnet]
		if ok {
			// Create a logical switch for the node and connect it to
			// the distributed router
			_, subnet, err := net.ParseCIDR(sub)
			if err != nil {
				logrus.Errorf("Invalid host subnet found for node %s - %v",
					node.Name, err)
				return err
			}
			err = ovn.CreateManagementPort(node.Name, subnet.String(),
				cluster.ClusterIPNet.String(),
				cluster.ClusterServicesSubnet)
			if err != nil {
				return err
			}
			// Update master node IP annotation on default namespace
			err = cluster.UpdateMasterNodeIP(masterNodeName)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// UpdateKubeNsObjects rebuilds ovn db for k8s namespaces
// and pods/networkpolicies inside the namespaces.
func (cluster *OvnClusterController) UpdateKubeNsObjects(
	oc *ovn.Controller) error {
	namespaces, err := cluster.Kube.GetNamespaces()
	if err != nil {
		logrus.Errorf("Failed to get k8s namespaces: %v", err)
		return err
	}
	for _, ns := range namespaces.Items {
		oc.AddNamespace(&ns)
		pods, err := cluster.Kube.GetPods(ns.Name)
		if err != nil {
			logrus.Errorf("Failed to get k8s pods: %v", err)
			return err
		}
		for _, pod := range pods.Items {
			oc.AddLogicalPortWithIP(&pod)
		}
		policies, err := cluster.Kube.GetNetworkPolicies(ns.Name)
		if err != nil {
			logrus.Errorf("Failed to get k8s network policies: %v", err)
			return err
		}
		for _, policy := range policies.Items {
			oc.AddNetworkPolicy(&policy)
		}
	}
	return nil
}

// UpdateMasterNodeIP add annotations of master node IP on
// default namespace
func (cluster *OvnClusterController) UpdateMasterNodeIP(
	masterNodeName string) error {
	masterNodeIP, err := netutils.GetNodeIP(masterNodeName)
	if err != nil {
		return fmt.Errorf("Failed to obtain local IP from master node "+
			"%q: %v", masterNodeName, err)
	}

	defaultNs, err := cluster.Kube.GetNamespace(DefaultNamespace)
	if err != nil {
		return fmt.Errorf("Failed to get default namespace: %v", err)
	}

	// Get overlay ip on OVN master node from default namespace. If it
	// doesn't have it or the IP address is different than the current one,
	// set it with current master overlay IP.
	masterIP, ok := defaultNs.Annotations[MasterOverlayIP]
	if !ok || masterIP != masterNodeIP {
		err := cluster.Kube.SetAnnotationOnNamespace(defaultNs, MasterOverlayIP,
			masterNodeIP)
		if err != nil {
			return fmt.Errorf("Failed to set %s=%s on namespace %s: %v",
				MasterOverlayIP, masterNodeIP, defaultNs.Name, err)
		}
	}

	return nil
}

func (cluster *OvnClusterController) checkOvnDBExist(
	masterNodeName string) error {
	// First check if ovn db has master node logical switch. If not, we
	// assume that the ovn db is crashed and and needs to be rebuilt.
	masterSwitch, stderr, err := util.RunOVNNbctl("--if-exist", "get",
		"logical_switch", masterNodeName, "name")
	if err != nil {
		logrus.Errorf("Failed to get logical switch %s, stderr: %q, "+
			"error: %v", masterNodeName, stderr, err)
		return err
	}
	if masterSwitch == "" {
		cluster.OvnDbExist = false
	} else {
		cluster.OvnDbExist = true
	}

	return nil
}

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (cluster *OvnClusterController) StartClusterMaster(masterNodeName string) error {
	err := cluster.checkOvnDBExist(masterNodeName)
	if err != nil {
		logrus.Errorf("Error in getting master node logical swtich: %v",
			err)
		return err
	}
	clusterNetwork := cluster.ClusterIPNet
	hostSubnetLength := cluster.HostSubnetLength

	subrange := make([]string, 0)
	existingNodes, err := cluster.Kube.GetNodes()
	if err != nil {
		logrus.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, node := range existingNodes.Items {
		hostsubnet, ok := node.Annotations[OvnHostSubnet]
		if ok {
			subrange = append(subrange, hostsubnet)
		}
	}
	masterSwitchNetwork, err := calculateMasterSwitchNetwork(clusterNetwork.String(), hostSubnetLength)
	if err != nil {
		return err
	}
	// Add the masterSwitchNetwork to subrange so that it is counted as one already taken
	subrange = append(subrange, masterSwitchNetwork)
	// NewSubnetAllocator is a subnet IPAM, which takes a CIDR (first argument)
	// and gives out subnets of length 'hostSubnetLength' (second argument)
	// but omitting any that exist in 'subrange' (third argument)
	cluster.masterSubnetAllocator, err = netutils.NewSubnetAllocator(clusterNetwork.String(), hostSubnetLength, subrange)
	if err != nil {
		return err
	}

	// now go over the 'existing' list again and create annotations for those who do not have it
	for _, node := range existingNodes.Items {
		_, ok := node.Annotations[OvnHostSubnet]
		if !ok {
			err := cluster.addNode(&node)
			if err != nil {
				logrus.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
		}
	}

	if err := cluster.SetupMaster(masterNodeName, masterSwitchNetwork); err != nil {
		return err
	}

	// go routine to watch all node events. On creation, addNode will be called that will create a subnet for the switch belonging to that node.
	// On a delete call, the subnet will be returned to the allocator as the switch is deleted from ovn
	cluster.watchNodes()
	return nil
}

func calculateMasterSwitchNetwork(clusterNetwork string, hostSubnetLength uint32) (string, error) {
	subAllocator, err := netutils.NewSubnetAllocator(clusterNetwork, hostSubnetLength, make([]string, 0))
	if err != nil {
		return "", err
	}
	sn, err := subAllocator.GetNetwork()
	return sn.String(), err
}

// SetupMaster calls the external script to create the switch and central routers for the network
func (cluster *OvnClusterController) SetupMaster(masterNodeName string, masterSwitchNetwork string) error {
	err := util.StartOVS()
	if err != nil {
		return err
	}

	err = util.StartOvnNorthd()
	if err != nil {
		return err
	}

	if err := setupOVNMaster(masterNodeName); err != nil {
		return err
	}

	// Create a single common distributed router for the cluster.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", masterNodeName, "--", "set", "logical_router", masterNodeName, "external_ids:k8s-cluster-router=yes")
	if err != nil {
		logrus.Errorf("Failed to create a single common distributed router for the cluster, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Create 2 load-balancers for east-west traffic.  One handles UDP and another handles TCP.
	k8sClusterLbTCP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
	if err != nil {
		logrus.Errorf("Failed to get tcp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}

	if k8sClusterLbTCP == "" {
		stdout, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
		if err != nil {
			logrus.Errorf("Failed to create tcp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	k8sClusterLbUDP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes")
	if err != nil {
		logrus.Errorf("Failed to get udp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}
	if k8sClusterLbUDP == "" {
		stdout, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes", "protocol=udp")
		if err != nil {
			logrus.Errorf("Failed to create udp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Create a logical switch called "join" that will be used to connect gateway routers to the distributed router.
	// The "join" will be allocated IP addresses in the range 100.64.1.0/24.
	stdout, stderr, err = util.RunOVNNbctl("--may-exist", "ls-add", "join")
	if err != nil {
		logrus.Errorf("Failed to create logical switch called \"join\", stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect the distributed router to "join".
	routerMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtoj-"+masterNodeName, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port rtoj-%v, stderr: %q, error: %v", masterNodeName, stderr, err)
		return err
	}
	if routerMac == "" {
		routerMac = util.GenerateMac()
		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lrp-add", masterNodeName, "rtoj-"+masterNodeName, routerMac, "100.64.1.1/24", "--", "set", "logical_router_port", "rtoj-"+masterNodeName, "external_ids:connect_to_join=yes")
		if err != nil {
			logrus.Errorf("Failed to add logical router port rtoj-%v, stdout: %q, stderr: %q, error: %v", masterNodeName, stdout, stderr, err)
			return err
		}
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", "join", "jtor-"+masterNodeName, "--", "set", "logical_switch_port", "jtor-"+masterNodeName, "type=router", "options:router-port=rtoj-"+masterNodeName, "addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical switch port to logical router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	err = ovn.CreateManagementPort(masterNodeName, masterSwitchNetwork,
		cluster.ClusterIPNet.String(), cluster.ClusterServicesSubnet)
	if err != nil {
		return fmt.Errorf("Failed create management port: %v", err)
	}

	// Create a lock for gateway-init to co-ordinate.
	stdout, stderr, err = util.RunOVNNbctl("--", "set", "nb_global", ".",
		"external-ids:gateway-lock=\"\"")
	if err != nil {
		logrus.Errorf("Failed to create lock for gateways, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	return nil
}

func (cluster *OvnClusterController) addNode(node *kapi.Node) error {
	// Do not create a subnet if the node already has a subnet
	hostsubnet, ok := node.Annotations[OvnHostSubnet]
	if ok {
		// double check if the hostsubnet looks valid
		_, _, err := net.ParseCIDR(hostsubnet)
		if err == nil {
			return nil
		}
	}
	// Create new subnet
	sn, err := cluster.masterSubnetAllocator.GetNetwork()
	if err != nil {
		return fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
	}

	err = cluster.Kube.SetAnnotationOnNode(node, OvnHostSubnet, sn.String())
	if err != nil {
		_ = cluster.masterSubnetAllocator.ReleaseNetwork(sn)
		return fmt.Errorf("Error creating subnet %s for node %s: %v", sn.String(), node.Name, err)
	}
	logrus.Infof("Created HostSubnet %s", sn.String())
	return nil
}

func (cluster *OvnClusterController) deleteNode(node *kapi.Node) error {
	sub, ok := node.Annotations[OvnHostSubnet]
	if !ok {
		return fmt.Errorf("Error in obtaining host subnet for node %q for deletion", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return fmt.Errorf("Error in parsing hostsubnet - %v", err)
	}
	err = cluster.masterSubnetAllocator.ReleaseNetwork(subnet)
	if err != nil {
		return fmt.Errorf("Error deleting subnet %v for node %q: %v", sub, node.Name, err)
	}

	logrus.Infof("Deleted HostSubnet %s for node %s", sub, node.Name)
	return nil
}

func (cluster *OvnClusterController) watchNodes() {
	cluster.watchFactory.AddNodeHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Added event for Node %q", node.Name)
			err := cluster.addNode(node)
			if err != nil {
				logrus.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
		},
		UpdateFunc: func(old, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*kapi.Node)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				node, ok = tombstone.Obj.(*kapi.Node)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a node %#v", obj)
					return
				}
			}
			logrus.Debugf("Delete event for Node %q", node.Name)
			err := cluster.deleteNode(node)
			if err != nil {
				logrus.Errorf("Error deleting node %s: %v", node.Name, err)
			}
		},
	}, nil)
}
