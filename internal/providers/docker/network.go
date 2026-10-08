package docker

import "strings"

// Native Linux Engine does not supply Desktop's host.docker.internal DNS name.
// Let the daemon choose the gateway (including its host-gateway-ip override).
// Preserve VM-provided DNS: on Desktop and Colima it reaches the outer host,
// whereas host-gateway can point only to the Linux VM's bridge.
func hostGatewayMapping(hostOS, daemonOS, daemonName string) []string {
	if hostOS != "linux" || strings.Contains(strings.ToLower(daemonOS), "docker desktop") || strings.Contains(strings.ToLower(daemonName), "colima") {
		return nil
	}
	return []string{"host.docker.internal:host-gateway"}
}
