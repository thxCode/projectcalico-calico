package daemon

import (
	"math/rand"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/projectcalico/libcalico-go/lib/health"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/buildinfo"
	"github.com/projectcalico/felix/config"
	_ "github.com/projectcalico/felix/config"
	"github.com/projectcalico/felix/logutils"
)

// Cleanup is to start the ACL cleaner of a Felix instance.
func Cleanup(configFile string, gitVersion string, buildDate string, gitRevision string) {
	// Go's RNG is not seeded by default.  Do that now.
	rand.Seed(time.Now().UTC().UnixNano())

	// Special-case handling for environment variable-configured logging:
	// Initialise early so we can trace out config parsing.
	logutils.ConfigureEarlyLogging()

	if os.Getenv("GOGC") == "" {
		// Tune the GC to trade off a little extra CPU usage for significantly lower
		// occupancy at high scale.  This is worthwhile because Felix runs per-host so
		// any occupancy improvement is multiplied by the number of hosts.
		log.Debugf("No GOGC value set, defaulting to %d%%.", defaultGCPercent)
		debug.SetGCPercent(defaultGCPercent)
	}

	if len(buildinfo.GitVersion) == 0 && len(gitVersion) != 0 {
		buildinfo.GitVersion = gitVersion
		buildinfo.BuildDate = buildDate
		buildinfo.GitRevision = gitRevision
	}

	buildInfoLogCxt := log.WithFields(log.Fields{
		"version":    buildinfo.GitVersion,
		"builddate":  buildinfo.BuildDate,
		"gitcommit":  buildinfo.GitRevision,
		"GOMAXPROCS": runtime.GOMAXPROCS(0),
	})
	buildInfoLogCxt.Info("Felix starting up")

	// Health monitoring, for liveness and readiness endpoints.  The following loop can take a
	// while before the datastore reports itself as ready - for example when there is data that
	// needs to be migrated from a previous version - and we still want to Felix to report
	// itself as live (but not ready) while we are waiting for that.  So we create the
	// aggregator upfront and will start serving health status over HTTP as soon as we see _any_
	// config that indicates that.
	healthAggregator := health.NewHealthAggregator()

	const healthName = "felix-startup"

	// Register this function as a reporter of liveness and readiness, with no timeout.
	healthAggregator.RegisterReporter(healthName, &health.HealthReport{Live: true, Ready: true}, 0)

	// Make an initial report that says we're live but not yet ready.
	healthAggregator.Report(healthName, &health.HealthReport{Live: true, Ready: false})

	// Log out the kubernetes server details that we use in BPF mode.
	log.WithFields(log.Fields{
		"KUBERNETES_SERVICE_HOST": os.Getenv("KUBERNETES_SERVICE_HOST"),
		"KUBERNETES_SERVICE_PORT": os.Getenv("KUBERNETES_SERVICE_PORT"),
	}).Info("Kubernetes server override env vars.")

	// Load the configuration from all the different sources including the
	// datastore and merge. Keep retrying on failure.  We'll sit in this
	// loop until the datastore is ready.
	log.Info("Loading configuration...")
	var configParams *config.Config
	var numClientsCreated int
configRetry:
	for {
		if numClientsCreated > 60 {
			// If we're in a restart loop, periodically exit (so we can be restarted) since
			// - it may solve the problem if there's something wrong with our process
			// - it prevents us from leaking connections to the datastore.
			exitWithCustomRC(configChangedRC, "Restarting to avoid leaking datastore connections")
		}

		// Make an initial report that says we're live but not yet ready.
		healthAggregator.Report(healthName, &health.HealthReport{Live: true, Ready: false})

		// Load locally-defined config, including the datastore connection
		// parameters. First the environment variables.
		configParams = config.New()
		envConfig := config.LoadConfigFromEnvironment(os.Environ())
		// Then, the config file.
		log.Infof("Loading config file: %v", configFile)
		fileConfig, err := config.LoadConfigFile(configFile)
		if err != nil {
			log.WithError(err).WithField("configFile", configFile).Error(
				"Failed to load configuration file")
			time.Sleep(1 * time.Second)
			continue configRetry
		}
		// Parse and merge the local config.
		_, err = configParams.UpdateFrom(envConfig, config.EnvironmentVariable)
		if err != nil {
			log.WithError(err).WithField("configFile", configFile).Error(
				"Failed to parse configuration environment variable")
			time.Sleep(1 * time.Second)
			continue configRetry
		}
		_, err = configParams.UpdateFrom(fileConfig, config.ConfigFile)
		if err != nil {
			log.WithError(err).WithField("configFile", configFile).Error(
				"Failed to parse configuration file")
			time.Sleep(1 * time.Second)
			continue configRetry
		}

		// Each time round this loop, check that we're serving health reports if we should
		// be, or cancel any existing server if we should not be serving any more.
		healthAggregator.ServeHTTP(configParams.HealthEnabled, configParams.HealthHost, configParams.HealthPort)

		break configRetry
	}

	if numClientsCreated > 2 {
		// We don't have a way to close datastore connection so, if we reconnected after
		// a failure to load config, restart felix to avoid leaking connections.
		exitWithCustomRC(configChangedRC, "Restarting to avoid leaking datastore connections")
	}

	// We're now both live and ready.
	healthAggregator.Report(healthName, &health.HealthReport{Live: true, Ready: true})

	// Enable or disable the health HTTP server according to coalesced config.
	healthAggregator.ServeHTTP(configParams.HealthEnabled, configParams.HealthHost, configParams.HealthPort)

	// If we get here, we've loaded the configuration successfully.
	// Update log levels before we do anything else.
	logutils.ConfigureLogging(configParams)
	// Since we may have enabled more logging, log with the build context
	// again.
	buildInfoLogCxt.WithField("config", configParams).Info(
		"Successfully loaded configuration.")

	// Register signal handlers to dump memory/CPU profiles.
	logutils.RegisterProfilingSignalHandlers(configParams)

	// Do clean
	failureReportChan := doClean()

	// Now monitor the worker process and our worker threads and shut
	// down the process gracefully if they fail.
	monitorAndManageShutdown(failureReportChan, nil, nil)
}
