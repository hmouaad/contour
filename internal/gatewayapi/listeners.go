// Copyright Project Contour Authors
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

package gatewayapi

import (
	"fmt"
	"net"
	"strings"

	"github.com/projectcontour/contour/internal/ref"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayapi_v1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// ContourHTTPSProtocolType is the protocol for an HTTPS Listener
// that is to be used with HTTPProxy/Ingress, where the TLS
// details are provided on the HTTPProxy/Ingress rather than
// on the Listener.
const ContourHTTPSProtocolType = "projectcontour.io/https"

type ValidateListenersResult struct {
	// ListenerNames is a map from Gateway Listener name
	// to DAG/Envoy Listener name. All Gateway Listeners
	// that share a port map to the same DAG/Envoy Listener
	// name.
	ListenerNames map[string]string

	// Ports is a list of ports to listen on.
	Ports []ListenerPort

	// InvalidListenerConditions is a map from Gateway Listener name
	// to a condition to set, if the Listener is invalid.
	InvalidListenerConditions map[gatewayapi_v1beta1.SectionName]metav1.Condition
}

type ListenerPort struct {
	Name          string
	Port          int32
	ContainerPort int32
	Protocol      string
}

// ValidateListeners validates protocols, ports and hostnames on a set of listeners.
// It ensures that:
//   - protocols are supported
//   - hostnames are syntactically valid
//   - listeners on each port have mutually compatible protocols
//   - listeners on each port have unique hostnames
//
// It returns a Listener name map, the ports to use, and conditions for all invalid listeners.
// If a listener is not in the "InvalidListenerConditions" map, it is assumed to be valid according
// to the above rules.
func ValidateListeners(listeners []gatewayapi_v1beta1.Listener) ValidateListenersResult {
	// TLS-based protocols that can all exist on the same port.
	compatibleTLSProtocols := sets.New(
		gatewayapi_v1beta1.HTTPSProtocolType,
		gatewayapi_v1beta1.TLSProtocolType,
		ContourHTTPSProtocolType,
	)

	result := ValidateListenersResult{
		ListenerNames:             map[string]string{},
		InvalidListenerConditions: map[gatewayapi_v1beta1.SectionName]metav1.Condition{},
	}

	for i, listener := range listeners {
		// Check for a valid hostname.
		if hostname := ref.Val(listener.Hostname, ""); len(hostname) > 0 {
			if err := IsValidHostname(string(hostname)); err != nil {
				result.InvalidListenerConditions[listener.Name] = metav1.Condition{
					Type:    string(gatewayapi_v1beta1.ListenerConditionProgrammed),
					Status:  metav1.ConditionFalse,
					Reason:  string(gatewayapi_v1beta1.ListenerReasonInvalid),
					Message: err.Error(),
				}
				continue
			}
		}

		// Check for a supported protocol.
		switch listener.Protocol {
		case gatewayapi_v1beta1.HTTPProtocolType, gatewayapi_v1beta1.HTTPSProtocolType, gatewayapi_v1beta1.TLSProtocolType, gatewayapi_v1beta1.TCPProtocolType, ContourHTTPSProtocolType:
		default:
			result.InvalidListenerConditions[listener.Name] = metav1.Condition{
				Type:    string(gatewayapi_v1beta1.ListenerConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayapi_v1beta1.ListenerReasonUnsupportedProtocol),
				Message: fmt.Sprintf("Listener protocol %q is unsupported, must be one of HTTP, HTTPS, TLS, TCP or projectcontour.io/https", listener.Protocol),
			}
			continue
		}

		conflicted := func() bool {
			// Check for conflicts with previous Listeners only.
			// This allows Listeners that appear first in list
			// order to take precedence, i.e. to be accepted and
			// programmed, when there is a conflict.
			for j := 0; j < i; j++ {
				otherListener := listeners[j]

				if listener.Port != otherListener.Port {
					// Port ranges 57536-58558 and 58559-59581 both map to container ports
					// 1024-2046, since we can't listen on ports 1-1023 in the Envoy container.
					// If there are conflicting container ports, the listener can't be accepted.
					if toContainerPort(listener.Port) == toContainerPort(otherListener.Port) {
						result.InvalidListenerConditions[listener.Name] = metav1.Condition{
							Type:    string(gatewayapi_v1beta1.ListenerConditionAccepted),
							Status:  metav1.ConditionFalse,
							Reason:  string(gatewayapi_v1beta1.ListenerReasonPortUnavailable),
							Message: "Listener port conflicts with a previous Listener's port",
						}
						return true
					}

					// Otherwise, listeners on different ports can't conflict.
					continue
				}

				// Protocol conflict
				switch {
				case listener.Protocol == gatewayapi_v1beta1.HTTPProtocolType:
					if otherListener.Protocol != gatewayapi_v1beta1.HTTPProtocolType {
						result.InvalidListenerConditions[listener.Name] = metav1.Condition{
							Type:    string(gatewayapi_v1beta1.ListenerConditionConflicted),
							Status:  metav1.ConditionTrue,
							Reason:  string(gatewayapi_v1beta1.ListenerReasonProtocolConflict),
							Message: "All Listener protocols for a given port must be compatible",
						}
						return true
					}
				case compatibleTLSProtocols.Has(listener.Protocol):
					if !compatibleTLSProtocols.Has(otherListener.Protocol) {
						result.InvalidListenerConditions[listener.Name] = metav1.Condition{
							Type:    string(gatewayapi_v1beta1.ListenerConditionConflicted),
							Status:  metav1.ConditionTrue,
							Reason:  string(gatewayapi_v1beta1.ListenerReasonProtocolConflict),
							Message: "All Listener protocols for a given port must be compatible",
						}
						return true
					}
				case listener.Protocol == gatewayapi_v1beta1.TCPProtocolType:
					if otherListener.Protocol != gatewayapi_v1beta1.TCPProtocolType {
						result.InvalidListenerConditions[listener.Name] = metav1.Condition{
							Type:    string(gatewayapi_v1beta1.ListenerConditionConflicted),
							Status:  metav1.ConditionTrue,
							Reason:  string(gatewayapi_v1beta1.ListenerReasonProtocolConflict),
							Message: "All Listener protocols for a given port must be compatible",
						}
						return true
					}
				}

				// Hostname conflict
				if ref.Val(listener.Hostname, "") == ref.Val(otherListener.Hostname, "") {
					result.InvalidListenerConditions[listener.Name] = metav1.Condition{
						Type:    string(gatewayapi_v1beta1.ListenerConditionConflicted),
						Status:  metav1.ConditionTrue,
						Reason:  string(gatewayapi_v1beta1.ListenerReasonHostnameConflict),
						Message: "All Listener hostnames for a given port must be unique",
					}
					return true
				}
			}

			return false
		}()

		if conflicted {
			continue
		}

		// Add an entry in the Listener name map.
		var protocol string
		switch listener.Protocol {
		case gatewayapi_v1beta1.HTTPProtocolType:
			protocol = "http"
		case gatewayapi_v1beta1.HTTPSProtocolType, gatewayapi_v1beta1.TLSProtocolType, ContourHTTPSProtocolType:
			protocol = "https"
		case gatewayapi_v1beta1.TCPProtocolType:
			protocol = "tcp"
		}
		envoyListenerName := fmt.Sprintf("%s-%d", protocol, listener.Port)

		result.ListenerNames[string(listener.Name)] = envoyListenerName

		// Add the port to the list if it hasn't been added already.
		found := false
		for _, port := range result.Ports {
			if port.Name == envoyListenerName {
				found = true
				break
			}
		}

		if !found {
			result.Ports = append(result.Ports, ListenerPort{
				Name:          envoyListenerName,
				Port:          int32(listener.Port),
				ContainerPort: toContainerPort(listener.Port),
				Protocol:      protocol,
			})
		}
	}

	return result
}

func toContainerPort(listenerPort gatewayapi_v1beta1.PortNumber) int32 {
	// Add 8000 to the Listener port, wrapping around if needed,
	// and skipping over privileged ports 1-1023.

	containerPort := listenerPort + 8000

	if containerPort > 65535 {
		containerPort -= 65535
	}

	if containerPort <= 1023 {
		containerPort += 1023
	}

	return int32(containerPort)
}

// IsValidHostname validates that a given hostname is syntactically valid.
// It returns nil if valid and an error if not valid.
func IsValidHostname(hostname string) error {
	if net.ParseIP(hostname) != nil {
		return fmt.Errorf("invalid hostname %q: must be a DNS name, not an IP address", hostname)
	}

	if strings.Contains(hostname, "*") {
		if errs := validation.IsWildcardDNS1123Subdomain(hostname); errs != nil {
			return fmt.Errorf("invalid hostname %q: %v", hostname, errs)
		}
	} else {
		if errs := validation.IsDNS1123Subdomain(hostname); errs != nil {
			return fmt.Errorf("invalid hostname %q: %v", hostname, errs)
		}
	}

	return nil
}
