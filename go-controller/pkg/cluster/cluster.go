package cluster

import (
	"fmt"
	"net"
	"net/url"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/client-go/kubernetes"
)

// OvnClusterController is the object holder for utilities meant for cluster management
type OvnClusterController struct {
	Kube                  kube.Interface
	watchFactory          *factory.WatchFactory
	masterSubnetAllocator *netutils.SubnetAllocator

	ClusterIPNet          *net.IPNet
	ClusterServicesSubnet string
	HostSubnetLength      uint32

	GatewayInit      bool
	GatewayIntf      string
	GatewayBridge    string
	GatewayNextHop   string
	GatewaySpareIntf bool
	NodePortEnable   bool
	OvnHA            bool
	OvnDbExist       bool
}

const (
	// OvnHostSubnet is the constant string representing the annotation key
	OvnHostSubnet = "ovn_host_subnet"
	// DefaultNamespace is the name of the default namespace
	DefaultNamespace = "default"
	// MasterOverlayIP is the overlay IP address on master node
	MasterOverlayIP = "master_overlay_ip"
)

// NewClusterController creates a new controller for IP subnet allocation to
// a given resource type (either Namespace or Node)
func NewClusterController(kubeClient kubernetes.Interface, wf *factory.WatchFactory) *OvnClusterController {
	return &OvnClusterController{
		Kube:         &kube.Kube{KClient: kubeClient},
		watchFactory: wf,
	}
}

func setOVSExternalIDs(nodeName string, ids ...string) error {
	nodeIP, err := netutils.GetNodeIP(nodeName)
	if err != nil {
		return fmt.Errorf("failed to obtain local IP from hostname %q: %v", nodeName, err)
	}

	args := []string{
		"set",
		"Open_vSwitch",
		".",
		"external_ids:ovn-encap-type=geneve",
		"external_ids:ovn-encap-ip=" + nodeIP,
	}
	for _, str := range ids {
		args = append(args, "external_ids:"+str)
	}
	_, stderr, err := util.RunOVSVsctl(args...)
	if err != nil {
		return fmt.Errorf("error setting OVS external IDs: %v\n  %q", err, stderr)
	}
	return nil
}

func setupOVNNode(nodeName, kubeServer, kubeToken string) error {
	// Tell ovn-*bctl how to talk to the database
	for _, auth := range []*config.OvnDBAuth{
		config.OvnNorth.ClientAuth,
		config.OvnSouth.ClientAuth} {
		if err := auth.SetDBAuth(); err != nil {
			return err
		}
	}

	// Tell other utilities (ovn-k8s-cni-overlay, etc) how to talk to Kubernetes
	if _, err := url.Parse(kubeServer); err != nil {
		return fmt.Errorf("error parsing k8s server %q: %v", kubeServer, err)
	}
	return setOVSExternalIDs(
		nodeName,
		fmt.Sprintf("k8s-api-server=\"%s\"", kubeServer),
		fmt.Sprintf("k8s-api-token=\"%s\"", kubeToken))
}

func setupOVNMaster(nodeName string) error {
	// Configure both server and client of OVN databases, since master uses both
	for _, auth := range []*config.OvnDBAuth{
		config.OvnNorth.ServerAuth,
		config.OvnNorth.ClientAuth,
		config.OvnSouth.ServerAuth,
		config.OvnSouth.ClientAuth} {
		if err := auth.SetDBAuth(); err != nil {
			return err
		}
	}

	return setOVSExternalIDs(nodeName)
}
