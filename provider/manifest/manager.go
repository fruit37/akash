package manifest

import (
	"context"
	"fmt"
	"time"

	"github.com/tendermint/tendermint/libs/log"

	lifecycle "github.com/boz/go-lifecycle"
	mutil "github.com/ovrclk/akash/manifest"
	"github.com/ovrclk/akash/provider/event"
	"github.com/ovrclk/akash/provider/session"
	"github.com/ovrclk/akash/types"
	"github.com/ovrclk/akash/types/base"
	"github.com/ovrclk/akash/util/runner"
	"github.com/ovrclk/akash/validation"
)

func newManager(h *handler, daddr base.Bytes) (*manager, error) {
	session := h.session.ForModule("manifest-manager")

	sub, err := h.sub.Clone()
	if err != nil {
		return nil, err
	}

	m := &manager{
		config:     h.config,
		daddr:      daddr,
		session:    session,
		bus:        h.bus,
		sub:        sub,
		leasech:    make(chan event.LeaseWon),
		rmleasech:  make(chan types.LeaseID),
		manifestch: make(chan manifestRequest),
		updatech:   make(chan base.Bytes),
		log:        session.Log().With("deployment", daddr),
		lc:         lifecycle.New(),
	}

	go m.lc.WatchChannel(h.lc.ShuttingDown())
	go m.run(h.managerch)

	return m, nil
}

type managerChainData struct {
	deployment *types.Deployment
	dgroups    []*types.DeploymentGroup
}

type manager struct {
	config  config
	daddr   base.Bytes
	session session.Session
	bus     event.Bus
	sub     event.Subscriber

	leasech    chan event.LeaseWon
	rmleasech  chan types.LeaseID
	manifestch chan manifestRequest
	updatech   chan base.Bytes

	data      *managerChainData
	requests  []manifestRequest
	leases    []event.LeaseWon
	manifests []*types.Manifest
	versions  []base.Bytes

	stoptimer *time.Timer

	log log.Logger
	lc  lifecycle.Lifecycle
}

func (m *manager) stop() {
	m.lc.ShutdownAsync(nil)
}

func (m *manager) handleLease(ev event.LeaseWon) {
	select {
	case m.leasech <- ev:
	case <-m.lc.ShuttingDown():
		m.log.Error("not running: handle manifest", "lease", ev.LeaseID)
	}
}

func (m *manager) removeLease(id types.LeaseID) {
	select {
	case m.rmleasech <- id:
	case <-m.lc.ShuttingDown():
		m.log.Error("not running: remove lease", "lease", id)
	}
}

func (m *manager) handleManifest(req manifestRequest) {
	select {
	case m.manifestch <- req:
	case <-m.lc.ShuttingDown():
		m.log.Error("not running: handle manifest")
		req.ch <- ErrNotRunning
	}
}

func (m *manager) handleUpdate(version base.Bytes) {
	select {
	case m.updatech <- version:
	case <-m.lc.ShuttingDown():
		m.log.Error("not running: version update", "version", version)
	}
}

func (m *manager) run(donech chan<- *manager) {
	defer m.lc.ShutdownCompleted()
	defer func() { donech <- m }()

	var runch <-chan runner.Result

	ctx, cancel := context.WithCancel(context.Background())

loop:
	for {

		var stopch <-chan time.Time
		if m.stoptimer != nil {
			stopch = m.stoptimer.C
		}

		select {

		case err := <-m.lc.ShutdownRequest():
			m.lc.ShutdownInitiated(err)
			break loop

		case <-stopch:
			m.log.Error("shutdown timer expired")
			m.lc.ShutdownInitiated(fmt.Errorf("shutdown timer expired"))
			break loop

		case ev := <-m.leasech:
			m.log.Info("new lease", "lease", ev.LeaseID)

			m.leases = append(m.leases, ev)
			m.emitReceivedEvents()
			m.maybeScheduleStop()
			runch = m.maybeFetchData(ctx, runch)

		case id := <-m.rmleasech:
			m.log.Info("lease removed", "lease", id)

			for idx, lease := range m.leases {
				if id.Equal(lease.LeaseID) {
					m.leases = append(m.leases[:idx], m.leases[idx+1:]...)
				}
			}

			m.maybeScheduleStop()

		case req := <-m.manifestch:
			m.log.Info("manifest received")

			// TODO: fail fast if invalid request to prevent DoS

			m.requests = append(m.requests, req)
			m.validateRequests()
			m.emitReceivedEvents()
			m.maybeScheduleStop()
			runch = m.maybeFetchData(ctx, runch)

		case version := <-m.updatech:
			m.log.Info("received version", "version", version)

			m.versions = append(m.versions, version)
			if m.data != nil {
				m.data.deployment.Version = version
			}

		case result := <-runch:
			runch = nil

			if err := result.Error(); err != nil {
				m.log.Error("error fetching data", "err", err)
				break
			}

			m.data = result.Value().(*managerChainData)

			m.log.Info("data received", "version", m.data.deployment.Version)

			m.validateRequests()
			m.emitReceivedEvents()
			m.maybeScheduleStop()

		}
	}

	cancel()

	for _, req := range m.requests {
		req.ch <- ErrNotRunning
	}

	if m.stoptimer != nil {
		if m.stoptimer.Stop() {
			<-m.stoptimer.C
		}
	}

	if runch != nil {
		<-runch
	}

}

