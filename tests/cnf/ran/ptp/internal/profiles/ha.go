package profiles

import (
	"context"
	"fmt"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/iface"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/metrics"
	"k8s.io/klog/v2"
)

// GetHAProfiles returns a map of HA profile names to their status for a given node.
func GetHAProfiles(ctx context.Context, prometheusAPI prometheusv1.API, nodeName string) (
	map[string]metrics.PtpHAProfileStatus, error) {
	query := metrics.HAProfileStatusQuery{
		Node: metrics.Equals(nodeName),
	}

	result, err := metrics.ExecuteQuery(ctx, prometheusAPI, query)
	if err != nil {
		return nil, fmt.Errorf("failed to execute HA profile status query for node %s: %w", nodeName, err)
	}

	haProfiles := make(map[string]metrics.PtpHAProfileStatus)

	// Iterate through the samples in the vector
	for _, sample := range result {
		// Extract the profile name from the metric labels
		profileName := string(sample.Metric["profile"])
		if profileName == "" {
			return nil, fmt.Errorf("HA profile metric missing required 'profile' label on node %s: %v",
				nodeName, sample.Metric)
		}

		// Convert the value to PtpHAProfileStatus (0 = inactive, 1 = active)
		status := metrics.PtpHAProfileStatus(sample.Value)
		haProfiles[profileName] = status
	}

	if len(haProfiles) == 0 {
		return nil, fmt.Errorf("no HA profile metrics found for node %s", nodeName)
	}

	return haProfiles, nil
}

// GetActiveHAProfiles returns a list of active HA profile names for a given node.
func GetActiveHAProfiles(ctx context.Context, prometheusAPI prometheusv1.API, nodeName string) ([]string, error) {
	haProfiles, err := GetHAProfiles(ctx, prometheusAPI, nodeName)
	if err != nil {
		return nil, err
	}

	var activeProfiles []string

	for profileName, status := range haProfiles {
		if status == metrics.HAProfileStatusActive {
			activeProfiles = append(activeProfiles, profileName)
		}
	}

	if len(activeProfiles) == 0 {
		return nil, fmt.Errorf("no active HA profiles found for node %s", nodeName)
	}

	return activeProfiles, nil
}

// GetInactiveHAProfiles returns a list of inactive HA profile names for a given node.
func GetInactiveHAProfiles(ctx context.Context, prometheusAPI prometheusv1.API, nodeName string) ([]string, error) {
	haProfiles, err := GetHAProfiles(ctx, prometheusAPI, nodeName)
	if err != nil {
		return nil, err
	}

	var inactiveProfiles []string

	for profileName, status := range haProfiles {
		if status == metrics.HAProfileStatusInactive {
			inactiveProfiles = append(inactiveProfiles, profileName)
		}
	}

	return inactiveProfiles, nil
}

// GetProfileInterfaceMap returns a map of profile names to their client (slave) interfaces for a given node.
func GetProfileInterfaceMap(nodeInfo *NodeInfo) (map[string]iface.Name, error) {
	profileInterfaceMap := make(map[string]iface.Name)

	for _, profileInfo := range nodeInfo.Profiles {
		// Get client interfaces for this profile
		clientInterfaces := profileInfo.GetInterfacesByClockType(ClockTypeClient)

		// For HA BC profiles, there should be exactly one client interface
		if len(clientInterfaces) == 1 {
			profileInterfaceMap[profileInfo.Reference.ProfileName] = clientInterfaces[0].Name
		} else if len(clientInterfaces) > 1 {
			// Take the first one, but log a warning
			klog.Warningf("Profile %s has %d client interfaces, using first one",
				profileInfo.Reference.ProfileName, len(clientInterfaces))
			profileInterfaceMap[profileInfo.Reference.ProfileName] = clientInterfaces[0].Name
		}
	}

	return profileInterfaceMap, nil
}

// WaitForHAProfileStatusChange waits for the HA profile status to change away from the specified profile.
// This is useful when deleting a profile or causing a failover.
func WaitForHAProfileStatusChange(
	ctx context.Context,
	prometheusAPI prometheusv1.API,
	nodeName string,
	oldProfileName string,
	timeout time.Duration,
) error {
	interval := 5 * time.Second
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		activeProfiles, err := GetActiveHAProfiles(ctx, prometheusAPI, nodeName)
		if err != nil {
			klog.V(5).Infof("Failed to get active HA profiles: %v", err)
			time.Sleep(interval)

			continue
		}

		// Check if the old profile is no longer active
		found := false

		for _, profileName := range activeProfiles {
			if profileName == oldProfileName {
				found = true

				break
			}
		}

		if !found {
			return nil
		}

		time.Sleep(interval)
	}

	return fmt.Errorf("HA profile %s did not change within %v on node %s",
		oldProfileName, timeout, nodeName)
}
