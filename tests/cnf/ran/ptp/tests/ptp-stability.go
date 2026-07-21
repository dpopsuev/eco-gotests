package tests

import (
	"context"
	"fmt"
	"maps"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	eventptp "github.com/redhat-cne/sdk-go/pkg/event/ptp"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/internal/nicinfo"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/internal/querier"
	. "github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/internal/raninittools"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/consumer"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/daemonlogs"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/events"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/iface"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/metrics"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/profiles"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/stability"
	"github.com/rh-ecosystem-edge/eco-gotests/tests/cnf/ran/ptp/internal/tsparams"
)

var _ = Describe("PTP Stability", Label(tsparams.LabelStability), func() {
	var prometheusAPI prometheusv1.API

	BeforeEach(func() {
		By("creating a Prometheus API client")

		var err error

		prometheusAPI, err = querier.CreatePrometheusAPIForCluster(RANConfig.Spoke1APIClient)
		Expect(err).ToNot(HaveOccurred(), "Failed to create Prometheus API client")

		By("ensuring clocks are locked before testing")

		err = metrics.EnsureClocksAreLocked(prometheusAPI)
		Expect(err).ToNot(HaveOccurred(), "Failed to assert clock state is locked")
	})

	AfterEach(func() {
		By("ensuring clocks are locked after testing")

		err := metrics.EnsureClocksAreLocked(prometheusAPI)
		Expect(err).ToNot(HaveOccurred(), "Failed to assert clock state is locked")
	})

	// 38228 - Measure the PTP Slave Clock Stability leveraging the PTP offset communicated in ptp4l logs
	It("validates PTP stability and offset behavior over configured duration", reportxml.ID("38228"), func() {
		testRanAtLeastOnce := false

		nodeInfoMap, err := profiles.GetNodeInfoMap(RANConfig.Spoke1APIClient)
		Expect(err).ToNot(HaveOccurred(), "Failed to get node info map")

		// Since the collection is sequential, test time scales linearly with the number of nodes. Node counts
		// are expected to be small, so the test is kept sequential to avoid complexity of parallelization.
		for _, nodeInfo := range nodeInfoMap {
			By("marking interfaces for nicinfo reporting on node " + nodeInfo.Name)

			for _, profile := range nodeInfo.Profiles {
				nicinfo.Node(nodeInfo.Name).MarkSeqTested(iface.NamesToStringSeq(maps.Keys(profile.Interfaces)))
			}

			testRanAtLeastOnce = true

			By("asserting ptp4l and phc2sys processes are up on node " + nodeInfo.Name)
			ptp4lProcessStatusQuery := metrics.ProcessStatusQuery{
				Node:    metrics.Equals(nodeInfo.Name),
				Process: metrics.Equals(metrics.ProcessPTP4L),
			}
			err = metrics.AssertQuery(context.TODO(), prometheusAPI, ptp4lProcessStatusQuery, metrics.ProcessStatusUp,
				metrics.AssertWithTimeout(5*time.Minute))
			Expect(err).ToNot(HaveOccurred(), "Failed to assert ptp4l process status is UP on node %s", nodeInfo.Name)

			// A single query may succeed if either process is missing from metrics, so we use a separate
			// query for each to guarantee both exist and are up.
			phc2sysProcessStatusQuery := metrics.ProcessStatusQuery{
				Node:    metrics.Equals(nodeInfo.Name),
				Process: metrics.Equals(metrics.ProcessPHC2SYS),
			}
			err = metrics.AssertQuery(context.TODO(), prometheusAPI, phc2sysProcessStatusQuery, metrics.ProcessStatusUp,
				metrics.AssertWithTimeout(5*time.Minute))
			Expect(err).ToNot(HaveOccurred(), "Failed to assert phc2sys process status is UP on node %s", nodeInfo.Name)

			By(fmt.Sprintf("collecting daemon logs from node %s for %s", nodeInfo.Name, RANConfig.PtpStabilityDuration))
			collectionResult, err := daemonlogs.CollectDaemonLogs(
				RANConfig.Spoke1APIClient, nodeInfo.Name, RANConfig.PtpStabilityDuration)
			Expect(err).ToNot(HaveOccurred(), "Failed to collect daemon logs on node %s", nodeInfo.Name)

			DeferCleanup(os.Remove, collectionResult.TempFilePath)

			By("asserting that we collected more log lines than errors on node " + nodeInfo.Name)
			Expect(collectionResult.CollectedLineCount).To(BeNumerically(">", len(collectionResult.Errors)),
				"collected fewer log lines (%d) than fetch errors (%d); log collection is unreliable",
				collectionResult.CollectedLineCount, len(collectionResult.Errors))

			By("analyzing collected daemon logs for node " + nodeInfo.Name)

			analysisResult, err := stability.AnalyzeFromFile(
				collectionResult.TempFilePath, RANConfig.PtpStabilityThreshold)
			Expect(err).ToNot(HaveOccurred(), "Failed to analyze daemon logs for node %s", nodeInfo.Name)

			AddReportEntry("ptp_stability_analysis_"+nodeInfo.Name, analysisResult.DiagnosticMessage())

			Expect(analysisResult.Passed).To(BeTrue(), analysisResult.DiagnosticMessage())

			// Clock class is only meaningful for the profile(s) that actually publish/announce it downstream (BC,
			// GM, MultiNICGM, TBCTransmitter). A receiver-only leg (e.g. TBCReceiver) computes its own independent
			// clock class series and is not the thing this assertion is about; including it would make the check
			// depend on an unrelated ptp4l instance. Class 6 itself is not exclusive to T-BC - GM and any BC
			// forwarding a class-6 upstream grandmaster will show it too.
			publisherProfiles := nodeInfo.GetProfilesByTypes(
				profiles.ProfileTypeGM,
				profiles.ProfileTypeMultiNICGM,
				profiles.ProfileTypeBC,
				profiles.ProfileTypeTBCTransmitter,
			)

			for _, publisherProfile := range publisherProfiles {
				Expect(publisherProfile.ConfigIndex).ToNot(BeNil(),
					"Publisher profile %s on node %s has no ConfigIndex set",
					publisherProfile.Reference.ProfileName, nodeInfo.Name)

				configName := fmt.Sprintf("ptp4l.%d.config", *publisherProfile.ConfigIndex)

				By(fmt.Sprintf("asserting clock class remained stable on node %s config %s", nodeInfo.Name, configName))

				clockClassQuery := metrics.ClockClassQuery{
					Node:    metrics.Equals(nodeInfo.Name),
					Process: metrics.Equals(metrics.ProcessPTP4L),
					Config:  metrics.Equals(configName),
				}
				err = metrics.AssertClockClassStable(context.TODO(), prometheusAPI, clockClassQuery,
					metrics.ClockClass6, collectionResult.StartedAt, RANConfig.PtpStabilityDuration, time.Second)
				Expect(err).ToNot(HaveOccurred(), "Clock class deviated on node %s config %s", nodeInfo.Name, configName)
			}

			// Unlike the clock class metric above, this check stays node-wide rather than scoped to the publisher
			// profile's config: event resource addresses are aggregated per NIC group (e.g. ens2fx), and a receiver
			// leg can share a physical NIC with the publisher leg, so OnInterface/ContainingResource cannot reliably
			// separate them without also dropping legitimate publisher events on the shared NIC.
			By("asserting only LOCKED event on node " + nodeInfo.Name)

			eventPod, err := consumer.GetConsumerPodforNode(RANConfig.Spoke1APIClient, nodeInfo.Name)
			Expect(err).ToNot(HaveOccurred(), "Failed to get event pod for node %s", nodeInfo.Name)

			disqualifyingFilter := events.Any(
				events.IsType(eventptp.PtpClockClassChange),
				events.All(
					events.IsType(eventptp.PtpStateChange),
					events.Not(events.HasValue(events.WithSyncState(eventptp.LOCKED))),
				),
			)
			err = events.WaitForEvent(eventPod, collectionResult.StartedAt, 30*time.Second, disqualifyingFilter,
				events.WithoutCurrentState(true))
			Expect(err).To(HaveOccurred(),
				"Expected no clock-class-change or non-LOCKED state-change event on node %s, but one was received",
				nodeInfo.Name)
		}

		if !testRanAtLeastOnce {
			Skip("Could not find any PTP-capable node for stability test")
		}
	})
})
