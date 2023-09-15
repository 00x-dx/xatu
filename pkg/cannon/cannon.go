package cannon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	//nolint:gosec // only exposed if pprofAddr config is set
	_ "net/http/pprof"

	"github.com/beevik/ntp"
	"github.com/ethpandaops/xatu/pkg/cannon/coordinator"
	"github.com/ethpandaops/xatu/pkg/cannon/deriver"
	v2 "github.com/ethpandaops/xatu/pkg/cannon/deriver/beacon/eth/v2"
	"github.com/ethpandaops/xatu/pkg/cannon/ethereum"
	"github.com/ethpandaops/xatu/pkg/cannon/iterator"
	"github.com/ethpandaops/xatu/pkg/output"
	"github.com/ethpandaops/xatu/pkg/proto/xatu"
	"github.com/go-co-op/gocron"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

type Cannon struct {
	Config *Config

	sinks []output.Sink

	beacon *ethereum.BeaconNode

	clockDrift time.Duration

	log logrus.FieldLogger

	id uuid.UUID

	metrics *Metrics

	scheduler *gocron.Scheduler

	eventDerivers []deriver.EventDeriver

	coordinatorClient *coordinator.Client
}

func New(ctx context.Context, log logrus.FieldLogger, config *Config) (*Cannon, error) {
	if config == nil {
		return nil, errors.New("config is required")
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	sinks, err := config.CreateSinks(log)
	if err != nil {
		return nil, err
	}

	beacon, err := ethereum.NewBeaconNode(ctx, config.Name, &config.Ethereum, log)
	if err != nil {
		return nil, err
	}

	coordinatorClient, err := coordinator.New(&config.Coordinator, log)
	if err != nil {
		return nil, err
	}

	return &Cannon{
		Config:            config,
		sinks:             sinks,
		beacon:            beacon,
		clockDrift:        time.Duration(0),
		log:               log,
		id:                uuid.New(),
		metrics:           NewMetrics("xatu_cannon"),
		scheduler:         gocron.NewScheduler(time.Local),
		eventDerivers:     nil, // Derivers are created once the beacon node is ready
		coordinatorClient: coordinatorClient,
	}, nil
}

func (c *Cannon) Start(ctx context.Context) error {
	if err := c.ServeMetrics(ctx); err != nil {
		return err
	}

	if c.Config.PProfAddr != nil {
		if err := c.ServePProf(ctx); err != nil {
			return err
		}
	}

	if err := c.startBeaconBlockProcessor(ctx); err != nil {
		return err
	}

	c.log.
		WithField("version", xatu.Full()).
		WithField("id", c.id.String()).
		Info("Starting Xatu in cannon mode 💣")

	if err := c.startCrons(ctx); err != nil {
		c.log.WithError(err).Fatal("Failed to start crons")
	}

	for _, sink := range c.sinks {
		if err := sink.Start(ctx); err != nil {
			return err
		}
	}

	if c.Config.Ethereum.OverrideNetworkName != "" {
		c.log.WithField("network", c.Config.Ethereum.OverrideNetworkName).Info("Overriding network name")
	}

	if err := c.beacon.Start(ctx); err != nil {
		return err
	}

	cancel := make(chan os.Signal, 1)
	signal.Notify(cancel, syscall.SIGTERM, syscall.SIGINT)

	sig := <-cancel
	c.log.Printf("Caught signal: %v", sig)

	c.log.Printf("Flushing sinks")

	for _, sink := range c.sinks {
		if err := sink.Stop(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (c *Cannon) ServeMetrics(ctx context.Context) error {
	go func() {
		sm := http.NewServeMux()
		sm.Handle("/metrics", promhttp.Handler())

		server := &http.Server{
			Addr:              c.Config.MetricsAddr,
			ReadHeaderTimeout: 15 * time.Second,
			Handler:           sm,
		}

		c.log.Infof("Serving metrics at %s", c.Config.MetricsAddr)

		if err := server.ListenAndServe(); err != nil {
			c.log.Fatal(err)
		}
	}()

	return nil
}

func (c *Cannon) ServePProf(ctx context.Context) error {
	pprofServer := &http.Server{
		Addr:              *c.Config.PProfAddr,
		ReadHeaderTimeout: 120 * time.Second,
	}

	go func() {
		c.log.Infof("Serving pprof at %s", *c.Config.PProfAddr)

		if err := pprofServer.ListenAndServe(); err != nil {
			c.log.Fatal(err)
		}
	}()

	return nil
}

func (c *Cannon) createNewClientMeta(ctx context.Context) (*xatu.ClientMeta, error) {
	var networkMeta *xatu.ClientMeta_Ethereum_Network

	network := c.beacon.Metadata().Network
	if network != nil {
		networkMeta = &xatu.ClientMeta_Ethereum_Network{
			Name: string(network.Name),
			Id:   network.ID,
		}

		if c.Config.Ethereum.OverrideNetworkName != "" {
			networkMeta.Name = c.Config.Ethereum.OverrideNetworkName
		}
	}

	return &xatu.ClientMeta{
		Name:           c.Config.Name,
		Version:        xatu.Short(),
		Id:             c.id.String(),
		Implementation: xatu.Implementation,
		Os:             runtime.GOOS,
		ClockDrift:     uint64(c.clockDrift.Milliseconds()),
		Ethereum: &xatu.ClientMeta_Ethereum{
			Network:   networkMeta,
			Execution: &xatu.ClientMeta_Ethereum_Execution{},
			Consensus: &xatu.ClientMeta_Ethereum_Consensus{
				Implementation: c.beacon.Metadata().Client(ctx),
				Version:        c.beacon.Metadata().NodeVersion(ctx),
			},
		},
		Labels: c.Config.Labels,
	}, nil
}

func (c *Cannon) startCrons(ctx context.Context) error {
	if _, err := c.scheduler.Every("5m").Do(func() {
		if err := c.syncClockDrift(ctx); err != nil {
			c.log.WithError(err).Error("Failed to sync clock drift")
		}
	}); err != nil {
		return err
	}

	c.scheduler.StartAsync()

	return nil
}

func (c *Cannon) syncClockDrift(ctx context.Context) error {
	response, err := ntp.Query(c.Config.NTPServer)
	if err != nil {
		return err
	}

	err = response.Validate()
	if err != nil {
		return err
	}

	c.clockDrift = response.ClockOffset
	c.log.WithField("drift", c.clockDrift).Info("Updated clock drift")

	return err
}

func (c *Cannon) handleNewDecoratedEvents(ctx context.Context, events []*xatu.DecoratedEvent) error {
	for _, sink := range c.sinks {
		if err := sink.HandleNewDecoratedEvents(ctx, events); err != nil {
			c.log.
				WithError(err).
				WithField("sink", sink.Type()).
				WithField("events", len(events)).
				Error("Failed to send events to sink")
		}
	}

	for _, event := range events {
		c.metrics.AddDecoratedEvent(1, event, string(c.beacon.Metadata().Network.Name))
	}

	return nil
}

func (c *Cannon) startBeaconBlockProcessor(ctx context.Context) error {
	c.beacon.OnReady(ctx, func(ctx context.Context) error {
		c.log.Info("Internal beacon node is ready, firing up event derivers")

		networkName := string(c.beacon.Metadata().Network.Name)
		networkID := fmt.Sprintf("%d", c.beacon.Metadata().Network.ID)

		wallclock := c.beacon.Metadata().Wallclock()

		clientMeta, err := c.createNewClientMeta(ctx)
		if err != nil {
			return err
		}

		checkpointIteratorMetrics := iterator.NewCheckpointMetrics("xatu_cannon")

		finalizedCheckpoint := "finalized"

		eventDerivers := []deriver.EventDeriver{
			v2.NewAttesterSlashingDeriver(
				c.log,
				&c.Config.Derivers.AttesterSlashingConfig,
				iterator.NewCheckpointIterator(
					c.log,
					networkName,
					networkID,
					xatu.CannonType_BEACON_API_ETH_V2_BEACON_BLOCK_ATTESTER_SLASHING,
					c.coordinatorClient,
					wallclock,
					&checkpointIteratorMetrics,
					c.beacon,
					finalizedCheckpoint,
				),
				c.beacon,
				clientMeta,
			),
			v2.NewProposerSlashingDeriver(
				c.log,
				&c.Config.Derivers.ProposerSlashingConfig,
				iterator.NewCheckpointIterator(
					c.log,
					networkName,
					networkID,
					xatu.CannonType_BEACON_API_ETH_V2_BEACON_BLOCK_PROPOSER_SLASHING,
					c.coordinatorClient,
					wallclock,
					&checkpointIteratorMetrics,
					c.beacon,
					finalizedCheckpoint,
				),
				c.beacon,
				clientMeta,
			),
			v2.NewVoluntaryExitDeriver(
				c.log,
				&c.Config.Derivers.VoluntaryExitConfig,
				iterator.NewCheckpointIterator(
					c.log,
					networkName,
					networkID,
					xatu.CannonType_BEACON_API_ETH_V2_BEACON_BLOCK_VOLUNTARY_EXIT,
					c.coordinatorClient,
					wallclock,
					&checkpointIteratorMetrics,
					c.beacon,
					finalizedCheckpoint,
				),
				c.beacon,
				clientMeta,
			),
			v2.NewDepositDeriver(
				c.log,
				&c.Config.Derivers.DepositConfig,
				iterator.NewCheckpointIterator(
					c.log,
					networkName,
					networkID,
					xatu.CannonType_BEACON_API_ETH_V2_BEACON_BLOCK_DEPOSIT,
					c.coordinatorClient,
					wallclock,
					&checkpointIteratorMetrics,
					c.beacon,
					finalizedCheckpoint,
				),
				c.beacon,
				clientMeta,
			),
			v2.NewBLSToExecutionChangeDeriver(
				c.log,
				&c.Config.Derivers.BLSToExecutionConfig,
				iterator.NewCheckpointIterator(
					c.log,
					networkName,
					networkID,
					xatu.CannonType_BEACON_API_ETH_V2_BEACON_BLOCK_BLS_TO_EXECUTION_CHANGE,
					c.coordinatorClient,
					wallclock,
					&checkpointIteratorMetrics,
					c.beacon,
					finalizedCheckpoint,
				),
				c.beacon,
				clientMeta,
			),
			v2.NewExecutionTransactionDeriver(
				c.log,
				&c.Config.Derivers.ExecutionTransactionConfig,
				iterator.NewCheckpointIterator(
					c.log,
					networkName,
					networkID,
					xatu.CannonType_BEACON_API_ETH_V2_BEACON_BLOCK_EXECUTION_TRANSACTION,
					c.coordinatorClient,
					wallclock,
					&checkpointIteratorMetrics,
					c.beacon,
					finalizedCheckpoint,
				),
				c.beacon,
				clientMeta,
			),
		}

		c.eventDerivers = eventDerivers

		for _, deriver := range c.eventDerivers {
			d := deriver

			d.OnEventsDerived(ctx, func(ctx context.Context, events []*xatu.DecoratedEvent) error {
				return c.handleNewDecoratedEvents(ctx, events)
			})

			d.OnLocationUpdated(ctx, func(ctx context.Context, location uint64) error {
				c.metrics.SetDeriverLocation(location, d.CannonType(), string(c.beacon.Metadata().Network.Name))

				return nil
			})

			c.log.
				WithField("deriver", deriver.Name()).
				WithField("type", deriver.CannonType()).
				Info("Starting cannon event deriver")

			if err := deriver.Start(ctx); err != nil {
				return err
			}
		}

		return nil
	})

	return nil
}