func (m *manager) maybeFetchData(ctx context.Context, runch <-chan runner.Result) <-chan runner.Result {
	if m.data == nil && runch == nil {
		return m.fetchData(ctx)
	}
	return runch
}

func (m *manager) fetchData(ctx context.Context) <-chan runner.Result {
	return runner.Do(func() runner.Result {
		// TODO: retry
		return runner.NewResult(m.doFetchData(ctx))
	})
}

func (m *manager) doFetchData(ctx context.Context) (*managerChainData, error) {
	deployment, err := m.session.Query().Deployment(ctx, m.daddr)
	if err != nil {
		return nil, err
	}

	dgroups, err := m.session.Query().DeploymentGroupsForDeployment(ctx, m.daddr)
	if err != nil {
		return nil, err
	}

	return &managerChainData{
		deployment: deployment,
		dgroups:    dgroups.Items,
	}, nil
}

func (m *manager) maybeScheduleStop() bool {
	if len(m.leases) > 0 || len(m.manifests) > 0 {
		if m.stoptimer != nil {
			m.log.Info("stopping stop timer")
			if m.stoptimer.Stop() {
				<-m.stoptimer.C
			}
			m.stoptimer = nil
		}
		return false
	}
	if m.stoptimer != nil {
		m.log.Info("starting stop timer", "duration", m.config.ManifestLingerDuration)
		m.stoptimer = time.NewTimer(m.config.ManifestLingerDuration)
	}
	return true
}

func (m *manager) emitReceivedEvents() {
	if m.data == nil || len(m.leases) == 0 || len(m.manifests) == 0 {
		return
	}

	manifest := m.manifests[len(m.manifests)-1]

	m.log.Debug("publishing manifest received", "num-leases", len(m.leases))

	for _, lease := range m.leases {
		if err := m.bus.Publish(event.ManifestReceived{
			LeaseID:    lease.LeaseID,
			Group:      lease.Group,
			Manifest:   manifest,
			Deployment: m.data.deployment,
		}); err != nil {
			m.log.Error("publishing event", "err", err, "lease", lease.LeaseID)
		}
	}
}

func (m *manager) validateRequests() {
	if m.data == nil || len(m.requests) == 0 {
		return
	}

	var manifests []*types.Manifest

	for _, req := range m.requests {
		if err := m.validateRequest(req); err != nil {
			m.log.Error("invalid manifest", "err", err)
			req.ch <- err
			continue
		}
		manifests = append(manifests, req.value.Manifest)
		req.ch <- nil
	}
	m.requests = nil

	m.log.Debug("requests valid", "num-requests", len(manifests))

	if len(manifests) > 0 {
		// XXX: only one version means only one valid manifest
		m.manifests = append(m.manifests, manifests[0])
	}
}

func (m *manager) validateRequest(req manifestRequest) error {
	if err := validation.ValidateManifestWithDeployment(req.value.Manifest, m.data.dgroups); err != nil {
		return err
	}
	return mutil.VerifyRequest(req.value, m.data.deployment)
}
