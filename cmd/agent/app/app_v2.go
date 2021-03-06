package app

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"time"

	gendiodes "code.cloudfoundry.org/go-diodes"
	"code.cloudfoundry.org/go-loggregator/pulseemitter"
	"code.cloudfoundry.org/loggregator-agent/pkg/clientpool"
	clientpoolv2 "code.cloudfoundry.org/loggregator-agent/pkg/clientpool/v2"
	"code.cloudfoundry.org/loggregator-agent/pkg/diodes"
	egress "code.cloudfoundry.org/loggregator-agent/pkg/egress/v2"
	"code.cloudfoundry.org/loggregator-agent/pkg/healthendpoint"
	ingress "code.cloudfoundry.org/loggregator-agent/pkg/ingress/v2"
	"code.cloudfoundry.org/loggregator-agent/pkg/plumbing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// MetricClient creates new CounterMetrics to be emitted periodically.
type MetricClient interface {
	NewCounterMetric(name string, opts ...pulseemitter.MetricOption) pulseemitter.CounterMetric
	NewGaugeMetric(name, unit string, opts ...pulseemitter.MetricOption) pulseemitter.GaugeMetric
}

// AppV2Option configures AppV2 options.
type AppV2Option func(*AppV2)

// WithV2Lookup allows the default DNS resolver to be changed.
func WithV2Lookup(l func(string) ([]net.IP, error)) func(*AppV2) {
	return func(a *AppV2) {
		a.lookup = l
	}
}

type AppV2 struct {
	config          *Config
	healthRegistrar *healthendpoint.Registrar
	clientCreds     credentials.TransportCredentials
	serverCreds     credentials.TransportCredentials
	metricClient    MetricClient
	lookup          func(string) ([]net.IP, error)
}

func NewV2App(
	c *Config,
	r *healthendpoint.Registrar,
	clientCreds credentials.TransportCredentials,
	serverCreds credentials.TransportCredentials,
	metricClient MetricClient,
	opts ...AppV2Option,
) *AppV2 {
	a := &AppV2{
		config:          c,
		healthRegistrar: r,
		clientCreds:     clientCreds,
		serverCreds:     serverCreds,
		metricClient:    metricClient,
		lookup:          net.LookupIP,
	}

	for _, o := range opts {
		o(a)
	}

	return a
}

func (a *AppV2) Start() {
	if a.serverCreds == nil {
		log.Panic("Failed to load TLS server config")
	}

	droppedMetric := a.metricClient.NewCounterMetric("dropped",
		pulseemitter.WithVersion(2, 0),
		pulseemitter.WithTags(map[string]string{"direction": "ingress"}),
	)

	envelopeBuffer := diodes.NewManyToOneEnvelopeV2(10000, gendiodes.AlertFunc(func(missed int) {
		// metric-documentation-v2: (loggregator.metron.dropped) Number of v2 envelopes
		// dropped from the agent ingress diode
		droppedMetric.Increment(uint64(missed))

		log.Printf("Dropped %d v2 envelopes", missed)
	}))

	pool := a.initializePool()
	counterAggr := egress.NewCounterAggregator(pool)
	tx := egress.NewTransponder(
		envelopeBuffer,
		counterAggr,
		a.config.Tags,
		100, 100*time.Millisecond,
		a.metricClient,
	)
	go tx.Start()

	agentAddress := fmt.Sprintf("127.0.0.1:%d", a.config.GRPC.Port)
	log.Printf("agent v2 API started on addr %s", agentAddress)

	rx := ingress.NewReceiver(envelopeBuffer, a.metricClient, a.healthRegistrar)
	kp := keepalive.EnforcementPolicy{
		MinTime:             10 * time.Second,
		PermitWithoutStream: true,
	}
	ingressServer := ingress.NewServer(
		agentAddress,
		rx,
		grpc.Creds(a.serverCreds),
		grpc.KeepaliveEnforcementPolicy(kp),
	)
	ingressServer.Start()
}

func (a *AppV2) initializePool() *clientpoolv2.ClientPool {
	if a.clientCreds == nil {
		log.Panic("Failed to load TLS client config")
	}

	balancers := make([]*clientpoolv2.Balancer, 0, 2)
	if a.config.RouterAddrWithAZ != "" {
		balancers = append(balancers, clientpoolv2.NewBalancer(
			a.config.RouterAddrWithAZ,
			clientpoolv2.WithLookup(a.lookup)),
		)
	}
	balancers = append(balancers, clientpoolv2.NewBalancer(
		a.config.RouterAddr,
		clientpoolv2.WithLookup(a.lookup)),
	)

	avgEnvelopeSize := a.metricClient.NewGaugeMetric("average_envelope", "bytes/minute",
		pulseemitter.WithVersion(2, 0),
		pulseemitter.WithTags(map[string]string{
			"loggregator": "v2",
		}))
	tracker := plumbing.NewEnvelopeAverager()
	tracker.Start(60*time.Second, func(average float64) {
		avgEnvelopeSize.Set(average)
	})
	statsHandler := clientpool.NewStatsHandler(tracker)

	kp := keepalive.ClientParameters{
		Time:                15 * time.Second,
		Timeout:             15 * time.Second,
		PermitWithoutStream: true,
	}
	fetcher := clientpoolv2.NewSenderFetcher(
		a.healthRegistrar,
		grpc.WithTransportCredentials(a.clientCreds),
		grpc.WithStatsHandler(statsHandler),
		grpc.WithKeepaliveParams(kp),
	)

	connector := clientpoolv2.MakeGRPCConnector(fetcher, balancers)

	var connManagers []clientpoolv2.Conn
	for i := 0; i < 5; i++ {
		connManagers = append(connManagers, clientpoolv2.NewConnManager(
			connector,
			100000+rand.Int63n(1000),
			time.Second,
		))
	}

	return clientpoolv2.New(connManagers...)
}
