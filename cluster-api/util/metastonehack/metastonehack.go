/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package metastonehack implements hacking based on customized needs.

package metastonehack

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"

	msnetv1 "ms-sdn/pkg/netcrd/api/v1"

	"github.com/vishvananda/netlink"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getManagerGWFromDefaultTenant retrieves the subnet associated with the default tenant,
// and returns the second IPv4 address from the subnet as the managerGW.
// It uses the provided controller-runtime client to interact with the Kubernetes API.
//
// Parameters:
//
//	runtimeClient: The controller-runtime client used for Kubernetes API interactions.
//
// Returns:
//
//	The second IPv4 address from the subnet associated with the default tenant.
func getManagerGWFromDefaultTenant(runtimeClient client.Client) (string, error) {
	defaultTenantName := "ten-mng"
	defaultTenantsNamespace := "ten-mng"

	// Create a list object to hold the retrieved TenantApiServer resources.
	// Use client.MatchingFields to efficiently filter the list by the "spec.tenant" field.
	tenantApiServerList := &msnetv1.TenantApiServerList{}
	if err := runtimeClient.List(context.TODO(), tenantApiServerList, client.InNamespace(defaultTenantsNamespace), client.MatchingFields{"spec.tenant": defaultTenantName}); err != nil {
		// Return an error if the API call fails. Wrap the original error for better context.
		return "", fmt.Errorf("failed to list tenantapiservers: %w", err)
	}

	// Return an error indicating that the number of the default tenant's subnet configuration is not one.
	if len(tenantApiServerList.Items) != 1 {
		return "", fmt.Errorf("unexpected configuration: %s in namespace %s either has no subnet or has multiple subnet", defaultTenantName, defaultTenantsNamespace)
	}

	defaultTenantsSubnet := tenantApiServerList.Items[0].Spec.Subnet
	if defaultTenantsSubnet == "" {
		return "", fmt.Errorf("unexpected configuration: %s in namespace %s's subnet is empty", defaultTenantName, defaultTenantsNamespace)
	}

	// process the subnet and get the .2 address
	managerGW, _, err := net.ParseCIDR(defaultTenantsSubnet)
	if err != nil {
		return "", fmt.Errorf("unexpected configuration: %s in namespace %s has invalid subnet %s: %w", defaultTenantName, defaultTenantsNamespace, defaultTenantsSubnet, err)
	}
	managerGW = managerGW.To4() // Ensure it's an IPv4 address
	if managerGW == nil {
		return "", fmt.Errorf("unexpected configuration: %s in namespace %s's subnet %s is not IPv4", defaultTenantName, defaultTenantsNamespace, managerGW)
	}
	managerGW[3] = 2

	// If only one match is found, return the subnet from that resource.
	return managerGW.String(), nil
}

// getApiServerFromCurrentCluster retrieves the API server endpoint host
// from the given cluster object.
//
// Parameters:
//   - cluster: A pointer to a Cluster object that contains the cluster's
//     specification, including the control plane endpoint.
//
// Returns:
// - A string representing the host of the API server for the current cluster.
func getApiServersIPFromCurrentCluster(cluster *clusterv1.Cluster) (apiServersIP string) {
	return cluster.Spec.ControlPlaneEndpoint.Host
}

// checkAndAddRoute checks if the route from apiServerHost/32 to managerGW exists.
// If the route exists but points to a different gateway, it returns an error.
// If the route doesn't exist, it adds the route. If it exists correctly, it prints a message.
func checkAndAddRoute(apiServersIP, managerGW string) error {
	// Construct the route to check
	routeToCheck := fmt.Sprintf("%s/32", apiServersIP)

	// Check if the route already exists
	cmd := exec.Command("ip", "route", "show")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to check existing routes: %w", err)
	}

	outputStr := string(output)
	fmt.Printf("Routing rules: %s.\n", outputStr)

	// Case 1: If the route exists with the correct manager gateway
	if strings.Contains(outputStr, routeToCheck) && strings.Contains(outputStr, managerGW) {
		fmt.Printf("Route %s already exists via manager gateway %s.\n", routeToCheck, managerGW)
		return nil
	}

	// Case 2: If the route exists but points to a different gateway, return an error
	if strings.Contains(outputStr, routeToCheck) && !strings.Contains(outputStr, managerGW) {
		return fmt.Errorf("route %s exists but points to a different gateway. Expected gateway: %s", routeToCheck, managerGW)
	}

	// Case 3: If the route doesn't exist, add the route
	commands := []string{"route", "add", routeToCheck, "via", managerGW}
	if _, err := exec.Command("ip", commands...).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add route %s via %s: %w", routeToCheck, managerGW, err)
	}

	fmt.Printf("Route %s added successfully via %s.\n", routeToCheck, managerGW)
	return nil
}

// getInterfaceNameByIP takes an IP address as input and returns the network interface name associated with it.
func getInterfaceNameByIP(ip string) (interfaceName string, err error) {
	// Parse the given IP address string to net.IP
	targetIP := net.ParseIP(ip)
	if targetIP == nil {
		return "", fmt.Errorf("invalid IP address: %s", ip)
	}

	// Get the list of network interfaces on the system
	interfaces, err := netlink.LinkList()
	if err != nil {
		return "", fmt.Errorf("failed to list network interfaces: %w", err)
	}

	// Iterate through the interfaces to find the one with the specified IP
	for _, iface := range interfaces {
		// Get the interface address list
		addrs, err := netlink.AddrList(iface, netlink.FAMILY_ALL)
		if err != nil {
			return "", fmt.Errorf("failed to get addresses for interface %s: %w", iface.Attrs().Name, err)
		}

		// Check if the target IP is in the list of addresses for this interface
		for _, addr := range addrs {
			if addr.IP.Equal(targetIP) {
				return iface.Attrs().Name, nil
			}
		}
	}

	return "", fmt.Errorf("no interface found for IP address %s", ip)
}

// disableTXOffloadAndEnableMTUProbing takes a network interface name and disables hardware TX offload.
// It also enables the TCP MTU probing option.
func disableTXOffloadAndEnableMTUProbing(interfaceName string) error {
	// Disable TX offload on the specified network interface using ethtool
	commands := []string{"--offload", interfaceName, "tx", "off"}
	_, err := exec.Command("/usr/sbin/ethtool", commands...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to disable TX offload using ethtool: %w", err)
	}

	// Enable TCP MTU probing using sysctl
	commands = []string{"-w", "net.ipv4.tcp_mtu_probing=1"}
	_, err = exec.Command("/usr/sbin/sysctl", commands...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to enable TCP MTU probing using sysctl: %w", err)
	}

	return nil
}
