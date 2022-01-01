package daemon

import (
	"encoding/json"
	"os"
	"regexp"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/dataplane/windows/hns"
)

const (
	// suffix to use for IPv4 addresses.
	ipv4AddrSuffix = "/32"
	// envNetworkName specifies the environment variable which should be read
	// to obtain the name of the hns network for which we will be managing
	// endpoint policies.
	envNetworkName = "KUBE_NETWORK"
	// the default hns network name to use if the envNetworkName environment
	// variable does not resolve to a value
	defaultNetworkName = "(?i)calico.*"
)

func doClean() chan string {
	var failureReportChan = make(chan string)
	go func() {
		var berr error
		defer func() {
			if r := recover(); r != nil || berr != nil {
				if berr != nil {
					r = berr
				}
				log.Errorf("Shutting down due to fatal error: %v", r)
				failureReportChan <- reasonFatalError
				time.Sleep(gracefulShutdownTimeout)
				log.Panic("Graceful shutdown took too long")
			}
		}()

		var networkName string
		if os.Getenv(envNetworkName) != "" {
			networkName = os.Getenv(envNetworkName)
			log.WithField("NetworkName", networkName).Info("Setting hns network name from environment variable")
		} else {
			networkName = defaultNetworkName
			log.WithField("NetworkName", networkName).Info("No Network Name environment variable was found, using default name")
		}
		networkNameRegexp, berr := regexp.Compile(networkName)
		if berr != nil {
			log.WithError(berr).Panicf(
				"Supplied value (%s) for %s environment variable not a valid regular expression.",
				networkName, envNetworkName)
		}

		var apis hns.API

		log.Info("Cleaning")
		endpoints, berr := apis.HNSListEndpointRequest()
		if berr != nil {
			log.Infof("Failed to obtain HNS endpoints: %v", berr)
			return
		}

		debug := log.GetLevel() >= log.DebugLevel
		for _, endpoint := range endpoints {
			if endpoint.IsRemoteEndpoint {
				if debug {
					log.WithField("id", endpoint.Id).Debug("Skipping remote endpoint")
				}
				continue
			}
			if !networkNameRegexp.MatchString(endpoint.VirtualNetworkName) {
				if debug {
					log.WithFields(log.Fields{
						"id":          endpoint.Id,
						"ourNet":      networkNameRegexp.String(),
						"endpointNet": endpoint.VirtualNetworkName,
					}).Debug("Skipping endpoint on other HNS network")
				}
				continue
			}

			// Some CNI plugins do not clear endpoint properly when a pod has been torn down.
			// In that case, it is possible Felix sees multiple endpoints with the same IP.
			// We need to filter out inactive endpoints that do not attach to any container.
			containers, err := apis.GetAttachedContainerIDs(&endpoint)
			if err != nil {
				log.WithFields(log.Fields{
					"id":   endpoint.Id,
					"name": endpoint.Name,
				}).Warn("Failed to get attached containers")
				continue
			}
			if len(containers) == 0 {
				log.WithFields(log.Fields{
					"id":   endpoint.Id,
					"name": endpoint.Name,
				}).Warn("This is a stale endpoint with no container attached")
				continue
			}
			ip := endpoint.IPAddress.String() + ipv4AddrSuffix
			logCxt := log.WithFields(log.Fields{"IPAddress": ip, "EndpointId": endpoint.Id})
			logCxt.Debug("Cleaning HNS Endpoint all ACLs")

			var policies []json.RawMessage
			for i := range endpoint.Policies {
				var policy hns.Policy
				berr = json.Unmarshal(endpoint.Policies[i], &policy)
				if berr != nil {
					logCxt.Warnf("Failed to unmarshal policy: %v", berr)
					continue
				}
				if policy.Type == hns.ACL {
					continue
				}
				policies = append(policies, endpoint.Policies[i])
			}
			endpoint.Policies = policies
			_, berr = endpoint.Update()
			if berr != nil {
				logCxt.Warnf("Failed to update: %v", berr)
				continue
			}

			logCxt.Info("Cleaned HNS Endpoint all ACLs")
		}

		log.Info("Cleaned")
	}()
	return failureReportChan
}
